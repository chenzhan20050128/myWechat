package user

import (
	"net/http"
	"strconv"

	"github.com/example/wechat/internal/platform/httpx"
	apperrors "github.com/example/wechat/internal/platform/errors"
)

// Handler exposes the user HTTP surface (SPEC-01 §4).
type Handler struct {
	svc *Service
}

// NewHandler builds the user handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ProfileView is the API shape (IDs as strings; sensitive fields only on /me).
type ProfileView struct {
	UserID         string  `json:"user_id"`
	Nickname       string  `json:"nickname"`
	AccountName    string  `json:"account_name"`
	Gender         string  `json:"gender,omitempty"`
	RegionCountry  string  `json:"region_country,omitempty"`
	RegionProvince string  `json:"region_province,omitempty"`
	RegionCity     string  `json:"region_city,omitempty"`
	Signature      string  `json:"signature,omitempty"`
	StatusText     string  `json:"status_text,omitempty"`
	AvatarMediaID  *string `json:"avatar_media_id,omitempty"`
}

func viewFromProfile(p Profile, accountName string, full bool) ProfileView {
	v := ProfileView{
		UserID:      strconv.FormatInt(p.UserID, 10),
		Nickname:    p.Nickname,
		AccountName: accountName,
	}
	if p.AvatarMediaID != nil {
		s := strconv.FormatInt(*p.AvatarMediaID, 10)
		v.AvatarMediaID = &s
	}
	if full {
		v.Gender = p.Gender
		v.RegionCountry = p.RegionCountry
		v.RegionProvince = p.RegionProvince
		v.RegionCity = p.RegionCity
		v.Signature = p.Signature
		v.StatusText = p.StatusText
	}
	return v
}

// GetMe handles GET /api/v1/users/me (R26).
func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prof, err := h.svc.GetProfile(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	acct, err := h.svc.FindAccountByID(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, viewFromProfile(prof, acct.AccountName, true))
}

// profileUpdateBody is the PATCH /api/v1/users/me body.
type profileUpdateBody struct {
	Nickname       *string `json:"nickname"`
	Gender         *string `json:"gender"`
	RegionCountry  *string `json:"region_country"`
	RegionProvince *string `json:"region_province"`
	RegionCity     *string `json:"region_city"`
	Signature      *string `json:"signature"`
	StatusText     *string `json:"status_text"`
}

// UpdateMe handles PATCH /api/v1/users/me (R26).
func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body profileUpdateBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	upd := ProfileUpdate(body)
	if err := h.svc.UpdateProfile(r.Context(), p.UserID, upd); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// avatarBody is the POST /api/v1/users/me/avatar body.
type avatarBody struct {
	MediaObjectID int64 `json:"media_object_id,string"`
}

// SetAvatar handles POST /api/v1/users/me/avatar (R27).
func (h *Handler) SetAvatar(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body avatarBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if body.MediaObjectID <= 0 {
		httpx.Error(w, r, apperrors.Invalid("media_object_id required"))
		return
	}
	if err := h.svc.SetAvatar(r.Context(), p.UserID, body.MediaObjectID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// GetUser handles GET /api/v1/users/{id} (R28: limited fields for non-self).
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Error(w, r, apperrors.Invalid("invalid user id"))
		return
	}
	prof, err := h.svc.GetProfile(r.Context(), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	acct, err := h.svc.FindAccountByID(r.Context(), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// full view only for the account owner; everyone else gets the
	// non-sensitive subset (contract §4.5)
	p, _ := httpx.PrincipalFrom(r.Context())
	full := p.UserID == id
	httpx.JSON(w, r, http.StatusOK, viewFromProfile(prof, acct.AccountName, full))
}
