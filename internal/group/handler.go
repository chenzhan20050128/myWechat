// Package group HTTP handler — mounts B1-B23 routes (SPEC-05 §5).
package group

import (
	"net/http"
	"strconv"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
)

// Handler wires the group HTTP surface.
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

// CreateGroup handles POST /api/v1/groups (B1).
func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Name         string `json:"name"`
		AvatarMediaID string `json:"avatar_media_id"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	var avatarID int64
	if body.AvatarMediaID != "" {
		if v, err := strconv.ParseInt(body.AvatarMediaID, 10, 64); err == nil {
			avatarID = v
		}
	}
	id, err := h.svc.CreateGroup(r.Context(), p.UserID, CreateInput{Name: body.Name, AvatarMediaID: avatarID})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]string{"group_id": strconv.FormatInt(id, 10)})
}

// GetGroup handles GET /api/v1/groups/{id} (B5).
func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	g, role, err := h.svc.GetGroup(r.Context(), gid, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, toGroupViewRow(g, role))
}

// RenameGroup handles PATCH /api/v1/groups/{id} (B2).
func (h *Handler) RenameGroup(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Rename(r.Context(), gid, p.UserID, body.Name); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// SetGroupAvatar handles PUT /api/v1/groups/{id}/avatar (B3).
func (h *Handler) SetGroupAvatar(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		MediaObjectID string `json:"media_object_id"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	mid, err := strconv.ParseInt(body.MediaObjectID, 10, 64)
	if err != nil || mid <= 0 {
		httpx.Error(w, r, apperrors.Invalid("media_object_id must be a positive integer"))
		return
	}
	if err := h.svc.SetAvatar(r.Context(), gid, p.UserID, mid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// SetGroupAnnouncement handles PUT /api/v1/groups/{id}/announcement (B4).
func (h *Handler) SetGroupAnnouncement(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.SetAnnouncement(r.Context(), gid, p.UserID, body.Content); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// ListGroupMembers handles GET /api/v1/groups/{id}/members (B6).
func (h *Handler) ListGroupMembers(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	members, err := h.svc.ListMembers(r.Context(), gid, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"members": members})
}

// InviteMembers handles POST /api/v1/groups/{id}/members (B7).
func (h *Handler) InviteMembers(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		UserIDs []int64 `json:"user_ids"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	added, skipped, err := h.svc.Invite(r.Context(), gid, p.UserID, body.UserIDs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"added": added, "skipped": skipped})
}

// JoinByInviteCode handles POST /api/v1/groups/join (B8).
func (h *Handler) JoinByInviteCode(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	var body struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := h.svc.Join(r.Context(), p.UserID, body.Code)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]string{"group_id": strconv.FormatInt(id, 10)})
}

// KickMember handles DELETE /api/v1/groups/{id}/members/{uid} (B9).
func (h *Handler) KickMember(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	uid, err := pathInt64(r, "uid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Kick(r.Context(), gid, p.UserID, uid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// QuitGroup handles POST /api/v1/groups/{id}/quit (B10).
func (h *Handler) QuitGroup(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Quit(r.Context(), gid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// TransferGroup handles POST /api/v1/groups/{id}/transfer (B11).
func (h *Handler) TransferGroup(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		UserID int64 `json:"user_id"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Transfer(r.Context(), gid, p.UserID, body.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// DissolveGroup handles POST /api/v1/groups/{id}/dissolve (B12).
func (h *Handler) DissolveGroup(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Dissolve(r.Context(), gid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// PromoteAdmin handles POST /api/v1/groups/{id}/admins (B13).
func (h *Handler) PromoteAdmin(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		UserID int64 `json:"user_id"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.PromoteAdmin(r.Context(), gid, p.UserID, body.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// DemoteAdmin handles DELETE /api/v1/groups/{id}/admins/{uid} (B14).
func (h *Handler) DemoteAdmin(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	uid, err := pathInt64(r, "uid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DemoteAdmin(r.Context(), gid, p.UserID, uid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// SetMute handles POST /api/v1/groups/{id}/mutes (B15).
func (h *Handler) SetMute(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		UserID   int64  `json:"user_id"`
		Duration string `json:"duration"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.SetMute(r.Context(), gid, p.UserID, body.UserID, body.Duration); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// Unmute handles DELETE /api/v1/groups/{id}/mutes/{uid} (B16).
func (h *Handler) Unmute(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	uid, err := pathInt64(r, "uid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Unmute(r.Context(), gid, p.UserID, uid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// CreateInviteCode handles POST /api/v1/groups/{id}/invite-codes (B17).
func (h *Handler) CreateInviteCode(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	code, expires, err := h.svc.NewInviteCode(r.Context(), gid, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"code": code, "expires_at": expires})
}

// CreateTodo handles POST /api/v1/groups/{id}/todos (B18).
func (h *Handler) CreateTodo(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		DueAt       *time.Time `json:"due_at"`
		AssigneeIDs []int64   `json:"assignee_ids"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := h.svc.CreateTodo(r.Context(), gid, p.UserID, CreateTodoInput{
		Title: body.Title, Description: body.Description, DueAt: body.DueAt, AssigneeIDs: body.AssigneeIDs,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]string{"todo_id": strconv.FormatInt(id, 10)})
}

// ListTodos handles GET /api/v1/groups/{id}/todos (B22).
func (h *Handler) ListTodos(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	gid, err := pathInt64(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	todos, err := h.svc.ListTodos(r.Context(), gid, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"todos": todos})
}

// GetTodo handles GET /api/v1/groups/{id}/todos/{tid} (B23).
func (h *Handler) GetTodo(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	tid, err := pathInt64(r, "tid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	view, err := h.svc.GetTodo(r.Context(), tid, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, view)
}

// CompleteTodo handles POST /api/v1/groups/{id}/todos/{tid}/complete (B20).
func (h *Handler) CompleteTodo(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	tid, err := pathInt64(r, "tid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.CompleteTodo(r.Context(), tid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// CancelTodo handles DELETE /api/v1/groups/{id}/todos/{tid} (B21).
func (h *Handler) CancelTodo(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	tid, err := pathInt64(r, "tid")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.CancelTodo(r.Context(), tid, p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{})
}

// ListMyGroups handles GET /api/v1/users/me/groups.
func (h *Handler) ListMyGroups(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.MustPrincipal(r.Context())
	groups, err := h.svc.ListMyGroups(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"groups": groups})
}

// groupViewRow is the JSON shape for GET /groups/{id}.
type groupViewRow struct {
	GroupID      string    `json:"group_id"`
	Name         string    `json:"name"`
	OwnerID      string    `json:"owner_id"`
	Role         string    `json:"role"`
	Announcement string    `json:"announcement"`
	MemberCount  int       `json:"member_count"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

func toGroupViewRow(g GroupRow, role string) groupViewRow {
	return groupViewRow{
		GroupID:      strconv.FormatInt(g.ID, 10),
		Name:         g.Name,
		OwnerID:      strconv.FormatInt(g.OwnerID, 10),
		Role:         role,
		Announcement: g.Announcement,
		MemberCount:  g.MemberCount,
		Status:       g.Status,
		CreatedAt:    g.CreatedAt.UTC(),
	}
}

// Mount registers all group routes on the mux.
func Mount(mux *http.ServeMux, h *Handler, wrap func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/v1/groups", wrap(h.CreateGroup))
	mux.Handle("GET /api/v1/groups/{id}", wrap(h.GetGroup))
	mux.Handle("PATCH /api/v1/groups/{id}", wrap(h.RenameGroup))
	mux.Handle("PUT /api/v1/groups/{id}/avatar", wrap(h.SetGroupAvatar))
	mux.Handle("PUT /api/v1/groups/{id}/announcement", wrap(h.SetGroupAnnouncement))
	mux.Handle("GET /api/v1/groups/{id}/members", wrap(h.ListGroupMembers))
	mux.Handle("POST /api/v1/groups/{id}/members", wrap(h.InviteMembers))
	mux.Handle("POST /api/v1/groups/join", wrap(h.JoinByInviteCode))
	mux.Handle("DELETE /api/v1/groups/{id}/members/{uid}", wrap(h.KickMember))
	mux.Handle("POST /api/v1/groups/{id}/quit", wrap(h.QuitGroup))
	mux.Handle("POST /api/v1/groups/{id}/transfer", wrap(h.TransferGroup))
	mux.Handle("POST /api/v1/groups/{id}/dissolve", wrap(h.DissolveGroup))
	mux.Handle("POST /api/v1/groups/{id}/admins", wrap(h.PromoteAdmin))
	mux.Handle("DELETE /api/v1/groups/{id}/admins/{uid}", wrap(h.DemoteAdmin))
	mux.Handle("POST /api/v1/groups/{id}/mutes", wrap(h.SetMute))
	mux.Handle("DELETE /api/v1/groups/{id}/mutes/{uid}", wrap(h.Unmute))
	mux.Handle("POST /api/v1/groups/{id}/invite-codes", wrap(h.CreateInviteCode))
	mux.Handle("POST /api/v1/groups/{id}/todos", wrap(h.CreateTodo))
	mux.Handle("GET /api/v1/groups/{id}/todos", wrap(h.ListTodos))
	mux.Handle("GET /api/v1/groups/{id}/todos/{tid}", wrap(h.GetTodo))
	mux.Handle("POST /api/v1/groups/{id}/todos/{tid}/complete", wrap(h.CompleteTodo))
	mux.Handle("DELETE /api/v1/groups/{id}/todos/{tid}", wrap(h.CancelTodo))
	mux.Handle("GET /api/v1/users/me/groups", wrap(h.ListMyGroups))
}
