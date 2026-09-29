package favorite

import (
	"encoding/json"
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler wires the favorite HTTP surface (D1-D12).
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func pathInt64(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, apperrors.Invalid("invalid " + name)
	}
	return v, nil
}

// Create handles POST /api/v1/favorites (D1).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Kind            string          `json:"kind"`
		Content         json.RawMessage `json:"content"`
		SourceMessageID string          `json:"source_message_id,omitempty"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := &CreateInput{OwnerID: p.UserID, Kind: body.Kind, Content: body.Content}
	if body.SourceMessageID != "" {
		id, err := strconv.ParseInt(body.SourceMessageID, 10, 64)
		if err != nil {
			httpx.Error(w, r, apperrors.Invalid("source_message_id must be int64 string"))
			return
		}
		in.SourceMessageID = &id
	}
	fid, duplicate, err := h.svc.Create(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"favorite_id": strconv.FormatInt(fid, 10),
		"duplicate":   duplicate,
	})
}

// List handles GET /api/v1/favorites (D2).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	var before int64
	if v := q.Get("before_id"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	limit := DefaultPageSize
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= MaxPageSize {
			limit = n
		}
	}
	items, err := h.svc.List(r.Context(), p.UserID, before, q.Get("kind"), q.Get("tag"), limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"favorites": items})
}

// Get handles GET /api/v1/favorites/{id} (D3).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f, err := h.svc.Get(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, f)
}

// Delete handles DELETE /api/v1/favorites/{id} (D4).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Delete(r.Context(), p.UserID, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// ApplyTags handles POST /api/v1/favorites/{id}/tags (D5).
func (h *Handler) ApplyTags(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.ApplyTags(r.Context(), p.UserID, id, body.Tags); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// ListTags handles GET /api/v1/favorite-tags (D6).
func (h *Handler) ListTags(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tags, err := h.svc.ListTags(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"tags": tags})
}

// CreateTag handles POST /api/v1/favorite-tags (D7).
func (h *Handler) CreateTag(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.CreateTag(r.Context(), p.UserID, body.Tag); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// PreviewCleanup handles POST /api/v1/storage/cleanup/previews (D9).
func (h *Handler) PreviewCleanup(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.Preview(r.Context(), &PreviewInput{UserID: p.UserID, Scope: body.Scope})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, res)
}

// ConfirmCleanup handles POST /api/v1/storage/cleanup/{id}/confirm (D10).
func (h *Handler) ConfirmCleanup(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	success, failed, err := h.svc.Confirm(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"processed": success, "failed": failed})
}

// GetCleanup handles GET /api/v1/storage/cleanup/{id} (D11).
func (h *Handler) GetCleanup(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	job, items, err := h.svc.Job(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"job": job, "items": items})
}

// Mount wires D1-D11.
func Mount(mux *http.ServeMux, h *Handler, wrapWrite, wrapRead func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/favorites", wrapWrite(h.Create))
	mux.Handle("GET /api/v1/favorites", wrapRead(h.List))
	mux.Handle("GET /api/v1/favorites/{id}", wrapRead(h.Get))
	mux.Handle("DELETE /api/v1/favorites/{id}", wrapWrite(h.Delete))
	mux.Handle("POST /api/v1/favorites/{id}/tags", wrapWrite(h.ApplyTags))
	mux.Handle("GET /api/v1/favorite-tags", wrapRead(h.ListTags))
	mux.Handle("POST /api/v1/favorite-tags", wrapWrite(h.CreateTag))
	mux.Handle("POST /api/v1/storage/cleanup/previews", wrapWrite(h.PreviewCleanup))
	mux.Handle("POST /api/v1/storage/cleanup/{id}/confirm", wrapWrite(h.ConfirmCleanup))
	mux.Handle("GET /api/v1/storage/cleanup/{id}", wrapRead(h.GetCleanup))
}
