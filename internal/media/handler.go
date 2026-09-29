package media

import (
	"io"
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler exposes the media HTTP surface (SPEC-03 §4).
type Handler struct {
	svc *Service
}

// NewHandler builds the media handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func me(r *http.Request) (int64, error) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return 0, err
	}
	return p.UserID, nil
}

// CreateUpload handles POST /api/v1/media/uploads (R1-R3).
func (h *Handler) CreateUpload(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body createUploadBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.svc.CreateSession(r.Context(), userID, CreateInput{
		FileName: body.FileName, Size: body.Size, MIME: body.MIME, SHA256: body.SHA256, Purpose: body.Purpose,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, viewFromSession(row))
}

// GetUpload handles GET /api/v1/media/uploads/{id} (R5).
func (h *Handler) GetUpload(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, bitmap, err := h.svc.GetSession(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if bitmap == nil {
		bitmap = []int{}
	}
	httpx.JSON(w, r, http.StatusOK, uploadStatusView{uploadView: viewFromSession(row), ReceivedChunks: bitmap})
}

// PutChunk handles PUT /api/v1/media/uploads/{id}/chunks/{n} (R4). The body is
// the raw part payload, not JSON.
func (h *Handler) PutChunk(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		httpx.Error(w, r, apperrors.Invalid("invalid chunk index"))
		return
	}
	if err := h.svc.PutChunk(r.Context(), userID, r.PathValue("id"), n, r.Body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CompleteUpload handles POST /api/v1/media/uploads/{id}/complete (R6).
func (h *Handler) CompleteUpload(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	objectID, err := h.svc.CompleteUpload(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, viewFromObjectID(objectID))
}

// AbortUpload handles POST /api/v1/media/uploads/{id}/abort (§4).
func (h *Handler) AbortUpload(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.AbortUpload(r.Context(), userID, r.PathValue("id")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// DownloadURL handles GET /api/v1/media/objects/{id}/download-url (R14).
func (h *Handler) DownloadURL(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	objectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || objectID <= 0 {
		httpx.Error(w, r, apperrors.Invalid("invalid object id"))
		return
	}
	url, err := h.svc.DownloadURL(r.Context(), userID, objectID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, downloadURLView{URL: url, ExpiresIn: int64(h.svc.ttl.Seconds())})
}

// Download handles GET /api/v1/media/download?token=... (R16): the local
// driver's byte proxy. Signature or expiry failure is FORBIDDEN.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	body, obj, err := h.svc.ProxyDownload(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", obj.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}
