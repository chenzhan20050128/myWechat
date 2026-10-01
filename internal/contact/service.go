package contact

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/example/wechat/internal/audit"
	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/pagination"
	"github.com/example/wechat/internal/platform/ratelimit"
	"github.com/example/wechat/internal/platform/sigtoken"
	"github.com/example/wechat/internal/platform/validate"
	"github.com/example/wechat/internal/user"
)

const maxTagMembersPerCall = 200

// Service owns every write to the friend-request, friendship, friend-settings
// and tag tables, and is the read path other modules use for relationship
// questions (ADR-001: tables are written only by their owning module).
type Service struct {
	db    *sql.DB
	now   clock.Clock
	users *user.Service
	convs *conversation.Service
	audit *audit.Service
	qr    *sigtoken.Codec
	rl    *ratelimit.Limiter
}

// New wires the contact service.
func New(db *sql.DB, users *user.Service, convs *conversation.Service, aud *audit.Service,
	c cache.Cache, qr *sigtoken.Codec, clk clock.Clock) *Service {
	return &Service{
		db: db, users: users, convs: convs, audit: aud, qr: qr, now: clk,
		rl: ratelimit.New(c, "contact.lookup", LookupPerMinute, time.Minute),
	}
}

// CreateRequestInput carries one application attempt (R1).
type CreateRequestInput struct {
	ApplicantID int64
	Source      string
	TargetID    int64
	Phone       string
	AccountName string
	QRToken     string
	CardOwnerID int64
	VerifyText  string
	IP          string
}

// AcceptResult reports the state after an accept. Repeating an accept returns
// the current state instead of an error (SPEC-02 §5).
type AcceptResult struct {
	Status         string
	Epoch          int64
	ConversationID int64
}

// CreateRequest resolves the entry point and merges the application into the
// single pending row for the direction (R1, R3-R7).
func (s *Service) CreateRequest(ctx context.Context, in CreateRequestInput) (RequestRow, error) {
	if err := validateSource(in.Source); err != nil {
		return RequestRow{}, err
	}
	if err := validateVerifyText(in.VerifyText); err != nil {
		return RequestRow{}, err
	}
	targetID, err := s.resolveTarget(ctx, in)
	if err != nil {
		return RequestRow{}, err
	}
	if targetID == in.ApplicantID {
		return RequestRow{}, apperrors.Invalid("cannot add yourself")
	}

	// R4: the target must exist. The identity map is also the module boundary
	// that keeps contact out of the users table.
	identities, err := s.users.Identities(ctx, []int64{targetID})
	if err != nil {
		return RequestRow{}, err
	}
	if _, ok := identities[targetID]; !ok {
		return RequestRow{}, apperrors.Unavail("user not found")
	}

	lo, hi := normalizePair(in.ApplicantID, targetID)
	if _, ok, err := activeFriendship(ctx, s.db, lo, hi); err != nil {
		return RequestRow{}, err
	} else if ok {
		return RequestRow{}, apperrors.New(apperrors.AlreadyFriend, "already friends")
	}

	// R5 + contract §4.4: a block in either direction forbids new applications.
	if err := s.assertNotBlocked(ctx, in.ApplicantID, targetID); err != nil {
		return RequestRow{}, err
	}

	// R6: rejection imposes a 24h cooldown on the same direction.
	if at, ok, err := lastRejectedAt(ctx, s.db, in.ApplicantID, targetID); err != nil {
		return RequestRow{}, err
	} else if ok {
		if left := cooldownRemaining(at, s.now.Now()); left > 0 {
			return RequestRow{}, apperrors.Conflict(fmt.Sprintf(
				"previous request was rejected, retry in %d seconds", int(left.Seconds())+1))
		}
	}

	var row RequestRow
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, err := lockEpochRow(ctx, tx, lo, hi); err != nil {
			return err
		}
		if _, ok, err := activeFriendship(ctx, tx, lo, hi); err != nil {
			return err
		} else if ok {
			return apperrors.New(apperrors.AlreadyFriend, "already friends")
		}
		if err := s.assertNotBlockedTx(ctx, tx, in.ApplicantID, targetID); err != nil {
			return err
		}
		id, err := upsertPendingRequest(ctx, tx, in, targetID, s.now.Now())
		if err != nil {
			return apperrors.Wrap(apperrors.InternalError, "create friend request", err)
		}
		row, err = findRequest(ctx, tx, id)
		return err
	})
	if err != nil {
		return RequestRow{}, err
	}
	return row, nil
}

// resolveTarget maps the five entry points onto a user id (R1).
func (s *Service) resolveTarget(ctx context.Context, in CreateRequestInput) (int64, error) {
	switch in.Source {
	case SourcePhone:
		if in.Phone == "" {
			return 0, apperrors.Invalid("phone is required for source=phone")
		}
		phone := validate.NormalizePhone(in.Phone)
		if !validate.Phone(phone) {
			return 0, apperrors.Invalid("invalid phone")
		}
		it, err := s.users.FindIdentityByIdentifier(ctx, phone, "")
		if err != nil {
			return 0, err
		}
		return it.UserID, nil
	case SourceAccountName:
		if in.AccountName == "" {
			return 0, apperrors.Invalid("account_name is required for source=account_name")
		}
		name := validate.NormalizeAccountName(in.AccountName)
		if name == "" {
			return 0, apperrors.Invalid("invalid account_name")
		}
		it, err := s.users.FindIdentityByIdentifier(ctx, "", name)
		if err != nil {
			return 0, err
		}
		return it.UserID, nil
	case SourceQRCode:
		if in.QRToken == "" {
			return 0, apperrors.Invalid("qrcode_token is required for source=qrcode")
		}
		return decodeQR(s.qr, in.QRToken, s.now.Now())
	case SourceGroup:
		// R1: co-membership needs the group module, which lands in phase 2.
		return 0, apperrors.Unavail("group entry is not available yet")
	case SourceCard:
		if in.TargetID <= 0 {
			return 0, apperrors.Invalid("target_id is required for source=card")
		}
		if in.CardOwnerID <= 0 {
			return 0, apperrors.Invalid("card_owner_id is required for source=card")
		}
		// R1: a card is only a valid voucher when a friend shared it.
		owners, err := activeFriendIDsIn(ctx, s.db, in.ApplicantID, []int64{in.CardOwnerID})
		if err != nil {
			return 0, err
		}
		if _, ok := owners[in.CardOwnerID]; !ok {
			return 0, apperrors.Unavail("card owner is not a friend")
		}
		return in.TargetID, nil
	default:
		return 0, apperrors.Invalid("source must be phone|account_name|qrcode|group|card")
	}
}

// assertNotBlocked enforces R5 in both directions (contract §4.4).
func (s *Service) assertNotBlocked(ctx context.Context, applicant, target int64) error {
	return s.assertNotBlockedTx(ctx, s.db, applicant, target)
}

func (s *Service) assertNotBlockedTx(ctx context.Context, db mysqlx.DBTX, applicant, target int64) error {
	byTarget, err := loadSettings(ctx, db, target, []int64{applicant})
	if err != nil {
		return err
	}
	if settingsOf(byTarget, applicant).MessagePerm == PermBlocked {
		return apperrors.Unavail("user not found")
	}
	byApplicant, err := loadSettings(ctx, db, applicant, []int64{target})
	if err != nil {
		return err
	}
	if settingsOf(byApplicant, target).MessagePerm == PermBlocked {
		return apperrors.Unavail("user not found")
	}
	return nil
}

// AcceptRequest approves an application, opens a new friendship epoch and
// creates the direct conversation in one transaction (R8).
func (s *Service) AcceptRequest(ctx context.Context, me, requestID int64, ip string) (AcceptResult, error) {
	head, err := findRequest(ctx, s.db, requestID)
	if err != nil {
		return AcceptResult{}, err
	}
	if head.TargetID != me {
		return AcceptResult{}, apperrors.New(apperrors.Forbidden, "only the recipient may accept")
	}
	lo, hi := normalizePair(head.ApplicantID, head.TargetID)

	var res AcceptResult
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		now := s.now.Now()
		// ADR-006 lock order: the pair lock precedes the business row.
		if _, err := lockEpochRow(ctx, tx, lo, hi); err != nil {
			return err
		}
		req, err := lockRequestTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if req.TargetID != me {
			return apperrors.New(apperrors.Forbidden, "only the recipient may accept")
		}
		epoch, active, err := activeFriendship(ctx, tx, lo, hi)
		if err != nil {
			return err
		}
		if req.Status != RequestPending {
			res = AcceptResult{Status: req.Status}
			if active {
				res.Epoch = epoch
				// SPEC-02 §5: a retried accept reports the same state as the
				// first one. Without the conversation id a client whose first
				// call timed out could never open the chat it just created.
				convID, err := s.convs.CreateDirectTx(ctx, tx, lo, hi)
				if err != nil {
					return apperrors.Wrap(apperrors.InternalError, "resolve direct conversation", err)
				}
				res.ConversationID = convID
			}
			return nil
		}
		if !active {
			if epoch, err = nextEpoch(ctx, tx, lo, hi); err != nil {
				return err
			}
			if err := insertFriendship(ctx, tx, lo, hi, epoch, now); err != nil {
				return err
			}
			if err := setCurrentEpoch(ctx, tx, lo, hi, epoch); err != nil {
				return err
			}
		}
		// R8④ / ruling 3: a new cycle restarts both directions from defaults.
		if err := resetSettings(ctx, tx, lo, hi, now); err != nil {
			return err
		}
		if err := resetSettings(ctx, tx, hi, lo, now); err != nil {
			return err
		}
		convID, err := s.convs.CreateDirectTx(ctx, tx, lo, hi)
		if err != nil {
			return apperrors.Wrap(apperrors.InternalError, "create direct conversation", err)
		}
		if err := setRequestStatus(ctx, tx, requestID, RequestAccepted, now); err != nil {
			return err
		}
		if err := s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "contact.request.accepted", ActorID: me, IP: ip,
			Detail: map[string]any{
				"request_id": requestID, "applicant_id": req.ApplicantID, "epoch": epoch,
			},
		}); err != nil {
			return err
		}
		res = AcceptResult{Status: RequestAccepted, Epoch: epoch, ConversationID: convID}
		return nil
	})
	if err != nil {
		return AcceptResult{}, err
	}
	return res, nil
}

// RejectRequest declines an application; only the recipient may (R9).
func (s *Service) RejectRequest(ctx context.Context, me, requestID int64) error {
	return s.transition(ctx, me, requestID, RequestRejected, true)
}

// CancelRequest withdraws an application; only the applicant may (R9).
func (s *Service) CancelRequest(ctx context.Context, me, requestID int64) error {
	return s.transition(ctx, me, requestID, RequestCancelled, false)
}

func (s *Service) transition(ctx context.Context, me, requestID int64, status string, byTarget bool) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		req, err := lockRequestTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if byTarget && req.TargetID != me {
			return apperrors.New(apperrors.Forbidden, "only the recipient may reject")
		}
		if !byTarget && req.ApplicantID != me {
			return apperrors.New(apperrors.Forbidden, "only the applicant may cancel")
		}
		if req.Status != RequestPending {
			return apperrors.Conflict("request is already " + req.Status)
		}
		return setRequestStatus(ctx, tx, requestID, status, s.now.Now())
	})
}

// DeleteFriend ends the current epoch; the epoch pointer and the settings rows
// survive, and history is untouched (R12).
func (s *Service) DeleteFriend(ctx context.Context, me, friendID int64, ip string) error {
	if friendID <= 0 {
		return apperrors.Invalid("invalid friend id")
	}
	if friendID == me {
		return apperrors.Invalid("cannot delete yourself")
	}
	lo, hi := normalizePair(me, friendID)
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, err := lockEpochRow(ctx, tx, lo, hi); err != nil {
			return err
		}
		epoch, ok, err := activeFriendship(ctx, tx, lo, hi)
		if err != nil {
			return err
		}
		if !ok {
			return apperrors.Unavail("not a friend")
		}
		if err := endFriendship(ctx, tx, lo, hi, epoch, s.now.Now()); err != nil {
			return err
		}
		return s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "contact.friend.deleted", ActorID: me, IP: ip,
			Detail: map[string]any{"friend_id": friendID, "epoch": epoch},
		})
	})
}

// UpdateFriendSettings applies a validated patch to the caller's own view of a
// friend (R15-R19); the counterpart's row is never touched.
func (s *Service) UpdateFriendSettings(ctx context.Context, me, friendID int64, patch SettingsPatch, ip string) (Settings, error) {
	if friendID <= 0 || friendID == me {
		return Settings{}, apperrors.Invalid("invalid friend id")
	}
	if err := patch.Validate(); err != nil {
		return Settings{}, err
	}
	lo, hi := normalizePair(me, friendID)
	var out Settings
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, err := lockEpochRow(ctx, tx, lo, hi); err != nil {
			return err
		}
		current, err := loadSettings(ctx, tx, me, []int64{friendID})
		if err != nil {
			return err
		}
		_, standing := current[friendID]
		if !standing {
			// R15/R17: a standing row is editable on its own — a block that
			// outlived the friendship must stay releasable, or the pair would
			// be stuck: blocked users cannot re-apply (R5) and could never be
			// unblocked. Without a row the pair has to be friends.
			if _, active, err := activeFriendship(ctx, tx, lo, hi); err != nil {
				return err
			} else if !active {
				return apperrors.Unavail("not a friend")
			}
		}
		base := settingsOf(current, friendID)
		next := patch.Apply(base)
		if err := upsertSettings(ctx, tx, me, friendID, next, s.now.Now()); err != nil {
			return err
		}
		out = next
		return s.auditBlockChange(ctx, tx, me, friendID, base, next, ip)
	})
	if err != nil {
		return Settings{}, err
	}
	return out, nil
}

// auditBlockChange records the transitions the contract requires auditing
// (§16.2: 好友同意、删除、拉黑).
func (s *Service) auditBlockChange(ctx context.Context, tx mysqlx.Tx, me, friendID int64, base, next Settings, ip string) error {
	switch {
	case next.MessagePerm == base.MessagePerm:
		return nil
	case next.MessagePerm == PermBlocked:
		return s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "contact.friend.blocked", ActorID: me, IP: ip,
			Detail: map[string]any{"friend_id": friendID},
		})
	case base.MessagePerm == PermBlocked:
		return s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "contact.friend.unblocked", ActorID: me, IP: ip,
			Detail: map[string]any{"friend_id": friendID, "message_perm": next.MessagePerm},
		})
	default:
		return nil
	}
}

// GetActiveFriendship reports the live epoch between two users (R11, R14).
func (s *Service) GetActiveFriendship(ctx context.Context, a, b int64) (int64, bool, error) {
	if a <= 0 || b <= 0 || a == b {
		return 0, false, nil
	}
	lo, hi := normalizePair(a, b)
	return activeFriendship(ctx, s.db, lo, hi)
}

// ListFriendIDs pages the active friend ids by ascending id — the primitive
// the phase-3 moment module snapshots (R14).
func (s *Service) ListFriendIDs(ctx context.Context, userID, afterID int64, limit int) ([]int64, error) {
	return listActiveFriendIDs(ctx, s.db, userID, afterID, pagination.Limit(limit))
}

// CanSendMessage answers the phase-2 message permission question (R18).
func (s *Service) CanSendMessage(ctx context.Context, from, to int64) (bool, string, error) {
	if _, ok, err := s.GetActiveFriendship(ctx, from, to); err != nil {
		return false, "", err
	} else if !ok {
		return false, ReasonNotFriend, nil
	}
	mine, err := loadSettings(ctx, s.db, from, []int64{to})
	if err != nil {
		return false, "", err
	}
	theirs, err := loadSettings(ctx, s.db, to, []int64{from})
	if err != nil {
		return false, "", err
	}
	ok, reason := decideSend(settingsOf(mine, to), settingsOf(theirs, from))
	return ok, reason, nil
}

// CanSendMessageTx performs the same permission decision inside the caller's
// transaction and locks the pair epoch first, serializing it with friendship
// deletion and message-permission updates.
func (s *Service) CanSendMessageTx(ctx context.Context, tx mysqlx.Tx, from, to int64) (bool, string, error) {
	if from <= 0 || to <= 0 || from == to {
		return false, ReasonNotFriend, nil
	}
	lo, hi := normalizePair(from, to)
	if _, err := lockEpochRow(ctx, tx, lo, hi); err != nil {
		return false, "", err
	}
	if _, ok, err := activeFriendship(ctx, tx, lo, hi); err != nil {
		return false, "", err
	} else if !ok {
		return false, ReasonNotFriend, nil
	}
	mine, err := loadSettings(ctx, tx, from, []int64{to})
	if err != nil {
		return false, "", err
	}
	theirs, err := loadSettings(ctx, tx, to, []int64{from})
	if err != nil {
		return false, "", err
	}
	ok, reason := decideSend(settingsOf(mine, to), settingsOf(theirs, from))
	return ok, reason, nil
}

// ExpireStaleRequests flips pending applications older than RequestTTL to
// expired (R2); the worker drives it from phase 2 onwards.
func (s *Service) ExpireStaleRequests(ctx context.Context) (int64, error) {
	now := s.now.Now()
	return expireStaleRequests(ctx, s.db, now.Add(-RequestTTL), now)
}

// FriendEpoch is one (user_id, current friendship_epoch) pair for snapshot.
type FriendEpoch struct {
	UserID int64
	Epoch  int64
}

// ActiveFriendsWithEpoch returns author's current active friends with their
// epoch (SPEC-06 §2, moment port).
func (s *Service) ActiveFriendsWithEpoch(ctx context.Context, authorID int64) ([]FriendEpoch, error) {
	return listActiveFriendEpochs(ctx, s.db, authorID)
}

// IsFriendCurrentEpoch reports whether viewer is currently friends with author
// and the live epoch matches `epoch` (snapshot vs. realtime).
func (s *Service) IsFriendCurrentEpoch(ctx context.Context, authorID, viewerID, epoch int64) (bool, error) {
	live, ok, err := s.GetActiveFriendship(ctx, authorID, viewerID)
	if err != nil || !ok {
		return false, err
	}
	return live == epoch, nil
}

// MomentPerm returns author's live restrictions against viewer (hidden/blocked/
// no_moments). Mapping from friend_settings.moment_perm (visible/hidden) and
// message_perm (normal/no_message/blocked).
func (s *Service) MomentPerm(ctx context.Context, authorID, viewerID int64) (hidden, blocked, noMoments bool, err error) {
	mine, err := loadSettings(ctx, s.db, authorID, []int64{viewerID})
	if err != nil {
		return false, false, false, err
	}
	st := settingsOf(mine, viewerID)
	if st.MomentPerm == "hidden" {
		hidden = true
	}
	switch st.MessagePerm {
	case "blocked":
		blocked = true
	case "no_message":
		noMoments = true
	}
	return hidden, blocked, noMoments, nil
}

// ExpandTag returns (user_id, epoch) snapshot for a tag (SPEC-06 §2).
func (s *Service) ExpandTag(ctx context.Context, ownerID, tagID int64) ([]FriendEpoch, error) {
	return listTagMemberEpochs(ctx, s.db, ownerID, tagID)
}

// IsMuted reports viewer's moment_notify=false against author (mute_author).
func (s *Service) IsMuted(ctx context.Context, viewerID, authorID int64) (bool, error) {
	mine, err := loadSettings(ctx, s.db, viewerID, []int64{authorID})
	if err != nil {
		return false, err
	}
	st := settingsOf(mine, authorID)
	return !st.MomentNotify, nil
}

// IssueQRCode signs the caller's personal QR token (R1).
func (s *Service) IssueQRCode(ctx context.Context, userID int64) (string, time.Time, error) {
	return encodeQR(s.qr, userID, s.now.Now())
}

// LookupResult is the stranger-visible slice of a profile (R24, contract §4.5).
type LookupResult struct {
	Identity user.Identity
	IsFriend bool
}

// Lookup resolves an exact phone or account_name without leaking anything a
// non-friend may not see (R24, R27).
func (s *Service) Lookup(ctx context.Context, me int64, phone, accountName string) (LookupResult, error) {
	if ok, _ := s.rl.Allow(ctx, strconv.FormatInt(me, 10)); !ok {
		return LookupResult{}, apperrors.New(apperrors.RateLimited, "too many lookups")
	}
	if phone == "" && accountName == "" {
		return LookupResult{}, apperrors.Invalid("phone or account_name required")
	}
	if phone != "" {
		normalized := validate.NormalizePhone(phone)
		if !validate.Phone(normalized) {
			return LookupResult{}, apperrors.Invalid("invalid phone")
		}
		phone = normalized
	} else {
		normalized := validate.NormalizeAccountName(accountName)
		if normalized == "" {
			return LookupResult{}, apperrors.Invalid("invalid account_name")
		}
		accountName = normalized
	}
	it, err := s.users.FindIdentityByIdentifier(ctx, phone, accountName)
	if err != nil {
		return LookupResult{}, err
	}
	_, isFriend, err := s.GetActiveFriendship(ctx, me, it.UserID)
	if err != nil {
		return LookupResult{}, err
	}
	return LookupResult{Identity: it, IsFriend: isFriend}, nil
}

// FriendEntry is one address-book row (R25).
type FriendEntry struct {
	UserID   int64
	Identity user.Identity
	Settings Settings
	Tags     []TagRef
}

// ListFriends pages the address book by ascending friend id (R25).
func (s *Service) ListFriends(ctx context.Context, me int64, cursor string, limit int) ([]FriendEntry, string, error) {
	cur, err := pagination.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	pageSize := pagination.LimitWithMax(limit, 50)
	ids, err := listActiveFriendIDs(ctx, s.db, me, cur.ID, pageSize+1)
	if err != nil {
		return nil, "", err
	}
	hasMore := len(ids) > pageSize
	if hasMore {
		ids = ids[:pageSize]
	}
	entries, err := s.hydrate(ctx, me, ids)
	if err != nil {
		return nil, "", err
	}
	next, err := cursorAfter(hasMore, ids)
	if err != nil {
		return nil, "", err
	}
	return entries, next, nil
}

// SearchFriends matches nickname or remark, case-insensitively (R26). Nickname
// lives in the user module, so the MVP scans at most SearchScanCap friends and
// matches in memory rather than joining across the module boundary.
func (s *Service) SearchFriends(ctx context.Context, me int64, q, cursor string, limit int) ([]FriendEntry, string, error) {
	if q == "" {
		return nil, "", apperrors.Invalid("q is required")
	}
	cur, err := pagination.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	pageSize := pagination.LimitWithMax(limit, 50)
	ids, err := listActiveFriendIDs(ctx, s.db, me, cur.ID, SearchScanCap+1)
	if err != nil {
		return nil, "", err
	}
	truncated := len(ids) > SearchScanCap
	if truncated {
		ids = ids[:SearchScanCap]
	}
	entries, err := s.hydrate(ctx, me, ids)
	if err != nil {
		return nil, "", err
	}

	var out []FriendEntry
	next := ""
	for _, e := range entries {
		if !matchesSearch(q, e.Identity.Nickname, e.Settings.Remark) {
			continue
		}
		if len(out) == pageSize {
			next, err = encodeID(out[len(out)-1].UserID)
			if err != nil {
				return nil, "", err
			}
			break
		}
		out = append(out, e)
	}
	if next == "" && truncated {
		if next, err = cursorAfter(true, ids); err != nil {
			return nil, "", err
		}
	}
	return out, next, nil
}

// hydrate joins the address-book page with the user module identities, the
// caller's own settings and tags — three batch reads, never per-row queries.
func (s *Service) hydrate(ctx context.Context, me int64, ids []int64) ([]FriendEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	identities, err := s.users.Identities(ctx, ids)
	if err != nil {
		return nil, err
	}
	settings, err := loadSettings(ctx, s.db, me, ids)
	if err != nil {
		return nil, err
	}
	tags, err := loadTags(ctx, s.db, me, ids)
	if err != nil {
		return nil, err
	}
	out := make([]FriendEntry, 0, len(ids))
	for _, id := range ids {
		it, ok := identities[id]
		if !ok {
			// The account disappeared mid-page; skipping beats fabricating.
			continue
		}
		out = append(out, FriendEntry{
			UserID:   id,
			Identity: it,
			Settings: settingsOf(settings, id),
			Tags:     tags[id],
		})
	}
	return out, nil
}

// RequestEntry is one mailbox row with both parties resolved (R10).
type RequestEntry struct {
	Request   RequestRow
	Applicant user.Identity
	Target    user.Identity
}

// ListRequests pages the received or sent mailbox newest-first (R10).
func (s *Service) ListRequests(ctx context.Context, me int64, box, cursor string, limit int) ([]RequestEntry, string, error) {
	cur, err := pagination.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	pageSize := pagination.LimitWithMax(limit, 50)
	rows, err := listRequests(ctx, s.db, box, me, cur, pageSize+1)
	if err != nil {
		return nil, "", err
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}

	ids := make([]int64, 0, 2*len(rows))
	for _, r := range rows {
		ids = append(ids, r.ApplicantID, r.TargetID)
	}
	identities, err := s.users.Identities(ctx, ids)
	if err != nil {
		return nil, "", err
	}
	out := make([]RequestEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, RequestEntry{
			Request:   r,
			Applicant: identities[r.ApplicantID],
			Target:    identities[r.TargetID],
		})
	}
	next := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next, err = pagination.Encode(pagination.Page{Sort: last.CreatedAt.UnixMicro(), ID: last.ID})
		if err != nil {
			return nil, "", err
		}
	}
	return out, next, nil
}

// CreateTag adds a named tag for the caller (R20).
func (s *Service) CreateTag(ctx context.Context, me int64, name string) (Tag, error) {
	if err := validateTagName(name); err != nil {
		return Tag{}, err
	}
	now := s.now.Now()
	id, err := insertTag(ctx, s.db, me, name, now)
	if mysqlx.IsDuplicate(err, "uk_ctags_owner_name") {
		return Tag{}, apperrors.Conflict("tag name already exists")
	}
	if err != nil {
		return Tag{}, apperrors.Wrap(apperrors.InternalError, "create tag", err)
	}
	return Tag{ID: id, Name: name, CreatedAt: now}, nil
}

// RenameTag renames one of the caller's tags (R20).
func (s *Service) RenameTag(ctx context.Context, me, tagID int64, name string) error {
	if err := validateTagName(name); err != nil {
		return err
	}
	if err := s.assertTagOwner(ctx, me, tagID); err != nil {
		return err
	}
	if err := renameTag(ctx, s.db, tagID, name); err != nil {
		if mysqlx.IsDuplicate(err, "uk_ctags_owner_name") {
			return apperrors.Conflict("tag name already exists")
		}
		return apperrors.Wrap(apperrors.InternalError, "rename tag", err)
	}
	return nil
}

// DeleteTag removes a tag and its membership rows; friendships and published
// moment snapshots are untouched (R22).
func (s *Service) DeleteTag(ctx context.Context, me, tagID int64) error {
	if err := s.assertTagOwner(ctx, me, tagID); err != nil {
		return err
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return deleteTagTx(ctx, tx, tagID)
	})
}

// ListTags returns the caller's tags ordered by name (R20).
func (s *Service) ListTags(ctx context.Context, me int64) ([]Tag, error) {
	return listTags(ctx, s.db, me)
}

// AddTagMembers attaches friends to a tag; repeats are no-ops (R21, R23).
func (s *Service) AddTagMembers(ctx context.Context, me, tagID int64, friendIDs []int64) error {
	return s.modifyTagMembers(ctx, me, tagID, friendIDs, addTagMembers)
}

// RemoveTagMembers detaches friends from a tag; absent rows are no-ops (R21).
func (s *Service) RemoveTagMembers(ctx context.Context, me, tagID int64, friendIDs []int64) error {
	return s.modifyTagMembers(ctx, me, tagID, friendIDs, removeTagMembers)
}

func (s *Service) modifyTagMembers(ctx context.Context, me, tagID int64, friendIDs []int64,
	apply func(context.Context, mysqlx.Tx, int64, []int64) error) error {
	ids, err := dedupeIDs(friendIDs)
	if err != nil {
		return err
	}
	if err := s.assertTagOwner(ctx, me, tagID); err != nil {
		return err
	}
	if len(ids) > 0 {
		// R23: tagging targets must be current friends.
		friends, err := activeFriendIDsIn(ctx, s.db, me, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, ok := friends[id]; !ok {
				return apperrors.Unavail("not a friend")
			}
		}
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return apply(ctx, tx, tagID, ids)
	})
}

// assertTagOwner hides other users' tags behind RESOURCE_UNAVAILABLE (R19).
func (s *Service) assertTagOwner(ctx context.Context, me, tagID int64) error {
	if tagID <= 0 {
		return apperrors.Invalid("invalid tag id")
	}
	owner, err := tagOwner(ctx, s.db, tagID)
	if err != nil {
		return err
	}
	if owner != me {
		return apperrors.Unavail("tag not found")
	}
	return nil
}

// settingsOf resolves a missing row to the defaults (R15).
func settingsOf(m map[int64]Settings, id int64) Settings {
	if s, ok := m[id]; ok {
		return s
	}
	return DefaultSettings()
}

// dedupeIDs validates and de-duplicates a batch of friend ids (R21).
func dedupeIDs(ids []int64) ([]int64, error) {
	if len(ids) > maxTagMembersPerCall {
		return nil, apperrors.Invalid(fmt.Sprintf("at most %d friends per call", maxTagMembersPerCall))
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, apperrors.Invalid("invalid friend id")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// cursorAfter encodes the friend-id cursor for the last row of a page.
func cursorAfter(hasMore bool, ids []int64) (string, error) {
	if !hasMore || len(ids) == 0 {
		return "", nil
	}
	return encodeID(ids[len(ids)-1])
}

func encodeID(id int64) (string, error) {
	return pagination.Encode(pagination.Page{ID: id})
}
