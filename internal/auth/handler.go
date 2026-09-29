package auth

import (
	"net/http"

	"github.com/example/wechat/internal/platform/httpx"
)

// Handler exposes the auth HTTP surface (SPEC-01 §4).
type Handler struct {
	svc *Service
}

// NewHandler builds the auth handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register handles POST /api/v1/auth/register.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	pair, err := h.svc.Register(r.Context(), req, httpx.ClientIP(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, pair)
}

// Login handles POST /api/v1/auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	pair, err := h.svc.Login(r.Context(), req, httpx.ClientIP(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, pair)
}

// Refresh handles POST /api/v1/auth/refresh.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken, httpx.ClientIP(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, pair)
}

// Logout handles POST /api/v1/auth/logout.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Logout(r.Context(), p, httpx.ClientIP(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// ChangePassword handles POST /api/v1/auth/password.
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req ChangePasswordRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.ChangePassword(r.Context(), p, req.OldPassword, req.NewPassword, httpx.ClientIP(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}
