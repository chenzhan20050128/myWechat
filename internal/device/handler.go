package device

import (
	"net/http"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler exposes the device HTTP surface (SPEC-01 §4).
type Handler struct {
	svc *Service
}

// NewHandler builds the device handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// DeviceView is the API shape for one device row (IDs as strings, ADR-005).
type DeviceView struct {
	DeviceID     string `json:"device_id"`
	DeviceName   string `json:"device_name"`
	Platform     string `json:"platform"`
	FirstLoginAt string `json:"first_login_at"`
	LastActiveAt string `json:"last_active_at"`
	LastIP       string `json:"last_ip"`
	Current      bool   `json:"current"`
}

// List handles GET /api/v1/devices (R21).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	devices, err := h.svc.ListDevices(r.Context(), p.UserID, p.DeviceID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]DeviceView, 0, len(devices))
	for _, d := range devices {
		out = append(out, DeviceView{
			DeviceID: d.ID, DeviceName: d.Name, Platform: d.Platform,
			FirstLoginAt: d.FirstLoginAt.Format(timeFormat), LastActiveAt: d.LastActiveAt.Format(timeFormat),
			LastIP: d.LastIP, Current: d.Current,
		})
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"devices": out})
}

// Revoke handles DELETE /api/v1/devices/{deviceID} (R22).
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	deviceID := r.PathValue("deviceID")
	if deviceID == "" {
		httpx.Error(w, r, apperrors.Invalid("device_id required"))
		return
	}
	if deviceID == p.DeviceID {
		// current device must use /auth/logout (contract §3.3 "不能误退出自身")
		httpx.Error(w, r, apperrors.Invalid("cannot revoke the current device; use logout"))
		return
	}
	n, err := h.svc.RevokeDevice(r.Context(), p.UserID, deviceID, "user_revoked")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n == 0 {
		httpx.Error(w, r, apperrors.Unavail("device not found or already revoked"))
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"revoked_sessions": n})
}

// LogoutOthers handles POST /api/v1/devices/logout-others (R23).
func (h *Handler) LogoutOthers(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	n, err := h.svc.RevokeOthers(r.Context(), p.UserID, p.DeviceID, "user_revoked_others")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"revoked_sessions": n})
}

const timeFormat = time.RFC3339 // contract §15.1: API times are RFC3339
