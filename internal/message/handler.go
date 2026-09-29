package message

import (
	"encoding/json"
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler wires the message HTTP surface (A1-A12).
type Handler struct {
	svc *Service
}

// NewHandler builds a Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func pathInt64(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, apperrors.Invalid("invalid " + name)
	}
	return v, nil
}

// Send handles POST /api/v1/conversations/{id}/messages (A1).
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		ClientMsgID string          `json:"client_msg_id"`
		Type        string          `json:"type"`
		Payload     json.RawMessage `json:"payload"`
		RefMsgID    string          `json:"ref_message_id,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := SendInput{ClientMsgID: body.ClientMsgID, Type: body.Type, Payload: body.Payload}
	if body.RefMsgID != "" {
		if id, err := strconv.ParseInt(body.RefMsgID, 10, 64); err == nil {
			in.RefMessageID = id
		}
	}
	res, err := h.svc.Send(r.Context(), p.UserID, convID, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"message_id":       strconv.FormatInt(res.MessageID, 10),
		"conversation_id":  strconv.FormatInt(res.ConversationID, 10),
		"conversation_seq": res.ConversationSeq,
		"status":           res.Status,
		"created_at":       res.CreatedAt,
	})
}

// History handles GET /api/v1/conversations/{id}/messages (A2).
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	in := HistoryInput{}
	if v := q.Get("after_seq"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			in.AfterSeq = n
		}
	}
	if v := q.Get("before_seq"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			in.BeforeSeq = n
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			in.Limit = n
		}
	}
	res, err := h.svc.History(r.Context(), p.UserID, convID, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, res)
}

// ListConversations handles GET /api/v1/conversations (A3).
func (h *Handler) ListConversations(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.svc.ListConversations(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"conversations": items})
}

// GetConversation handles GET /api/v1/conversations/{id} (A4).
func (h *Handler) GetConversation(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	detail, err := h.svc.GetConversation(r.Context(), p.UserID, convID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, detail)
}

// Read handles POST /api/v1/conversations/{id}/read (A5).
func (h *Handler) Read(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Seq uint64 `json:"seq"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	lastRead, err := h.svc.ReadSeq(r.Context(), p.UserID, convID, body.Seq)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"last_read_seq": lastRead})
}

// MarkUnread handles POST /api/v1/conversations/{id}/mark-unread (A6).
func (h *Handler) MarkUnread(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.MarkUnread(r.Context(), p.UserID, convID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// UpdateSettings handles PATCH /api/v1/conversations/{id}/settings (A7).
func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Pinned     *bool   `json:"pinned,omitempty"`
		Muted      *bool   `json:"muted,omitempty"`
		Background *string `json:"background,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.UpdateSettings(r.Context(), p.UserID, convID, SettingsPatch{
		Pinned: body.Pinned, Muted: body.Muted, Background: body.Background,
	}); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Recall handles POST /api/v1/messages/{id}/recall (A8).
func (h *Handler) Recall(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	msgID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Recall(r.Context(), p.UserID, msgID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Forward handles POST /api/v1/messages/{id}/forward (A9).
func (h *Handler) Forward(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	msgID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		TargetConversationIDs []string `json:"target_conversation_ids"`
		ClientMsgIDs         []string `json:"client_msg_ids"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if len(body.TargetConversationIDs) != len(body.ClientMsgIDs) {
		httpx.Error(w, r, apperrors.Invalid("target_conversation_ids and client_msg_ids must align"))
		return
	}
	in := ForwardInput{ClientMsgIDs: body.ClientMsgIDs}
	for _, s := range body.TargetConversationIDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			httpx.Error(w, r, apperrors.Invalid("invalid conversation id"))
			return
		}
		in.TargetConversationIDs = append(in.TargetConversationIDs, id)
	}
	results, err := h.svc.Forward(r.Context(), p.UserID, msgID, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"results": results})
}

// Pin handles POST /api/v1/conversations/{id}/pins (A10).
func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		MessageID string `json:"message_id"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	msgID, err := strconv.ParseInt(body.MessageID, 10, 64)
	if err != nil {
		httpx.Error(w, r, apperrors.Invalid("invalid message_id"))
		return
	}
	if err := h.svc.Pin(r.Context(), p.UserID, convID, msgID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Unpin handles DELETE /api/v1/conversations/{id}/pins/{message_id} (A11).
func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	msgID, err := pathInt64(r, "message_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Unpin(r.Context(), convID, msgID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// ListPins handles GET /api/v1/conversations/{id}/pins (A12).
func (h *Handler) ListPins(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	convID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.svc.ListPins(r.Context(), convID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"pins": items})
}

// Mount wires all A1-A12 routes.
func Mount(mux *http.ServeMux, h *Handler, wrap func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/conversations/{id}/messages", wrap(h.Send))
	mux.Handle("GET /api/v1/conversations/{id}/messages", wrap(h.History))
	mux.Handle("GET /api/v1/conversations", wrap(h.ListConversations))
	mux.Handle("GET /api/v1/conversations/{id}", wrap(h.GetConversation))
	mux.Handle("POST /api/v1/conversations/{id}/read", wrap(h.Read))
	mux.Handle("POST /api/v1/conversations/{id}/mark-unread", wrap(h.MarkUnread))
	mux.Handle("PATCH /api/v1/conversations/{id}/settings", wrap(h.UpdateSettings))
	mux.Handle("POST /api/v1/messages/{id}/recall", wrap(h.Recall))
	mux.Handle("POST /api/v1/messages/{id}/forward", wrap(h.Forward))
	mux.Handle("POST /api/v1/conversations/{id}/pins", wrap(h.Pin))
	mux.Handle("DELETE /api/v1/conversations/{id}/pins/{message_id}", wrap(h.Unpin))
	mux.Handle("GET /api/v1/conversations/{id}/pins", wrap(h.ListPins))
}
