package backup

import (
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler wires the backup HTTP surface (E1-E12).
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

// Request handles POST /api/v1/backups (E1).
func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	b, err := h.svc.Request(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"backup_id":    strconv.FormatInt(b.ID, 10),
		"status":       b.Status,
		"total_size":   b.TotalSize,
		"created_at":    b.CreatedAt,
	})
}

// Current handles GET /api/v1/backups/current (E2).
func (h *Handler) Current(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	b, err := h.svc.Current(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, b)
}

// DeleteCurrent handles DELETE /api/v1/backups/current (E3).
func (h *Handler) DeleteCurrent(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteCurrent(r.Context(), p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Restore handles POST /api/v1/backups/current/restore (E4).
func (h *Handler) Restore(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	jobID, err := h.svc.Restore(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"restore_job_id": strconv.FormatInt(jobID, 10)})
}

// RestoreJobs handles GET /api/v1/restore-jobs (E5).
func (h *Handler) RestoreJobs(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	jobs, err := h.svc.RestoreJobs(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"jobs": jobs})
}

// ProfileArchive handles GET /api/v1/restored-archives/{job_id}/profile (E6).
func (h *Handler) ProfileArchive(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	jobID, err := pathInt64(r, "job_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prof, err := h.svc.ProfileArchive(r.Context(), p.UserID, jobID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, prof)
}

// DeleteArchive handles DELETE /api/v1/restored-archives/{job_id} (E8).
func (h *Handler) DeleteArchive(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	jobID, err := pathInt64(r, "job_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteArchive(r.Context(), p.UserID, jobID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// CreateHandshake handles POST /api/v1/transfer-handshakes (E9).
func (h *Handler) CreateHandshake(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, code, err := h.svc.CreateHandshake(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"handshake_id": strconv.FormatInt(id, 10),
		"code":         code,
	})
}

// AcceptHandshake handles POST /api/v1/transfer-handshakes/{id}/accept (E10).
func (h *Handler) AcceptHandshake(w http.ResponseWriter, r *http.Request) {
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
		Code string `json:"code"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.AcceptHandshake(r.Context(), p.UserID, id, body.Code); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// CompleteHandshake handles POST /api/v1/transfer-handshakes/{id}/complete (E11).
func (h *Handler) CompleteHandshake(w http.ResponseWriter, r *http.Request) {
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
	if err := h.svc.CompleteHandshake(r.Context(), p.UserID, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Mount wires E1-E11.
func Mount(mux *http.ServeMux, h *Handler, wrapWrite, wrapRead func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/backups", wrapWrite(h.Request))
	mux.Handle("GET /api/v1/backups/current", wrapRead(h.Current))
	mux.Handle("DELETE /api/v1/backups/current", wrapWrite(h.DeleteCurrent))
	mux.Handle("POST /api/v1/backups/current/restore", wrapWrite(h.Restore))
	mux.Handle("GET /api/v1/restore-jobs", wrapRead(h.RestoreJobs))
	mux.Handle("GET /api/v1/restored-archives/{job_id}/profile", wrapRead(h.ProfileArchive))
	mux.Handle("DELETE /api/v1/restored-archives/{job_id}", wrapWrite(h.DeleteArchive))
	mux.Handle("POST /api/v1/transfer-handshakes", wrapWrite(h.CreateHandshake))
	mux.Handle("POST /api/v1/transfer-handshakes/{id}/accept", wrapWrite(h.AcceptHandshake))
	mux.Handle("POST /api/v1/transfer-handshakes/{id}/complete", wrapWrite(h.CompleteHandshake))
}
