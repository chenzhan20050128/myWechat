package contact

import (
	"context"
	"net/http"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
	"github.com/example/wechat/internal/platform/pagination"
)

// Handler exposes the contact HTTP surface (SPEC-02 §4).
type Handler struct {
	svc *Service
}

// NewHandler builds the contact handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// pathID reads a positive numeric path parameter.
func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperrors.Invalid("invalid " + name)
	}
	return id, nil
}

// me extracts the caller id.
func me(r *http.Request) (int64, error) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return 0, err
	}
	return p.UserID, nil
}

// CreateRequest handles POST /api/v1/contacts/requests (R1-R7).
func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body createRequestBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.svc.CreateRequest(r.Context(), CreateRequestInput{
		ApplicantID: userID,
		Source:      body.Source,
		TargetID:    body.TargetID,
		Phone:       body.Phone,
		AccountName: body.AccountName,
		QRToken:     body.QRCodeToken,
		CardOwnerID: body.CardOwnerID,
		VerifyText:  body.VerifyText,
		IP:          httpx.ClientIP(r),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, requestView{ID: strconv.FormatInt(row.ID, 10), Status: row.Status})
}

// ListRequests handles GET /api/v1/contacts/requests (R10).
func (h *Handler) ListRequests(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	entries, next, err := h.svc.ListRequests(r.Context(), userID, q.Get("box"), q.Get("cursor"),
		pagination.QueryLimit(q.Get("limit")))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	views := make([]requestView, 0, len(entries))
	for _, e := range entries {
		views = append(views, viewFromRequest(e))
	}
	httpx.JSON(w, r, http.StatusOK, pagination.NewList(views, next))
}

// AcceptRequest handles POST /api/v1/contacts/requests/{id}/accept (R8).
func (h *Handler) AcceptRequest(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.AcceptRequest(r.Context(), userID, id, httpx.ClientIP(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := acceptView{Status: res.Status}
	if res.Epoch > 0 {
		out.Epoch = strconv.FormatInt(res.Epoch, 10)
	}
	if res.ConversationID > 0 {
		out.ConversationID = strconv.FormatInt(res.ConversationID, 10)
	}
	httpx.JSON(w, r, http.StatusOK, out)
}

// RejectRequest handles POST /api/v1/contacts/requests/{id}/reject (R9).
func (h *Handler) RejectRequest(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.svc.RejectRequest)
}

// CancelRequest handles POST /api/v1/contacts/requests/{id}/cancel (R9).
func (h *Handler) CancelRequest(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.svc.CancelRequest)
}

// decide runs the reject/cancel transition, which differ only in who may act.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request,
	apply func(ctx context.Context, me, requestID int64) error) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := apply(r.Context(), userID, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// DeleteFriend handles DELETE /api/v1/contacts/friends/{friendID} (R12).
func (h *Handler) DeleteFriend(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	friendID, err := pathID(r, "friendID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteFriend(r.Context(), userID, friendID, httpx.ClientIP(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// UpdateFriendSettings handles PATCH /api/v1/contacts/friends/{friendID}/settings (R15-R19).
func (h *Handler) UpdateFriendSettings(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	friendID, err := pathID(r, "friendID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body settingsPatchBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	st, err := h.svc.UpdateFriendSettings(r.Context(), userID, friendID, SettingsPatch{
		Remark:       body.Remark,
		MessagePerm:  body.MessagePerm,
		MomentPerm:   body.MomentPerm,
		MomentNotify: body.MomentNotify,
	}, httpx.ClientIP(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, settingsView{
		Remark:       st.Remark,
		MessagePerm:  st.MessagePerm,
		MomentPerm:   st.MomentPerm,
		MomentNotify: st.MomentNotify,
	})
}

// ListFriends handles GET /api/v1/contacts/friends (R25).
func (h *Handler) ListFriends(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	entries, next, err := h.svc.ListFriends(r.Context(), userID, q.Get("cursor"),
		pagination.QueryLimit(q.Get("limit")))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	views := make([]friendView, 0, len(entries))
	for _, e := range entries {
		views = append(views, viewFromFriend(e))
	}
	httpx.JSON(w, r, http.StatusOK, pagination.NewList(views, next))
}

// SearchFriends handles GET /api/v1/contacts/friends/search (R26).
func (h *Handler) SearchFriends(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	entries, next, err := h.svc.SearchFriends(r.Context(), userID, q.Get("q"), q.Get("cursor"),
		pagination.QueryLimit(q.Get("limit")))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	views := make([]friendView, 0, len(entries))
	for _, e := range entries {
		views = append(views, viewFromFriend(e))
	}
	httpx.JSON(w, r, http.StatusOK, pagination.NewList(views, next))
}

// Lookup handles GET /api/v1/contacts/lookup (R24).
func (h *Handler) Lookup(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	res, err := h.svc.Lookup(r.Context(), userID, q.Get("phone"), q.Get("account_name"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, lookupView{
		identityView: viewFromIdentity(res.Identity),
		IsFriend:     res.IsFriend,
	})
}

// QRCode handles GET /api/v1/contacts/qrcode (R1).
func (h *Handler) QRCode(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	token, exp, err := h.svc.IssueQRCode(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, qrcodeView{QRCodeToken: token, ExpiresAt: exp})
}

// CreateTag handles POST /api/v1/contacts/tags (R20).
func (h *Handler) CreateTag(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body tagBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	tag, err := h.svc.CreateTag(r.Context(), userID, body.Name)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, viewFromTag(tag))
}

// ListTags handles GET /api/v1/contacts/tags (R20).
func (h *Handler) ListTags(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tags, err := h.svc.ListTags(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	views := make([]tagView, 0, len(tags))
	for _, t := range tags {
		views = append(views, viewFromTag(t))
	}
	httpx.JSON(w, r, http.StatusOK, pagination.NewList(views, ""))
}

// RenameTag handles PATCH /api/v1/contacts/tags/{id} (R20).
func (h *Handler) RenameTag(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body tagBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.RenameTag(r.Context(), userID, id, body.Name); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// DeleteTag handles DELETE /api/v1/contacts/tags/{id} (R22).
func (h *Handler) DeleteTag(w http.ResponseWriter, r *http.Request) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteTag(r.Context(), userID, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}

// AddTagMembers handles POST /api/v1/contacts/tags/{id}/members (R21).
func (h *Handler) AddTagMembers(w http.ResponseWriter, r *http.Request) {
	h.tagMembers(w, r, h.svc.AddTagMembers)
}

// RemoveTagMembers handles DELETE /api/v1/contacts/tags/{id}/members (R21).
func (h *Handler) RemoveTagMembers(w http.ResponseWriter, r *http.Request) {
	h.tagMembers(w, r, h.svc.RemoveTagMembers)
}

func (h *Handler) tagMembers(w http.ResponseWriter, r *http.Request,
	apply func(ctx context.Context, me, tagID int64, friendIDs []int64) error) {
	userID, err := me(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body tagMembersBody
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := apply(r.Context(), userID, id, body.FriendIDs); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, nil)
}
