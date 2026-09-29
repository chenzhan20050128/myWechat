package contact

import (
	"strconv"
	"time"

	"github.com/example/wechat/internal/platform/httpx"
	"github.com/example/wechat/internal/user"
)

// createRequestBody is the POST /api/v1/contacts/requests body (R1). Exactly
// one target field is meaningful for the chosen source.
type createRequestBody struct {
	Source      string `json:"source"`
	TargetID    int64  `json:"target_id,string"`
	Phone       string `json:"phone"`
	AccountName string `json:"account_name"`
	QRCodeToken string `json:"qrcode_token"`
	CardOwnerID int64  `json:"card_owner_id,string"`
	VerifyText  string `json:"verify_text"`
}

// settingsPatchBody is the PATCH /api/v1/contacts/friends/{friendID}/settings
// body; omitted fields keep their value (R15-R19).
type settingsPatchBody struct {
	Remark       *string `json:"remark"`
	MessagePerm  *string `json:"message_perm"`
	MomentPerm   *string `json:"moment_perm"`
	MomentNotify *bool   `json:"moment_notify"`
}

// tagBody is the POST/PATCH /api/v1/contacts/tags body (R20).
type tagBody struct {
	Name string `json:"name"`
}

// tagMembersBody is the POST/DELETE /api/v1/contacts/tags/{id}/members body (R21).
type tagMembersBody struct {
	FriendIDs httpx.IDList `json:"friend_ids"`
}

// identityView is the user-module projection shared by every contact response.
type identityView struct {
	UserID        string  `json:"user_id"`
	Nickname      string  `json:"nickname"`
	AccountName   string  `json:"account_name"`
	AvatarMediaID *string `json:"avatar_media_id,omitempty"`
}

func viewFromIdentity(it user.Identity) identityView {
	v := identityView{
		UserID:      strconv.FormatInt(it.UserID, 10),
		Nickname:    it.Nickname,
		AccountName: it.AccountName,
	}
	if it.AvatarMediaID != nil {
		s := strconv.FormatInt(*it.AvatarMediaID, 10)
		v.AvatarMediaID = &s
	}
	return v
}

// requestView is one friend-application row (R10).
type requestView struct {
	ID          string       `json:"id"`
	Applicant   identityView `json:"applicant"`
	Target      identityView `json:"target"`
	Source      string       `json:"source"`
	VerifyText  string       `json:"verify_text"`
	Status      string       `json:"status"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func viewFromRequest(e RequestEntry) requestView {
	return requestView{
		ID:         strconv.FormatInt(e.Request.ID, 10),
		Applicant:  viewFromIdentity(e.Applicant),
		Target:     viewFromIdentity(e.Target),
		Source:     e.Request.Source,
		VerifyText: e.Request.VerifyText,
		Status:     e.Request.Status,
		CreatedAt:  e.Request.CreatedAt,
		UpdatedAt:  e.Request.UpdatedAt,
	}
}

// friendView is one address-book row: the caller's own view of a friend (R25).
type friendView struct {
	identityView
	Remark       string    `json:"remark"`
	MessagePerm  string    `json:"message_perm"`
	MomentPerm   string    `json:"moment_perm"`
	MomentNotify bool      `json:"moment_notify"`
	Tags         []tagView `json:"tags"`
}

func viewFromFriend(e FriendEntry) friendView {
	tags := make([]tagView, 0, len(e.Tags))
	for _, t := range e.Tags {
		tags = append(tags, viewFromTagRef(t))
	}
	return friendView{
		identityView: viewFromIdentity(e.Identity),
		Remark:       e.Settings.Remark,
		MessagePerm:  e.Settings.MessagePerm,
		MomentPerm:   e.Settings.MomentPerm,
		MomentNotify: e.Settings.MomentNotify,
		Tags:         tags,
	}
}

// lookupView is the stranger-visible result of R24; never carries phone,
// region or signature (contract §4.5).
type lookupView struct {
	identityView
	IsFriend bool `json:"is_friend"`
}

// tagView is one contact tag (R20).
type tagView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func viewFromTag(t Tag) tagView {
	return tagView{ID: strconv.FormatInt(t.ID, 10), Name: t.Name, CreatedAt: t.CreatedAt}
}

func viewFromTagRef(t TagRef) tagView {
	return tagView{ID: strconv.FormatInt(t.ID, 10), Name: t.Name}
}

// settingsView echoes the stored settings after a patch.
type settingsView struct {
	Remark       string `json:"remark"`
	MessagePerm  string `json:"message_perm"`
	MomentPerm   string `json:"moment_perm"`
	MomentNotify bool   `json:"moment_notify"`
}

// qrcodeView is the signed personal QR token (R1).
type qrcodeView struct {
	QRCodeToken string    `json:"qrcode_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// acceptView reports the outcome of R8.
type acceptView struct {
	Status         string `json:"status"`
	Epoch          string `json:"epoch,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}
