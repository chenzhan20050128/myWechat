package moment

import (
	"net/http"
	"strconv"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler wires the moment HTTP surface (C1-C19).
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

func parseInt64Query(r *http.Request, key string) int64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func parseAssetIDs(raw []string) []int64 {
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

type publishBody struct {
	Content         string   `json:"content"`
	Country         string   `json:"country"`
	Province        string   `json:"province"`
	City            string   `json:"city"`
	PlaceName       string   `json:"place_name"`
	AssetIDs        []string `json:"asset_ids"`
	Visibility      string   `json:"visibility"`
	AllowedUserIDs  []string `json:"allowed_user_ids"`
	TagID           string   `json:"tag_id"`
	ExcludedUserIDs []string `json:"excluded_user_ids"`
	AllowComments   *bool    `json:"allow_comments"`
	AllowLikes      *bool    `json:"allow_likes"`
	RunAt           string   `json:"run_at,omitempty"`
}

func (b *publishBody) toInput(authorID int64) (*PublishInput, error) {
	in := &PublishInput{
		AuthorID:      authorID,
		Content:       b.Content,
		Country:       b.Country,
		Province:      b.Province,
		City:          b.City,
		PlaceName:     b.PlaceName,
		AssetIDs:      parseAssetIDs(b.AssetIDs),
		Visibility:    b.Visibility,
		AllowComments: b.AllowComments,
		AllowLikes:    b.AllowLikes,
	}
	var err error
	if in.AllowedUserIDs, err = parseIDList(b.AllowedUserIDs); err != nil {
		return nil, apperrors.Invalid("allowed_user_ids must be int64 strings")
	}
	if in.ExcludedUserIDs, err = parseIDList(b.ExcludedUserIDs); err != nil {
		return nil, apperrors.Invalid("excluded_user_ids must be int64 strings")
	}
	if b.TagID != "" {
		if in.TagID, err = strconv.ParseInt(b.TagID, 10, 64); err != nil {
			return nil, apperrors.Invalid("tag_id must be int64 string")
		}
	}
	return in, nil
}

func parseIDList(raw []string) ([]int64, error) {
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// Publish handles POST /api/v1/moments (C1).
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body publishBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in, err := body.toInput(p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if body.RunAt != "" {
		runAt, err := time.Parse(time.RFC3339, body.RunAt)
		if err != nil {
			httpx.Error(w, r, apperrors.Invalid("run_at must be RFC3339"))
			return
		}
		id, err := h.svc.CreateSchedule(r.Context(), in, runAt)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.JSON(w, r, http.StatusOK, map[string]any{"schedule_id": strconv.FormatInt(id, 10)})
		return
	}
	id, err := h.svc.Publish(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"moment_id": strconv.FormatInt(id, 10)})
}

// Delete handles DELETE /api/v1/moments/{id} (C2).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Delete(r.Context(), mid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Feed handles GET /api/v1/moments/feed (C3).
func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	before := parseInt64Query(r, "before_id")
	limit := FeedPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= FeedPageSize*2 {
			limit = n
		}
	}
	items, err := h.svc.Feed(r.Context(), p.UserID, before, limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"moments": items})
}

// UserMoments handles GET /api/v1/users/{id}/moments (C4).
func (h *Handler) UserMoments(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	authorID, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	before := parseInt64Query(r, "before_id")
	albumOnly := r.URL.Query().Get("album") == "true"
	limit := FeedPageSize
	items, err := h.svc.UserMoments(r.Context(), authorID, p.UserID, before, limit, albumOnly)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"moments": items})
}

// Get handles GET /api/v1/moments/{id} (C5).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	m, err := h.svc.Get(r.Context(), p.UserID, mid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	assets, err := h.svc.Assets(r.Context(), p.UserID, mid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"moment": m,
		"assets": assets,
	})
}

// Like handles POST /api/v1/moments/{id}/like (C6).
func (h *Handler) Like(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Like(r.Context(), mid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Unlike handles DELETE /api/v1/moments/{id}/like (C7).
func (h *Handler) Unlike(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Unlike(r.Context(), mid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Likes handles GET /api/v1/moments/{id}/likes (C8).
func (h *Handler) Likes(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	users, err := h.svc.Likes(r.Context(), p.UserID, mid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"user_ids": users})
}

// AddComment handles POST /api/v1/moments/{id}/comments (C9).
func (h *Handler) AddComment(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Content string `json:"content"`
		ReplyTo string `json:"reply_to,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	var replyTo *int64
	if body.ReplyTo != "" {
		if n, err := strconv.ParseInt(body.ReplyTo, 10, 64); err == nil && n > 0 {
			replyTo = &n
		} else {
			httpx.Error(w, r, apperrors.Invalid("reply_to must be int64 string"))
			return
		}
	}
	c, err := h.svc.AddComment(r.Context(), mid, p.UserID, replyTo, body.Content)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"comment_id": strconv.FormatInt(c.ID, 10)})
}

// DeleteComment handles DELETE /api/v1/moment-comments/{id}. The path avoids
// an overlap with DELETE /api/v1/moments/{id}/like in Go's ServeMux.
func (h *Handler) DeleteComment(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	cid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteComment(r.Context(), cid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Comments handles GET /api/v1/moments/{id}/comments (C11).
func (h *Handler) Comments(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.svc.Comments(r.Context(), p.UserID, mid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"comments": items})
}

// Notifications handles GET /api/v1/moments/notifications (C12).
func (h *Handler) Notifications(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.svc.Notifications(r.Context(), p.UserID, 50)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"notifications": items})
}

// MarkNotificationsRead handles POST /api/v1/moments/notifications/read (C13).
func (h *Handler) MarkNotificationsRead(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
		All bool     `json:"all"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ids, err := parseIDList(body.IDs)
	if err != nil {
		httpx.Error(w, r, apperrors.Invalid("ids must be int64 strings"))
		return
	}
	if err := h.svc.MarkNotificationsRead(r.Context(), p.UserID, ids, body.All); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// ListSchedules handles GET /api/v1/moments/schedules (C14).
func (h *Handler) ListSchedules(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.svc.ListSchedules(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"schedules": items})
}

// CancelSchedule handles DELETE /api/v1/moments/schedules/{id} (C15).
func (h *Handler) CancelSchedule(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	sid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.CancelSchedule(r.Context(), sid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// RetrySchedule handles POST /api/v1/moments/schedules/{id}/retry (C16).
func (h *Handler) RetrySchedule(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	sid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.RetrySchedule(r.Context(), sid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Mount wires all C1-C16 routes. wrapWrite applies the write limiter, wrapRead
// applies the read limiter.
func Mount(mux *http.ServeMux, h *Handler, wrapWrite, wrapRead func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/moments", wrapWrite(h.Publish))
	mux.Handle("DELETE /api/v1/moments/{id}", wrapWrite(h.Delete))
	mux.Handle("GET /api/v1/moments/feed", wrapRead(h.Feed))
	mux.Handle("GET /api/v1/users/{id}/moments", wrapRead(h.UserMoments))
	mux.Handle("GET /api/v1/moments/{id}", wrapRead(h.Get))
	mux.Handle("POST /api/v1/moments/{id}/like", wrapWrite(h.Like))
	mux.Handle("DELETE /api/v1/moments/{id}/like", wrapWrite(h.Unlike))
	mux.Handle("GET /api/v1/moments/{id}/likes", wrapRead(h.Likes))
	mux.Handle("POST /api/v1/moments/{id}/comments", wrapWrite(h.AddComment))
	mux.Handle("DELETE /api/v1/moment-comments/{id}", wrapWrite(h.DeleteComment))
	mux.Handle("GET /api/v1/moments/{id}/comments", wrapRead(h.Comments))
	mux.Handle("GET /api/v1/moments/notifications", wrapRead(h.Notifications))
	mux.Handle("POST /api/v1/moments/notifications/read", wrapWrite(h.MarkNotificationsRead))
	mux.Handle("GET /api/v1/moments/schedules", wrapRead(h.ListSchedules))
	mux.Handle("DELETE /api/v1/moment-schedules/{id}", wrapWrite(h.CancelSchedule))
	mux.Handle("POST /api/v1/moments/schedules/{id}/retry", wrapWrite(h.RetrySchedule))
}
