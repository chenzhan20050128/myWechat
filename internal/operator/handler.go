package operator

import (
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(s *Service) *Handler { return &Handler{svc: s} }

// SubmitReport handles POST /api/v1/reports (G1).
func (h *Handler) SubmitReport(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Reason     string `json:"reason"`
		Note       string `json:"note"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	targetID, err := strconv.ParseInt(body.TargetID, 10, 64)
	if err != nil || targetID <= 0 {
		httpx.Error(w, r, apperrors.Invalid("target_id must be int64 string"))
		return
	}
	id, err := h.svc.SubmitReport(r.Context(), p.UserID, body.TargetType, targetID, body.Reason, body.Note)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"report_id": strconv.FormatInt(id, 10)})
}

// ListReports handles GET /api/v1/admin/reports (G3).
func (h *Handler) ListReports(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var before int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	items, err := h.svc.ListReports(r.Context(), p.UserID, r.URL.Query().Get("status"), before, 50)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"reports": items})
}

// ResolveReport handles POST /api/v1/admin/reports/{id}/resolve (G4).
func (h *Handler) ResolveReport(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Error(w, r, apperrors.Invalid("invalid id"))
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.ResolveReport(r.Context(), p.UserID, id, body.Decision); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// QueueStats handles GET /api/v1/admin/stats/queues (G7).
func (h *Handler) QueueStats(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	stats, err := h.svc.QueueStats(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, stats)
}

// ListDeadLetters handles GET /api/v1/admin/dead-letters (G8).
func (h *Handler) ListDeadLetters(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.svc.ListDeadLetters(r.Context(), p.UserID, 50)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"dead_letters": items})
}

// ReplayDeadLetter handles POST /api/v1/admin/dead-letters/{event_id}/replay (G8).
func (h *Handler) ReplayDeadLetter(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	eventID := r.PathValue("event_id")
	consumer := r.URL.Query().Get("consumer")
	if consumer == "" {
		httpx.Error(w, r, apperrors.Invalid("consumer is required"))
		return
	}
	if err := h.svc.ReplayDeadLetter(r.Context(), p.UserID, eventID, consumer); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Mount registers G1-G8 routes.
func Mount(mux *http.ServeMux, h *Handler, wrapWrite, wrapRead func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/reports", wrapWrite(h.SubmitReport))
	mux.Handle("GET /api/v1/admin/reports", wrapRead(h.ListReports))
	mux.Handle("POST /api/v1/admin/reports/{id}/resolve", wrapWrite(h.ResolveReport))
	mux.Handle("GET /api/v1/admin/stats/queues", wrapRead(h.QueueStats))
	mux.Handle("GET /api/v1/admin/dead-letters", wrapRead(h.ListDeadLetters))
	mux.Handle("POST /api/v1/admin/dead-letters/{event_id}/replay", wrapWrite(h.ReplayDeadLetter))
}
