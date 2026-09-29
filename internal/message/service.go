package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/outbox"
)

// Friend is the port message uses to authorize direct-message send (R3).
type Friend interface {
	CanSendMessage(ctx context.Context, from, to int64) (bool, string, error)
}

// Group is the port message uses for group send/read rules (R4/R15).
type Group interface {
	CanSend(ctx context.Context, groupID, userID int64) error
	IsModerator(ctx context.Context, groupID, userID int64) (bool, error)
	GroupByConversation(ctx context.Context, conversationID int64) (GroupRowView, error)
	WasMemberAt(ctx context.Context, groupID, userID int64, at time.Time) (bool, error)
	ListGroupConversationsForUser(ctx context.Context, userID int64) ([]GroupConvView, error)
}

// GroupRowView is the subset of group state message needs (no cross-import).
type GroupRowView struct {
	GroupID        int64
	Name           string
	Status         string
	ConversationID int64
}

// GroupConvView mirrors group.GroupConvView for the A3 port.
type GroupConvView struct {
	ConversationID int64
	GroupID        int64
	Name           string
}

// Media is the port message uses to verify media objects are sender-owned and ready.
type Media interface {
	// AssertReady verifies object is owned by ownerID, status=ready, mime-prefixed by kind.
	AssertReady(ctx context.Context, ownerID, objectID int64, kind string) error
	// OnReferencesRemoved is invoked by the retention worker when a message
	// referencing these objects expires. V1: GC hook (SPEC-03 R13).
	OnReferencesRemoved(ctx context.Context, objectIDs []int64) error
}

// DirectLookup resolves the direct partner for a conversation row (R3/R19).
type DirectLookup interface {
	IsMember(ctx context.Context, conversationID, userID int64) (bool, error)
}

// Service owns message rows, conversation_seq, references, forwards, pins,
// conversation settings and the 180-day retention worker (SPEC-04).
type Service struct {
	db    *sql.DB
	friend Friend
	group Group
	media Media
	conv  DirectLookup
	now   func() time.Time
}

// New wires the message service.
func New(db *sql.DB, f Friend, g Group, m Media, c DirectLookup, now func() time.Time) *Service {
	return &Service{db: db, friend: f, group: g, media: m, conv: c, now: now}
}

// ---------------------------------------------------------------------------
// Send (R1/R2/R3/R4/R5/R6/R7/R8/R10/R11).

// SendInput is POST /conversations/{id}/messages.
type SendInput struct {
	ClientMsgID      string
	Type             string
	Payload          json.RawMessage
	RefMessageID     int64
	ForwardSourceID  int64
}

// SendResult is the A1 response (R8).
type SendResult struct {
	MessageID       int64
	ConversationID  int64
	ConversationSeq uint64
	Status          string
	CreatedAt       time.Time
}

// Send is the two-phase send: lock-free pre-check then conversation-row lock (R2).
func (s *Service) Send(ctx context.Context, senderID, conversationID int64, in SendInput) (SendResult, error) {
	if in.ClientMsgID == "" {
		return SendResult{}, apperrors.Invalid("client_msg_id required")
	}
	if in.Type == TypeSystem {
		return SendResult{}, apperrors.Invalid("clients may not send system messages")
	}
	payload, err := decodePayload(in.Payload)
	if err != nil {
		return SendResult{}, err
	}
	if err := validatePayload(in.Type, payload); err != nil {
		return SendResult{}, err
	}

	// Phase 1: lock-free pre-check (R2 step 1). Idempotency pre-check hits and returns early.
	if existing, err := findByClientMsg(ctx, s.db, senderID, in.ClientMsgID); err != nil {
		return SendResult{}, err
	} else if existing.ID != 0 {
		return SendResult{
			MessageID: existing.ID, ConversationID: existing.ConversationID,
			ConversationSeq: existing.ConversationSeq, Status: existing.Status, CreatedAt: existing.CreatedAt,
		}, nil
	}

	convType, groupID, err := s.precheckSend(ctx, senderID, conversationID, in.Type, payload)
	if err != nil {
		return SendResult{}, err
	}

	// Reference snapshot (R10): validated in precheck; digest is computed before lock.
	var ref *ReferenceRow
	if in.RefMessageID != 0 {
		ref, err = s.snapshotReference(ctx, senderID, conversationID, in.RefMessageID)
		if err != nil {
			return SendResult{}, err
		}
	}

	var out SendResult
	txErr := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		now := s.now()
		lastSeq, lockedType, err := lockConversation(ctx, tx, conversationID)
		if err != nil {
			return err
		}
		if lockedType != convType {
			return apperrors.Unavail("conversation type changed")
		}
		seq := lastSeq + 1
		row := MessageRow{
			ConversationID: conversationID, ConversationSeq: seq,
			SenderID: senderID, SenderType: SenderUser, ClientMsgID: in.ClientMsgID,
			Type: in.Type, Payload: in.Payload, Status: StatusStored,
			CreatedAt: now, ExpiresAt: expiryAt(now),
		}
		msgID, err := insertMessage(ctx, tx, &row)
		if err != nil {
			if mysqlx.IsDuplicate(err, "uk_messages_client") {
				existing, qerr := findByClientMsg(ctx, tx, senderID, in.ClientMsgID)
				if qerr == nil && existing.ID != 0 {
					out = SendResult{
						MessageID: existing.ID, ConversationID: existing.ConversationID,
						ConversationSeq: existing.ConversationSeq, Status: existing.Status, CreatedAt: existing.CreatedAt,
					}
					return nil
				}
			}
			return err
		}
		if err := bumpConversationSeq(ctx, tx, conversationID, seq); err != nil {
			return err
		}
		if kind := assetKind(in.Type); kind != "" {
			oid, err := strconv.ParseInt(payload.MediaObjectID, 10, 64)
			if err != nil {
				return apperrors.Invalid("media_object_id must be an integer id")
			}
			if err := insertAsset(ctx, tx, msgID, oid, kind, now); err != nil {
				return err
			}
		}
		if ref != nil {
			if err := insertReference(ctx, tx, msgID, ref.RefMsgID, ref.RefSenderID, ref.RefType, ref.RefDigest, now); err != nil {
				return err
			}
		}
		if in.ForwardSourceID != 0 {
			if err := insertForward(ctx, tx, in.ForwardSourceID, msgID, senderID, now); err != nil {
				return err
			}
		}
		if err := outbox.Emit(ctx, tx, outbox.Event{
			Type: "message.stored", AggregateID: strconv.FormatInt(msgID, 10), Queue: mq.QueueMessagePush,
		}); err != nil {
			return err
		}
		out = SendResult{MessageID: msgID, ConversationID: conversationID, ConversationSeq: seq, Status: StatusStored, CreatedAt: now}
		_ = groupID
		return nil
	})
	if txErr != nil {
		return SendResult{}, txErr
	}
	return out, nil
}

// precheckSend runs all R3-R6 authorization rules outside the conversation lock.
func (s *Service) precheckSend(ctx context.Context, senderID, conversationID int64, msgType string, p *Payload) (convType string, groupID int64, err error) {
	rowType, err := conversationType(ctx, s.db, conversationID)
	if err != nil {
		return "", 0, err
	}
	switch rowType {
	case "direct":
		partner, err := otherDirectMember(ctx, s.db, conversationID, senderID)
		if err != nil {
			return "", 0, err
		}
		allowed, reason, err := s.friend.CanSendMessage(ctx, senderID, partner)
		if err != nil {
			return "", 0, err
		}
		if !allowed {
			return "", 0, apperrors.New(apperrors.Forbidden, reason)
		}
	case "group":
		g, err := s.group.GroupByConversation(ctx, conversationID)
		if err != nil {
			return "", 0, err
		}
		if err := s.group.CanSend(ctx, g.GroupID, senderID); err != nil {
			return "", 0, err
		}
		groupID = g.GroupID
	case "transfer":
		owner, err := transferOwner(ctx, s.db, conversationID)
		if err != nil {
			return "", 0, err
		}
		if owner != senderID {
			return "", 0, apperrors.Unavail("transfer conversation is owner-only")
		}
	case "official_service":
		return "", 0, apperrors.New(apperrors.StateConflict, "official_service sending not wired in phase 2")
	default:
		return "", 0, apperrors.Unavail("unknown conversation type")
	}
	// Media object verification (R7).
	if kind := assetKind(msgType); kind != "" {
		oid, perr := strconv.ParseInt(p.MediaObjectID, 10, 64)
		if perr != nil {
			return "", 0, apperrors.Invalid("media_object_id must be an integer id")
		}
		if err := s.media.AssertReady(ctx, senderID, oid, kind); err != nil {
			return "", 0, err
		}
	}
	if msgType == TypeCard {
		targetID, perr := strconv.ParseInt(p.UserID, 10, 64)
		if perr != nil {
			return "", 0, apperrors.Invalid("card user_id must be an integer id")
		}
		allowed, _, err := s.friend.CanSendMessage(ctx, senderID, targetID)
		if err != nil {
			return "", 0, err
		}
		if !allowed {
			return "", 0, apperrors.Invalid("card target must be a current friend")
		}
	}
	return rowType, groupID, nil
}

// snapshotReference loads the referenced message and produces the digest (R10).
func (s *Service) snapshotReference(ctx context.Context, senderID, conversationID, refID int64) (*ReferenceRow, error) {
	refMsg, err := getMessage(ctx, s.db, refID)
	if err != nil {
		return nil, err
	}
	if refMsg.ConversationID != conversationID {
		return nil, apperrors.Invalid("reference must be in the same conversation")
	}
	if refMsg.Status == StatusRecalled || refMsg.Status == StatusExpired {
		return nil, apperrors.New(apperrors.StateConflict, "cannot reference a recalled/expired message")
	}
	p, err := decodePayload(refMsg.Payload)
	if err != nil {
		return nil, err
	}
	return &ReferenceRow{
		RefMsgID: refMsg.ID, RefSenderID: refMsg.SenderID,
		RefType: refMsg.Type, RefDigest: digestOf(refMsg.Type, p),
	}, nil
}

func assetKind(msgType string) string {
	switch msgType {
	case TypeImage:
		return "image"
	case TypeVideo:
		return "video"
	case TypeVoice:
		return "voice"
	case TypeFile:
		return "file"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Recall (R9).

// Recall marks a message recalled (R9).
func (s *Service) Recall(ctx context.Context, senderID, messageID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		ok, err := recallMessage(ctx, tx, messageID, senderID, s.now())
		if err != nil {
			return err
		}
		if !ok {
			return apperrors.New(apperrors.StateConflict, "cannot recall message")
		}
		return outbox.Emit(ctx, tx, outbox.Event{
			Type: "message.recalled", AggregateID: strconv.FormatInt(messageID, 10), Queue: mq.QueueMessagePush,
		})
	})
}

// MarkDelivered flips stored→delivered idempotently (R22).
func (s *Service) MarkDelivered(ctx context.Context, messageID int64) error {
	return markDelivered(ctx, s.db, messageID)
}

// SendSystemTx implements the group.SystemMessenger port (system message in a tx).
func (s *Service) SendSystemTx(ctx context.Context, tx mysqlx.Tx, conversationID int64, event string, detail map[string]any) error {
	now := s.now()
	payload, err := json.Marshal(map[string]any{"event": event, "detail": detail})
	if err != nil {
		return err
	}
	lastSeq, _, err := lockConversation(ctx, tx, conversationID)
	if err != nil {
		return err
	}
	seq := lastSeq + 1
	row := MessageRow{
		ConversationID: conversationID, ConversationSeq: seq,
		SenderID: 0, SenderType: SenderSystem, ClientMsgID: "",
		Type: TypeSystem, Payload: payload, Status: StatusStored,
		CreatedAt: now, ExpiresAt: expiryAt(now),
	}
	msgID, err := insertMessage(ctx, tx, &row)
	if err != nil {
		return err
	}
	if err := bumpConversationSeq(ctx, tx, conversationID, seq); err != nil {
		return err
	}
	return outbox.Emit(ctx, tx, outbox.Event{
		Type: "message.stored", AggregateID: strconv.FormatInt(msgID, 10), Queue: mq.QueueMessagePush,
	})
}

// ---------------------------------------------------------------------------
// Read path (R14/R15/R16/R17).

// MessageView is one element of A2.
type MessageView struct {
	MessageID       int64           `json:"message_id,string"`
	ConversationID  int64           `json:"conversation_id,string"`
	ConversationSeq uint64          `json:"conversation_seq"`
	SenderID        int64           `json:"sender_id,string"`
	SenderType      string          `json:"sender_type"`
	Type            string          `json:"type"`
	Payload         json.RawMessage `json:"payload"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"created_at"`
	RecalledAt      *time.Time      `json:"recalled_at,omitempty"`
	Reference       *RefView        `json:"ref,omitempty"`
	Assets          []AssetView     `json:"assets,omitempty"`
}

// RefView is the reference-reply snapshot.
type RefView struct {
	MessageID int64  `json:"message_id,string"`
	SenderID  int64  `json:"sender_id,string"`
	Type      string `json:"type"`
	Digest    string `json:"digest"`
	Status    string `json:"status"`
}

// AssetView is one message_assets row.
type AssetView struct {
	MediaObjectID int64  `json:"media_object_id,string"`
	Kind          string `json:"kind"`
}

// HistoryInput is A2.
type HistoryInput struct {
	AfterSeq    uint64
	BeforeSeq   uint64
	Limit       int
}

// HistoryResult is A2 response.
type HistoryResult struct {
	Messages  []MessageView `json:"messages"`
	HasMore   bool          `json:"has_more"`
	NextSeq   uint64        `json:"next_cursor"`
}

// History returns messages in (after, before] that user may read (R14/R15/R16).
func (s *Service) History(ctx context.Context, userID, conversationID int64, in HistoryInput) (HistoryResult, error) {
	if in.AfterSeq != 0 && in.BeforeSeq != 0 {
		return HistoryResult{}, apperrors.Invalid("after_seq and before_seq are mutually exclusive")
	}
	limit := normalizeHistoryLimit(in.Limit)

	// R16: if after_seq is older than the oldest live message, cursor is expired.
	if in.AfterSeq != 0 {
		minLive, err := minUsableSeq(ctx, s.db, conversationID, s.now())
		if err != nil {
			return HistoryResult{}, err
		}
		if minLive > 0 && in.AfterSeq < minLive-1 {
			return HistoryResult{}, apperrors.New(apperrors.SyncCursorExpired, "sync cursor expired")
		}
	}

	var rows []MessageRow
	var err error
	switch {
	case in.BeforeSeq != 0:
		rows, err = listMessagesBefore(ctx, s.db, conversationID, in.BeforeSeq, limit+1)
	case in.AfterSeq != 0:
		rows, err = listMessagesAfter(ctx, s.db, conversationID, in.AfterSeq, limit+1)
	default:
		rows, err = listMessagesBefore(ctx, s.db, conversationID, 0, limit+1)
	}
	if err != nil {
		return HistoryResult{}, err
	}

	visible := make([]MessageRow, 0, len(rows))
	for _, r := range rows {
		ok, verr := s.canRead(ctx, userID, conversationID, r)
		if verr != nil {
			return HistoryResult{}, verr
		}
		if ok {
			visible = append(visible, r)
		}
	}
	hasMore := len(visible) > limit
	if hasMore {
		visible = visible[:limit]
	}
	views, err := s.decorate(ctx, visible)
	if err != nil {
		return HistoryResult{}, err
	}
	var nextSeq uint64
	if n := len(views); n > 0 {
		nextSeq = views[n-1].ConversationSeq
	}
	return HistoryResult{Messages: views, HasMore: hasMore, NextSeq: nextSeq}, nil
}

// canRead enforces R15 visibility per message.
func (s *Service) canRead(ctx context.Context, userID, conversationID int64, r MessageRow) (bool, error) {
	if r.Status == StatusExpired {
		return false, nil
	}
	ct, err := conversationType(ctx, s.db, conversationID)
	if err != nil {
		return false, err
	}
	switch ct {
	case "direct", "transfer", "official_service":
		return s.conv.IsMember(ctx, conversationID, userID)
	case "group":
		g, err := s.group.GroupByConversation(ctx, conversationID)
		if err != nil {
			return false, err
		}
		if r.SenderID == userID {
			return true, nil
		}
		return s.group.WasMemberAt(ctx, g.GroupID, userID, r.CreatedAt)
	}
	return false, nil
}

// decorate attaches reference/asset views and masks recalled payloads.
func (s *Service) decorate(ctx context.Context, rows []MessageRow) ([]MessageView, error) {
	out := make([]MessageView, 0, len(rows))
	for _, r := range rows {
		v := MessageView{
			MessageID: r.ID, ConversationID: r.ConversationID, ConversationSeq: r.ConversationSeq,
			SenderID: r.SenderID, SenderType: r.SenderType, Type: r.Type, Status: r.Status,
			CreatedAt: r.CreatedAt, RecalledAt: r.RecalledAt,
		}
		if r.Status != StatusRecalled {
			v.Payload = r.Payload
		}
		if ref, err := loadReference(ctx, s.db, r.ID); err != nil {
			return nil, err
		} else if ref.RefMsgID != 0 {
			refStatus := StatusStored
			if refMsg, gerr := getMessage(ctx, s.db, ref.RefMsgID); gerr == nil {
				refStatus = refMsg.Status
			}
			v.Reference = &RefView{
				MessageID: ref.RefMsgID, SenderID: ref.RefSenderID,
				Type: ref.RefType, Digest: ref.RefDigest, Status: refStatus,
			}
		}
		if assets, err := assetRowsForMessage(ctx, s.db, r.ID); err != nil {
			return nil, err
		} else {
			for _, a := range assets {
				v.Assets = append(v.Assets, AssetView{MediaObjectID: a.MediaObjectID, Kind: a.Kind})
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Read cursor / mark-unread / settings (R13).

// ReadSeq advances last_read_seq (A5, ruling 4).
func (s *Service) ReadSeq(ctx context.Context, userID, conversationID int64, seq uint64) (uint64, error) {
	if seq == 0 {
		return 0, apperrors.Invalid("seq required")
	}
	if _, err := s.conv.IsMember(ctx, conversationID, userID); err != nil {
		return 0, err
	}
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return advanceReadSeq(ctx, tx, conversationID, userID, seq, s.now())
	})
	if err != nil {
		return 0, err
	}
	cur, err := loadSettings(ctx, s.db, conversationID, userID)
	if err != nil {
		return 0, err
	}
	return cur.LastReadSeq, nil
}

// MarkUnread flags the conversation as unread (A6) without touching last_read_seq.
func (s *Service) MarkUnread(ctx context.Context, userID, conversationID int64) error {
	ct, err := conversationType(ctx, s.db, conversationID)
	if err != nil {
		return err
	}
	_ = ct
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var lastSeq uint64
		if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM conversations WHERE id = ?`, conversationID).Scan(&lastSeq); err != nil {
			return err
		}
		return markUnread(ctx, tx, conversationID, userID, lastSeq, s.now())
	})
}

// SettingsPatch is A7.
type SettingsPatch struct {
	Pinned     *bool
	Muted      *bool
	Background *string
}

// UpdateSettings upserts conversation settings (A7).
func (s *Service) UpdateSettings(ctx context.Context, userID, conversationID int64, patch SettingsPatch) error {
	cur, err := loadSettings(ctx, s.db, conversationID, userID)
	if err != nil {
		return err
	}
	if patch.Pinned != nil {
		cur.Pinned = *patch.Pinned
	}
	if patch.Muted != nil {
		cur.Muted = *patch.Muted
	}
	if patch.Background != nil {
		if len(*patch.Background) > 255 {
			return apperrors.Invalid("background key too long")
		}
		cur.Background = *patch.Background
	}
	cur.UserID = userID
	cur.ConversationID = conversationID
	cur.UpdatedAt = s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return upsertSettings(ctx, tx, &cur)
	})
}

// ---------------------------------------------------------------------------
// Pins (R12).

// Pin adds a message to the conversation pin list (A10).
func (s *Service) Pin(ctx context.Context, userID, conversationID, messageID int64) error {
	msg, err := getMessage(ctx, s.db, messageID)
	if err != nil {
		return err
	}
	if msg.ConversationID != conversationID {
		return apperrors.Invalid("message not in conversation")
	}
	ct, err := conversationType(ctx, s.db, conversationID)
	if err != nil {
		return err
	}
	switch ct {
	case "group":
		g, err := s.group.GroupByConversation(ctx, conversationID)
		if err != nil {
			return err
		}
		ok, err := s.group.IsModerator(ctx, g.GroupID, userID)
		if err != nil {
			return err
		}
		if !ok {
			return apperrors.New(apperrors.Forbidden, "only group moderators may pin")
		}
	case "direct":
		if _, err := s.conv.IsMember(ctx, conversationID, userID); err != nil {
			return err
		}
	default:
		return apperrors.Invalid("pins not allowed in this conversation type")
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		n, err := countPins(ctx, tx, conversationID)
		if err != nil {
			return err
		}
		if n >= MaxMessagePins {
			return apperrors.New(apperrors.QuotaExceeded, "pin limit reached")
		}
		_, err = pinMessage(ctx, tx, conversationID, messageID, userID, s.now())
		return err
	})
}

// Unpin removes a pin (A11).
func (s *Service) Unpin(ctx context.Context, conversationID, messageID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return unpinMessage(ctx, tx, conversationID, messageID)
	})
}

// ListPins returns the pinned messages for A12.
func (s *Service) ListPins(ctx context.Context, conversationID int64) ([]MessageView, error) {
	rows, err := listPinnedMessages(ctx, s.db, conversationID)
	if err != nil {
		return nil, err
	}
	out := make([]MessageView, 0, len(rows))
	for _, v := range rows {
		mv := MessageView{
			MessageID: v.MsgID, ConversationID: conversationID, ConversationSeq: v.Seq,
			SenderID: v.SenderID, Type: v.MsgType, Status: v.Status, CreatedAt: v.CreatedAt,
		}
		if v.Status != StatusRecalled {
			mv.Payload = v.Payload
		}
		out = append(out, mv)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Conversation list (R19) + detail (A3/A4).

// ConversationSummary is one element of A3.
type ConversationSummary struct {
	ConversationID int64         `json:"conversation_id,string"`
	Type           string        `json:"type"`
	Title          string        `json:"title"`
	LastPreview    string        `json:"last_preview"`
	LastSeq        uint64        `json:"last_seq"`
	Unread         int           `json:"unread"`
	Pinned         bool          `json:"pinned"`
	Muted          bool          `json:"muted"`
	LastMessageAt   time.Time     `json:"last_message_at"`
}

// ListConversations returns R19 summaries.
func (s *Service) ListConversations(ctx context.Context, userID int64) ([]ConversationSummary, error) {
	directIDs, err := listDirectConversationsForUser(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	groups, err := s.group.ListGroupConversationsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	allIDs := append([]int64{}, directIDs...)
	for _, g := range groups {
		allIDs = append(allIDs, g.ConversationID)
	}
	settings, err := loadSettingsBatch(ctx, s.db, userID, allIDs)
	if err != nil {
		return nil, err
	}
	titleOf := func(convID int64) (string, error) {
		for _, g := range groups {
			if g.ConversationID == convID {
				return g.Name, nil
			}
		}
		partner, err := otherDirectMember(ctx, s.db, convID, userID)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(partner, 10), nil
	}

	out := make([]ConversationSummary, 0, len(allIDs))
	for _, id := range allIDs {
		ct, err := conversationType(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		title, err := titleOf(id)
		if err != nil {
			return nil, err
		}
		last, err := lastLiveMessage(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		st := settings[id]
		unread, err := countUnread(ctx, s.db, id, userID, st.LastReadSeq)
		if err != nil {
			return nil, err
		}
		sum := ConversationSummary{
			ConversationID: id, Type: ct, Title: title,
			LastSeq: last.ConversationSeq, Unread: unread,
			Pinned: st.Pinned, Muted: st.Muted, LastMessageAt: last.CreatedAt,
		}
		if last.ID != 0 {
			sum.LastPreview = previewDigest(last)
		}
		out = append(out, sum)
	}
	return out, nil
}

// previewDigest renders the R19 one-line digest.
func previewDigest(r MessageRow) string {
	switch r.Status {
	case StatusRecalled:
		return "[消息已撤回]"
	case StatusExpired:
		return "[消息已过期]"
	}
	switch r.Type {
	case TypeImage:
		return "[图片]"
	case TypeVideo:
		return "[视频]"
	case TypeVoice:
		return "[语音]"
	case TypeFile:
		return "[文件]"
	case TypeCard:
		return "[名片]"
	case TypeSystem:
		return "[系统消息]"
	}
	p, err := decodePayload(r.Payload)
	if err != nil {
		return ""
	}
	return previewOf(r.Type, p)
}

// ConversationDetail is A4.
type ConversationDetail struct {
	ConversationID int64         `json:"conversation_id,string"`
	Type           string        `json:"type"`
	Settings       SettingsRow   `json:"settings"`
}

// GetConversation returns conversation detail + my settings (A4).
func (s *Service) GetConversation(ctx context.Context, userID, conversationID int64) (ConversationDetail, error) {
	ct, err := conversationType(ctx, s.db, conversationID)
	if err != nil {
		return ConversationDetail{}, err
	}
	if _, err := s.conv.IsMember(ctx, conversationID, userID); err != nil {
		return ConversationDetail{}, err
	}
	st, err := loadSettings(ctx, s.db, conversationID, userID)
	if err != nil {
		return ConversationDetail{}, err
	}
	return ConversationDetail{ConversationID: conversationID, Type: ct, Settings: st}, nil
}

// ---------------------------------------------------------------------------
// Retention worker (R26).

// SweepExpired flips stale messages to expired and releases media assets.
func (s *Service) SweepExpired(ctx context.Context, batch int) (int, error) {
	if batch <= 0 || batch > 1000 {
		batch = 1000
	}
	var total int
	for {
		n, err := s.runSweepBatch(ctx, batch)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

func (s *Service) runSweepBatch(ctx context.Context, batch int) (int, error) {
	now := s.now()
	var expiredIDs []int64
	var assetIDs []int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		ids, err := sweepExpired(ctx, tx, now, batch)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := expireMessage(ctx, tx, id); err != nil {
				return err
			}
			assets, err := assetsForMessage(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := deleteAssets(ctx, tx, id); err != nil {
				return err
			}
			expiredIDs = append(expiredIDs, id)
			assetIDs = append(assetIDs, assets...)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(assetIDs) > 0 && s.media != nil {
		if err := s.media.OnReferencesRemoved(ctx, assetIDs); err != nil {
			return 0, err
		}
	}
	return len(expiredIDs), nil
}

// ---------------------------------------------------------------------------
// Forward (R11): thin wrapper over Send.

// ForwardInput is A9.
type ForwardInput struct {
	TargetConversationIDs []int64
	ClientMsgIDs          []string
}

// ForwardResult is one element of A9 results.
type ForwardResult struct {
	ConversationID  int64 `json:"conversation_id,string"`
	MessageID       int64 `json:"message_id,string"`
	ConversationSeq uint64 `json:"conversation_seq"`
}

// Forward copies a readable message into N target conversations (R11).
func (s *Service) Forward(ctx context.Context, userID, sourceMsgID int64, in ForwardInput) ([]ForwardResult, error) {
	if len(in.TargetConversationIDs) == 0 || len(in.TargetConversationIDs) != len(in.ClientMsgIDs) {
		return nil, apperrors.Invalid("target_conversation_ids and client_msg_ids must align")
	}
	src, err := getMessage(ctx, s.db, sourceMsgID)
	if err != nil {
		return nil, err
	}
	if src.Status == StatusRecalled || src.Status == StatusExpired {
		return nil, apperrors.New(apperrors.StateConflict, "cannot forward a recalled/expired message")
	}
	if src.SenderID != userID {
		ok, err := s.canRead(ctx, userID, src.ConversationID, src)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apperrors.Unavail("cannot read source message")
		}
	}
	results := make([]ForwardResult, 0, len(in.TargetConversationIDs))
	for i, targetID := range in.TargetConversationIDs {
		res, err := s.Send(ctx, userID, targetID, SendInput{
			ClientMsgID:     in.ClientMsgIDs[i],
			Type:            src.Type,
			Payload:         src.Payload,
			ForwardSourceID: src.ID,
		})
		if err != nil {
			return nil, err
		}
		results = append(results, ForwardResult{
			ConversationID: res.ConversationID, MessageID: res.MessageID, ConversationSeq: res.ConversationSeq,
		})
	}
	return results, nil
}
