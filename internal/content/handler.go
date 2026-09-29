package content

import (
	"encoding/json"
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func p64(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, apperrors.Invalid("invalid " + name)
	}
	return v, nil
}

// F1: POST /api/v1/official-accounts
func (h *Handler) CreateAccount(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Intro    string `json:"intro"`
		AvatarID string `json:"avatar_media_id,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := &CreateAccountInput{Name: body.Name, Intro: body.Intro, OperatorID: p.UserID}
	if body.AvatarID != "" {
		if id, err := strconv.ParseInt(body.AvatarID, 10, 64); err == nil {
			in.AvatarID = &id
		}
	}
	id, err := h.svc.CreateAccount(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"account_id": strconv.FormatInt(id, 10)})
}

// F2: PATCH /api/v1/official-accounts/{id}
func (h *Handler) UpdateAccount(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Name   string `json:"name"`
		Intro  string `json:"intro"`
		Status string `json:"status"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	err = h.svc.UpdateAccount(r.Context(), &UpdateAccountInput{
		ID: id, OperatorID: p.UserID, Name: body.Name, Intro: body.Intro, Status: body.Status,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F3: GET /api/v1/official-accounts?name=
func (h *Handler) LookupAccount(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	acc, err := h.svc.LookupAccount(r.Context(), name)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, acc)
}

// F4: GET /api/v1/official-accounts/{id}
func (h *Handler) GetAccount(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	acc, menu, follow, err := h.svc.GetAccount(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"account":    acc,
		"menu":       menu,
		"following":  follow != nil,
		"muted":      follow != nil && follow.Muted,
	})
}

// F5: POST /api/v1/official-accounts/{id}/follow
func (h *Handler) Follow(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Follow(r.Context(), id, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F5: DELETE /api/v1/official-accounts/{id}/follow
func (h *Handler) Unfollow(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Unfollow(r.Context(), id, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F20: PUT /api/v1/official-accounts/{id}/mute
func (h *Handler) SetMute(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Muted bool `json:"muted"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.SetMute(r.Context(), id, p.UserID, body.Muted); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F6: POST /api/v1/official-accounts/{id}/articles
func (h *Handler) CreateArticle(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	accountID, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Body    string `json:"body"`
		CoverID string `json:"cover_media_id,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := &CreateArticleInput{OperatorID: p.UserID, AccountID: accountID, Title: body.Title, Summary: body.Summary, Body: body.Body}
	if body.CoverID != "" {
		if id, err := strconv.ParseInt(body.CoverID, 10, 64); err == nil {
			in.CoverID = &id
		}
	}
	id, err := h.svc.CreateArticle(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"article_id": strconv.FormatInt(id, 10)})
}

// F7: PUT /api/v1/official-accounts/{id}/articles/{aid}
func (h *Handler) UpdateArticle(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	aid, err := p64(r, "aid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Body    string `json:"body"`
		CoverID string `json:"cover_media_id,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := &UpdateArticleInput{OperatorID: p.UserID, ID: aid, Title: body.Title, Summary: body.Summary, Body: body.Body}
	if body.CoverID != "" {
		if id, err := strconv.ParseInt(body.CoverID, 10, 64); err == nil {
			in.CoverID = &id
		}
	}
	if err := h.svc.UpdateArticle(r.Context(), in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F8: POST /api/v1/official-accounts/{id}/articles/{aid}/publish
func (h *Handler) PublishArticle(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	aid, err := p64(r, "aid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.PublishArticle(r.Context(), p.UserID, aid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F9: POST /api/v1/official-accounts/{id}/articles/{aid}/unpublish
func (h *Handler) UnpublishArticle(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	aid, err := p64(r, "aid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.UnpublishArticle(r.Context(), p.UserID, aid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F10: GET /api/v1/official-accounts/{id}/articles?cursor=
func (h *Handler) ListArticles(w http.ResponseWriter, r *http.Request) {
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var before int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	items, err := h.svc.ListPublishedArticles(r.Context(), id, before, 20)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"articles": items})
}

// F11: GET /api/v1/articles/{id}
func (h *Handler) GetArticle(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	art, err := h.svc.GetArticle(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, art)
}

// F12: PUT /api/v1/official-accounts/{id}/menu
func (h *Handler) SaveMenu(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Items []MenuInput `json:"items"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.SaveMenu(r.Context(), p.UserID, id, body.Items); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F13: POST /api/v1/official-accounts/{id}/service-sessions
func (h *Handler) StartSession(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.StartSession(r.Context(), id, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, res)
}

// F14: POST /api/v1/service-sessions/{id}/close
func (h *Handler) CloseSession(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.CloseSession(r.Context(), id, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F15: GET /api/v1/staff/service-sessions?account_id=&status=&cursor=
func (h *Handler) ListStaffSessions(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	accountID, err := p64FromQuery(r, "account_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var before int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	items, err := h.svc.ListStaffSessions(r.Context(), p.UserID, accountID, r.URL.Query().Get("status"), before, 50)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"sessions": items})
}

// F17: POST /api/v1/staff/service-sessions/{id}/messages
func (h *Handler) StaffReply(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := p64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if body.Type != "text" || body.Text == "" {
		httpx.Error(w, r, apperrors.Invalid("staff replies must be text"))
		return
	}
	if err := h.svc.StaffReply(r.Context(), id, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// F18: GET /api/v1/notifications?cursor=
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var before int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	items, err := h.svc.ListNotifications(r.Context(), p.UserID, before, 50)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"notifications": items})
}

// F19: POST /api/v1/notifications/read
func (h *Handler) MarkNotificationsRead(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		IDs []json.Number `json:"ids"`
		All bool          `json:"all"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ids := make([]int64, 0, len(body.IDs))
	for _, n := range body.IDs {
		if v, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			ids = append(ids, v)
		}
	}
	if err := h.svc.MarkNotificationsRead(r.Context(), p.UserID, ids, body.All); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

func p64FromQuery(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, apperrors.Invalid(name + " is required")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, apperrors.Invalid("invalid " + name)
	}
	return n, nil
}

// Mount wires F1-F20.
func Mount(mux *http.ServeMux, h *Handler, wrapWrite, wrapRead func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/official-accounts", wrapWrite(h.CreateAccount))
	mux.Handle("PATCH /api/v1/official-accounts/{id}", wrapWrite(h.UpdateAccount))
	mux.Handle("GET /api/v1/official-accounts", wrapRead(h.LookupAccount))
	mux.Handle("GET /api/v1/official-accounts/{id}", wrapRead(h.GetAccount))
	mux.Handle("POST /api/v1/official-accounts/{id}/follow", wrapWrite(h.Follow))
	mux.Handle("DELETE /api/v1/official-accounts/{id}/follow", wrapWrite(h.Unfollow))
	mux.Handle("PUT /api/v1/official-accounts/{id}/mute", wrapWrite(h.SetMute))

	mux.Handle("POST /api/v1/official-accounts/{id}/articles", wrapWrite(h.CreateArticle))
	mux.Handle("PUT /api/v1/official-accounts/{id}/articles/{aid}", wrapWrite(h.UpdateArticle))
	mux.Handle("POST /api/v1/official-accounts/{id}/articles/{aid}/publish", wrapWrite(h.PublishArticle))
	mux.Handle("POST /api/v1/official-accounts/{id}/articles/{aid}/unpublish", wrapWrite(h.UnpublishArticle))
	mux.Handle("GET /api/v1/official-accounts/{id}/articles", wrapRead(h.ListArticles))
	mux.Handle("GET /api/v1/articles/{id}", wrapRead(h.GetArticle))

	mux.Handle("PUT /api/v1/official-accounts/{id}/menu", wrapWrite(h.SaveMenu))

	mux.Handle("POST /api/v1/official-accounts/{id}/service-sessions", wrapWrite(h.StartSession))
	mux.Handle("POST /api/v1/service-sessions/{id}/close", wrapWrite(h.CloseSession))
	mux.Handle("GET /api/v1/staff/service-sessions", wrapRead(h.ListStaffSessions))
	mux.Handle("POST /api/v1/staff/service-sessions/{id}/messages", wrapWrite(h.StaffReply))

	mux.Handle("GET /api/v1/notifications", wrapRead(h.ListNotifications))
	mux.Handle("POST /api/v1/notifications/read", wrapWrite(h.MarkNotificationsRead))
}
