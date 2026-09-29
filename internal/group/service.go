// Package group service implements the R1-R26 rules (SPEC-05 §4).
package group

import (
	"context"
	"database/sql"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/ids"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// SystemMessenger is the port the message module implements (SPEC-05 §2).
// group writes only the payload; message owns seq assignment and storage.
type SystemMessenger interface {
	SendSystemTx(ctx context.Context, tx mysqlx.Tx, conversationID int64, event string, detail map[string]any) error
}

// Conversation is the port group needs to create the group conversation row.
type Conversation interface {
	CreateGroupTx(ctx context.Context, tx mysqlx.Tx, creatorID int64) (int64, error)
}

// Friend is the port group needs to validate invitees are active friends (R9).
type Friend interface {
	GetActiveFriendship(ctx context.Context, a, b int64) (int64, bool, error)
}

// Service owns group state.
type Service struct {
	db     *sql.DB
	conv   Conversation
	friend Friend
	msg    SystemMessenger
	now    func() time.Time
}

// New wires the group service.
func New(db *sql.DB, conv Conversation, friend Friend, msg SystemMessenger, now func() time.Time) *Service {
	return &Service{db: db, conv: conv, friend: friend, msg: msg, now: now}
}

// CreateInput is the POST /groups body.
type CreateInput struct {
	Name           string
	AvatarMediaID  int64
}

// CreateGroup builds a group (R1).
func (s *Service) CreateGroup(ctx context.Context, creator int64, in CreateInput) (int64, error) {
	if err := validateGroupName(in.Name); err != nil {
		return 0, err
	}
	var groupID int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		now := s.now()
		convID, err := s.conv.CreateGroupTx(ctx, tx, creator)
		if err != nil {
			return err
		}
		id, err := insertGroup(ctx, tx, GroupRow{
			Name: in.Name, OwnerID: creator, ConversationID: convID, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		groupID = id
		if _, err := insertMember(ctx, tx, id, creator, RoleOwner, now); err != nil {
			return err
		}
		_ = s.system(ctx, tx, convID, "group.created", map[string]any{"group_id": id})
		return insertGroupEvent(ctx, tx, id, "group.created", creator, 0, map[string]any{}, now)
	})
	if err != nil {
		return 0, err
	}
	return groupID, nil
}

// GetGroup returns the group plus the caller's role (R5 detail).
func (s *Service) GetGroup(ctx context.Context, groupID, userID int64) (GroupRow, string, error) {
	g, err := findGroup(ctx, s.db, groupID)
	if err != nil {
		return GroupRow{}, "", err
	}
	_, role, err := activeMember(ctx, s.db, groupID, userID)
	if err != nil {
		return GroupRow{}, "", err
	}
	if role == "" {
		return GroupRow{}, "", apperrors.New(apperrors.Forbidden, "not a member")
	}
	return g, role, nil
}

// requireGroupForWrite locks the group row, enforces active status, and returns it.
func (s *Service) requireGroupForWrite(ctx context.Context, tx mysqlx.Tx, groupID int64) (GroupRow, error) {
	g, err := lockGroup(ctx, tx, groupID)
	if err != nil {
		return GroupRow{}, err
	}
	if g.Status != StatusActive {
		return GroupRow{}, apperrors.New(apperrors.StateConflict, "group is dissolved")
	}
	return g, nil
}

// requireRole locks the group and checks role in {owner, admin} (R5/R8).
func (s *Service) requireRole(ctx context.Context, tx mysqlx.Tx, groupID, userID int64, ownerOnly bool) (GroupRow, MemberRow, error) {
	g, err := s.requireGroupForWrite(ctx, tx, groupID)
	if err != nil {
		return GroupRow{}, MemberRow{}, err
	}
	m, role, err := activeMember(ctx, tx, groupID, userID)
	if err != nil {
		return GroupRow{}, MemberRow{}, err
	}
	if role == "" {
		return GroupRow{}, MemberRow{}, apperrors.New(apperrors.Forbidden, "not a member")
	}
	if ownerOnly && role != RoleOwner {
		return GroupRow{}, MemberRow{}, apperrors.New(apperrors.Forbidden, "owner only")
	}
	if !ownerOnly && role == RoleMember {
		return GroupRow{}, MemberRow{}, apperrors.New(apperrors.Forbidden, "owner or admin only")
	}
	return g, m, nil
}

// Rename updates the group name (B2).
func (s *Service) Rename(ctx context.Context, groupID, userID int64, name string) error {
	if err := validateGroupName(name); err != nil {
		return err
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, _, err := s.requireRole(ctx, tx, groupID, userID, false); err != nil {
			return err
		}
		if err := updateGroupName(ctx, tx, groupID, name); err != nil {
			return err
		}
		return insertGroupEvent(ctx, tx, groupID, "announcement.changed", userID, 0, map[string]any{"name": name}, s.now())
	})
}

// SetAvatar binds a media object to the group (B3).
func (s *Service) SetAvatar(ctx context.Context, groupID, userID, mediaID int64) error {
	if mediaID <= 0 {
		return apperrors.Invalid("media_object_id required")
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, _, err := s.requireRole(ctx, tx, groupID, userID, false); err != nil {
			return err
		}
		return updateGroupAvatar(ctx, tx, groupID, mediaID)
	})
}

// SetAnnouncement (B4).
func (s *Service) SetAnnouncement(ctx context.Context, groupID, userID int64, content string) error {
	if len([]rune(content)) > MaxAnnouncement {
		return apperrors.Invalid("announcement too long")
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, userID, false)
		if err != nil {
			return err
		}
		now := s.now()
		if err := updateAnnouncement(ctx, tx, groupID, content, userID, now); err != nil {
			return err
		}
		_ = s.system(ctx, tx, g.ConversationID, "announcement.changed", map[string]any{"by": userID})
		return insertGroupEvent(ctx, tx, groupID, "announcement.changed", userID, 0, nil, now)
	})
}

// Invite adds a batch of invitees (R9, B7). Returns added + skipped ids.
func (s *Service) Invite(ctx context.Context, groupID, inviter int64, invitees []int64) (added, skipped []int64, err error) {
	if len(invitees) > MaxInviteBatch {
		return nil, nil, apperrors.Invalid("at most 50 invitees per request")
	}
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, inviter, false)
		if err != nil {
			return err
		}
		added = nil
		skipped = nil
		for _, uid := range invitees {
			if uid == inviter {
				skipped = append(skipped, uid)
				continue
			}
			if _, role, err := activeMember(ctx, tx, groupID, uid); err == nil && role != "" {
				skipped = append(skipped, uid)
				continue
			}
			if s.friend != nil {
				_, isFriend, err := s.friend.GetActiveFriendship(ctx, inviter, uid)
				if err != nil || !isFriend {
					skipped = append(skipped, uid)
					continue
				}
			}
			if g.MemberCount+len(added) > MaxGroupSize {
				return apperrors.New(apperrors.QuotaExceeded, "group is full")
			}
			ins, err := insertMember(ctx, tx, groupID, uid, RoleMember, s.now())
			if err != nil {
				return err
			}
			if ins {
				added = append(added, uid)
			} else {
				skipped = append(skipped, uid)
			}
		}
		if len(added) > 0 {
			if err := bumpMemberCount(ctx, tx, groupID, len(added)); err != nil {
				return err
			}
			_ = s.system(ctx, tx, g.ConversationID, "member.joined", map[string]any{
				"by": inviter, "members": added,
			})
			for _, uid := range added {
				_ = insertGroupEvent(ctx, tx, groupID, "member.joined", inviter, uid, nil, s.now())
			}
		}
		return nil
	})
	return added, skipped, err
}

// Join uses an invite code (R10, B8).
func (s *Service) Join(ctx context.Context, userID int64, code string) (int64, error) {
	var groupID int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		now := s.now()
		ic, err := lockInviteCode(ctx, tx, code)
		if err != nil {
			return err
		}
		if now.After(ic.ExpiresAt) || ic.UseCount >= ic.MaxUses {
			return apperrors.Unavail("invite code expired or exhausted")
		}
		g, err := lockGroup(ctx, tx, ic.GroupID)
		if err != nil {
			return err
		}
		if g.Status != StatusActive {
			return apperrors.New(apperrors.StateConflict, "group is dissolved")
		}
		// Idempotent: already-active member → success.
		if _, role, _ := activeMember(ctx, tx, g.ID, userID); role != "" {
			groupID = g.ID
			return nil
		}
		recent, err := recentlyRemoved(ctx, tx, g.ID, userID, BanRejoinWindow, now)
		if err != nil {
			return err
		}
		if recent {
			return apperrors.New(apperrors.StateConflict, "cannot rejoin yet")
		}
		if g.MemberCount >= MaxGroupSize {
			return apperrors.New(apperrors.QuotaExceeded, "group is full")
		}
		if _, err := insertMember(ctx, tx, g.ID, userID, RoleMember, now); err != nil {
			return err
		}
		if _, err := consumeInviteCode(ctx, tx, ic.ID); err != nil {
			return err
		}
		if err := recordInviteUse(ctx, tx, ic.ID, userID, now); err != nil {
			return err
		}
		if err := bumpMemberCount(ctx, tx, g.ID, 1); err != nil {
			return err
		}
		_ = s.system(ctx, tx, g.ConversationID, "member.joined", map[string]any{"by": userID, "members": []int64{userID}})
		_ = insertGroupEvent(ctx, tx, g.ID, "member.joined", userID, userID, nil, now)
		groupID = g.ID
		return nil
	})
	return groupID, err
}

// Quit (R12, B10).
func (s *Service) Quit(ctx context.Context, groupID, userID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, err := s.requireGroupForWrite(ctx, tx, groupID)
		if err != nil {
			return err
		}
		m, role, err := activeMember(ctx, tx, groupID, userID)
		if err != nil {
			return err
		}
		if role == "" {
			return apperrors.New(apperrors.Forbidden, "not a member")
		}
		if m.Role == RoleOwner {
			return apperrors.New(apperrors.StateConflict, "owner cannot quit; transfer or dissolve")
		}
		if err := quitMember(ctx, tx, groupID, userID, s.now(), LeftQuit); err != nil {
			return err
		}
		if affected, err := waiveAssigneesOnLeave(ctx, tx, groupID, userID); err != nil {
			return err
		} else {
			now := s.now()
			for _, tid := range affected {
				if err := recomputeTodoStatus(ctx, tx, tid, now); err != nil {
					return err
				}
			}
		}
		_ = bumpMemberCount(ctx, tx, groupID, -1)
		_ = insertGroupEvent(ctx, tx, groupID, "member.quit", userID, userID, nil, s.now())
		_ = s.system(ctx, tx, g.ConversationID, "member.quit", map[string]any{"user_id": userID})
		return nil
	})
}

// Kick removes a member (R7, B9).
func (s *Service) Kick(ctx context.Context, groupID, actor, target int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, actor, false)
		if err != nil {
			return err
		}
		if actor == target {
			return apperrors.Invalid("cannot kick yourself")
		}
		tm, role, err := activeMember(ctx, tx, groupID, target)
		if err != nil || role == "" {
			return apperrors.Unavail("member not found")
		}
		if role == RoleOwner {
			return apperrors.New(apperrors.Forbidden, "cannot remove owner")
		}
		// R7: admins can only be removed by owner.
		if role == RoleAdmin {
			if _, myRole, _ := activeMember(ctx, tx, groupID, actor); myRole != RoleOwner {
				return apperrors.New(apperrors.Forbidden, "only owner can remove admins")
			}
		}
		if err := quitMember(ctx, tx, groupID, target, s.now(), LeftRemoved); err != nil {
			return err
		}
		if affected, err := waiveAssigneesOnLeave(ctx, tx, groupID, target); err != nil {
			return err
		} else {
			now := s.now()
			for _, tid := range affected {
				if err := recomputeTodoStatus(ctx, tx, tid, now); err != nil {
					return err
				}
			}
		}
		_ = bumpMemberCount(ctx, tx, groupID, -1)
		_ = insertGroupEvent(ctx, tx, groupID, "member.removed", actor, target, nil, s.now())
		_ = s.system(ctx, tx, g.ConversationID, "member.removed", map[string]any{"by": actor, "user_id": target})
		_ = tm
		return nil
	})
}

// Transfer (R4, B11).
func (s *Service) Transfer(ctx context.Context, groupID, actor, newOwner int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, actor, true)
		if err != nil {
			return err
		}
		if _, role, _ := activeMember(ctx, tx, groupID, newOwner); role == "" {
			return apperrors.Unavail("new owner is not a member")
		}
		if err := transferOwnership(ctx, tx, groupID, newOwner); err != nil {
			return err
		}
		_ = insertGroupEvent(ctx, tx, groupID, "owner.transferred", actor, newOwner, nil, s.now())
		_ = s.system(ctx, tx, g.ConversationID, "owner.transferred", map[string]any{"by": actor, "to": newOwner})
		return nil
	})
}

// Dissolve (R3, B12).
func (s *Service) Dissolve(ctx context.Context, groupID, actor int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, actor, true)
		if err != nil {
			return err
		}
		if err := dissolveGroup(ctx, tx, groupID, s.now()); err != nil {
			return err
		}
		_ = insertGroupEvent(ctx, tx, groupID, "dissolved", actor, 0, nil, s.now())
		_ = s.system(ctx, tx, g.ConversationID, "group.dissolved", map[string]any{"by": actor})
		return nil
	})
}

// PromoteAdmin (R6, B13).
func (s *Service) PromoteAdmin(ctx context.Context, groupID, actor, target int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, _, err := s.requireRole(ctx, tx, groupID, actor, true); err != nil {
			return err
		}
		_, role, err := activeMember(ctx, tx, groupID, target)
		if err != nil || role == "" {
			return apperrors.Unavail("member not found")
		}
		if role != RoleMember {
			return apperrors.Invalid("can only promote ordinary members")
		}
		n, err := countAdmins(ctx, tx, groupID)
		if err != nil {
			return err
		}
		if n >= MaxAdmins {
			return apperrors.New(apperrors.QuotaExceeded, "admin limit reached")
		}
		_, err = tx.ExecContext(ctx, `UPDATE group_members SET role = 'admin' WHERE group_id = ? AND user_id = ?`, groupID, target)
		return err
	})
}

// DemoteAdmin (B14).
func (s *Service) DemoteAdmin(ctx context.Context, groupID, actor, target int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, _, err := s.requireRole(ctx, tx, groupID, actor, true); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE group_members SET role = 'member' WHERE group_id = ? AND user_id = ? AND role = 'admin'`, groupID, target)
		return err
	})
}

// SetMute (R14, B15).
func (s *Service) SetMute(ctx context.Context, groupID, actor, target int64, duration string) error {
	until, err := muteUntil(duration, s.now())
	if err != nil {
		return err
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, actor, false)
		if err != nil {
			return err
		}
		_, role, err := activeMember(ctx, tx, groupID, target)
		if err != nil || role == "" {
			return apperrors.Unavail("member not found")
		}
		if role == RoleOwner {
			return apperrors.Invalid("cannot mute owner")
		}
		if role == RoleAdmin {
			if _, myRole, _ := activeMember(ctx, tx, groupID, actor); myRole != RoleOwner {
				return apperrors.New(apperrors.Forbidden, "only owner can mute admins")
			}
		}
		if err := upsertMute(ctx, tx, groupID, target, actor, until, s.now()); err != nil {
			return err
		}
		_ = insertGroupEvent(ctx, tx, groupID, "muted", actor, target, map[string]any{"duration": duration}, s.now())
		_ = s.system(ctx, tx, g.ConversationID, "member.muted", map[string]any{"by": actor, "user_id": target})
		return nil
	})
}

// Unmute (R17, B16).
func (s *Service) Unmute(ctx context.Context, groupID, actor, target int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		g, _, err := s.requireRole(ctx, tx, groupID, actor, false)
		if err != nil {
			return err
		}
		if _, err := deleteMute(ctx, tx, groupID, target); err != nil {
			return err
		}
		_ = insertGroupEvent(ctx, tx, groupID, "unmuted", actor, target, nil, s.now())
		_ = s.system(ctx, tx, g.ConversationID, "member.unmuted", map[string]any{"by": actor, "user_id": target})
		return nil
	})
}

// NewInviteCode (R11, B17).
func (s *Service) NewInviteCode(ctx context.Context, groupID, actor int64) (string, time.Time, error) {
	var code string
	var expires time.Time
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, _, err := s.requireRole(ctx, tx, groupID, actor, false); err != nil {
			return err
		}
		now := s.now()
		expires = now.Add(InviteTTL)
		// Retry 3 times on code collision (R11).
		var insertErr error
		for i := 0; i < 3; i++ {
			code = ids.NewInviteCode()
			if insertErr = insertInviteCode(ctx, tx, groupID, code, actor, now, expires); insertErr == nil {
				break
			}
		}
		return insertErr
	})
	return code, expires, err
}

// CanSend implements the message-side GroupAuth port (R15/R16).
func (s *Service) CanSend(ctx context.Context, groupID, userID int64) error {
	m, role, err := activeMember(ctx, s.db, groupID, userID)
	if err != nil {
		return err
	}
	if role == "" {
		return apperrors.New(apperrors.Forbidden, "not a member")
	}
	g, err := findGroup(ctx, s.db, groupID)
	if err != nil {
		return err
	}
	if g.Status != StatusActive {
		return apperrors.New(apperrors.StateConflict, "group is dissolved")
	}
	return canSend(m.Role, m.MutedUntil, s.now())
}

// IsModerator reports whether user is owner or admin (R5).
func (s *Service) IsModerator(ctx context.Context, groupID, userID int64) (bool, error) {
	_, role, err := activeMember(ctx, s.db, groupID, userID)
	return role == RoleOwner || role == RoleAdmin, err
}

// IsMember reports active membership.
func (s *Service) IsMember(ctx context.Context, groupID, userID int64) (bool, error) {
	_, role, err := activeMember(ctx, s.db, groupID, userID)
	return role != "", err
}

// system is a nil-safe wrapper around the optional messenger port.
func (s *Service) system(ctx context.Context, tx mysqlx.Tx, convID int64, event string, detail map[string]any) error {
	if s.msg == nil {
		return nil
	}
	return s.msg.SendSystemTx(ctx, tx, convID, event, detail)
}

// ListMembers returns the active roster (B6).
func (s *Service) ListMembers(ctx context.Context, groupID, userID int64) ([]MemberView, error) {
	if _, role, err := activeMember(ctx, s.db, groupID, userID); err != nil {
		return nil, err
	} else if role == "" {
		return nil, apperrors.New(apperrors.Forbidden, "not a member")
	}
	return listActiveMembers(ctx, s.db, groupID)
}

// ListMyGroups backs GET /users/me/groups.
func (s *Service) ListMyGroups(ctx context.Context, userID int64) ([]MyGroupView, error) {
	return listMyGroups(ctx, s.db, userID)
}

// CreateTodoInput is POST /groups/{id}/todos.
type CreateTodoInput struct {
	Title       string
	Description string
	DueAt       *time.Time
	AssigneeIDs []int64
}

// CreateTodo (R19, B18).
func (s *Service) CreateTodo(ctx context.Context, groupID, actor int64, in CreateTodoInput) (int64, error) {
	if err := validateTitle(in.Title); err != nil {
		return 0, err
	}
	if err := validateDescription(in.Description); err != nil {
		return 0, err
	}
	assignees, err := resolveAssignees(in.AssigneeIDs)
	if err != nil {
		return 0, err
	}
	var id int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if _, _, err := s.requireRole(ctx, tx, groupID, actor, false); err != nil {
			return err
		}
		now := s.now()
		tid, err := insertTodo(ctx, tx, TodoRow{
			GroupID:     groupID,
			Title:       in.Title,
			Description: in.Description,
			DueAt:       in.DueAt,
			CreatedBy:   actor,
		}, now)
		if err != nil {
			return err
		}
		_ = tid
		id = tid
		if err := snapshotMembers(ctx, tx, tid, groupID); err != nil {
			return err
		}
		for _, uid := range assignees {
			if _, role, _ := activeMember(ctx, tx, groupID, uid); role == "" {
				return apperrors.Invalid("assignee is not a member")
			}
			if err := insertAssignee(ctx, tx, tid, uid); err != nil {
				return err
			}
		}
		if err := insertTodoEvent(ctx, tx, tid, "created", actor, map[string]any{"assignees": assignees}, now); err != nil {
			return err
		}
		g, err := lockGroup(ctx, tx, groupID)
		if err != nil {
			return err
		}
		_ = s.system(ctx, tx, g.ConversationID, "todo.created", map[string]any{"todo_id": tid, "group_id": groupID})
		return nil
	})
	return id, err
}

// ListTodos returns todos visible to the caller (R20, B19).
func (s *Service) ListTodos(ctx context.Context, groupID, userID int64) ([]TodoView, error) {
	g, role, err := activeMember(ctx, s.db, groupID, userID)
	_ = g
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, apperrors.New(apperrors.Forbidden, "not a member")
	}
	isMod := role == RoleOwner || role == RoleAdmin
	rows, err := listVisibleTodos(ctx, s.db, groupID, userID, isMod)
	if err != nil {
		return nil, err
	}
	out := make([]TodoView, 0, len(rows))
	for _, t := range rows {
		as, err := listAssignees(ctx, s.db, t.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, TodoView{
			ID: t.ID, Title: t.Title, Description: t.Description, DueAt: t.DueAt,
			Status: t.Status, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, Assignees: as,
		})
	}
	return out, nil
}

// GetTodo returns one todo with visibility enforcement (R20/R21, B20).
func (s *Service) GetTodo(ctx context.Context, todoID, userID int64) (*TodoView, error) {
	var view *TodoView
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		t, err := lockTodo(ctx, tx, todoID)
		if err != nil {
			return err
		}
		_, role, err := activeMember(ctx, tx, t.GroupID, userID)
		if err != nil {
			return err
		}
		if role == "" {
			return apperrors.New(apperrors.Forbidden, "not a member")
		}
		isMod := role == RoleOwner || role == RoleAdmin
		if !isMod {
			inSnap, err := snapshotHasUser(ctx, tx, todoID, userID)
			if err != nil {
				return err
			}
			if !inSnap {
				return apperrors.New(apperrors.Forbidden, "todo not visible to you")
			}
		}
		as, err := listAssignees(ctx, tx, todoID)
		if err != nil {
			return err
		}
		view = &TodoView{
			ID: t.ID, Title: t.Title, Description: t.Description, DueAt: t.DueAt,
			Status: t.Status, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, Assignees: as,
		}
		return nil
	})
	return view, err
}

// CompleteTodo marks the caller's own assignee row completed (R22, B21).
func (s *Service) CompleteTodo(ctx context.Context, todoID, userID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		t, err := lockTodo(ctx, tx, todoID)
		if err != nil {
			return err
		}
		if t.Status != TodoActive {
			return apperrors.New(apperrors.StateConflict, "todo already finished")
		}
		now := s.now()
		ok, err := completeAssignee(ctx, tx, todoID, userID, now)
		if err != nil {
			return err
		}
		if !ok {
			return apperrors.Invalid("you are not an assigned assignee")
		}
		if err := recomputeTodoStatus(ctx, tx, todoID, now); err != nil {
			return err
		}
		return insertTodoEvent(ctx, tx, todoID, "completed", userID, nil, now)
	})
}

// CancelTodo hides an active todo (R24, B22) — moderator only.
func (s *Service) CancelTodo(ctx context.Context, todoID, actor int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		t, err := lockTodo(ctx, tx, todoID)
		if err != nil {
			return err
		}
		_, role, err := activeMember(ctx, tx, t.GroupID, actor)
		if err != nil {
			return err
		}
		if role != RoleOwner && role != RoleAdmin {
			return apperrors.New(apperrors.Forbidden, "moderators only")
		}
		now := s.now()
		if _, err := cancelTodo(ctx, tx, todoID, now); err != nil {
			return err
		}
		if err := hideTodo(ctx, tx, todoID, now); err != nil {
			return err
		}
		return insertTodoEvent(ctx, tx, todoID, "cancelled", actor, nil, now)
	})
}

// ScanOverdueTodos is the worker hook for the overdue transition (R23).
func (s *Service) ScanOverdueTodos(ctx context.Context) (int64, error) {
	return markOverdue(ctx, s.db, s.now())
}

// CleanupExpiredMutes is the worker hook for mute GC.
func (s *Service) CleanupExpiredMutes(ctx context.Context) (int64, error) {
	return pruneExpiredMutes(ctx, s.db, s.now())
}
