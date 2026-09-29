package contact

import (
	"errors"
	"strings"
	"testing"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/sigtoken"
)

func codeOf(t *testing.T, err error) apperrors.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var ae *apperrors.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("not an AppError: %v", err)
	}
	return ae.Code
}

func TestNormalizePairIsOrderIndependent(t *testing.T) {
	lo, hi := normalizePair(9, 3)
	if lo != 3 || hi != 9 {
		t.Fatalf("got (%d,%d)", lo, hi)
	}
	lo2, hi2 := normalizePair(3, 9)
	if lo != lo2 || hi != hi2 {
		t.Fatal("pair normalization is not symmetric")
	}
}

// R1: only the five documented entry points are accepted.
func TestR1ValidateSource(t *testing.T) {
	for _, ok := range []string{SourcePhone, SourceAccountName, SourceQRCode, SourceGroup, SourceCard} {
		if err := validateSource(ok); err != nil {
			t.Errorf("source %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "email", "Phone", "wechat_id"} {
		if code := codeOf(t, validateSource(bad)); code != apperrors.InvalidArgument {
			t.Errorf("source %q: code = %s, want INVALID_ARGUMENT", bad, code)
		}
	}
}

// R7: verify text is at most 200 characters and free of control characters.
func TestR7VerifyTextBounds(t *testing.T) {
	if err := validateVerifyText(strings.Repeat("a", 200)); err != nil {
		t.Fatalf("200 chars rejected: %v", err)
	}
	if code := codeOf(t, validateVerifyText(strings.Repeat("a", 201))); code != apperrors.InvalidArgument {
		t.Fatalf("201 chars: code = %s", code)
	}
	if code := codeOf(t, validateVerifyText(" hello ")); code != apperrors.InvalidArgument {
		t.Fatalf("edge whitespace: code = %s", code)
	}
	if code := codeOf(t, validateVerifyText("a\x00b")); code != apperrors.InvalidArgument {
		t.Fatalf("control char: code = %s", code)
	}
	if err := validateVerifyText(""); err != nil {
		t.Fatalf("empty verify text must be allowed: %v", err)
	}
}

// R15/R16/R19: remark bound and permission enumerations.
func TestR15SettingsPatchValidation(t *testing.T) {
	ok := SettingsPatch{
		Remark:       strPtr(strings.Repeat("x", 64)),
		MessagePerm:  strPtr(PermNoMessage),
		MomentPerm:   strPtr(MomentHidden),
		MomentNotify: boolPtr(false),
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid patch rejected: %v", err)
	}
	cases := map[string]SettingsPatch{
		"remark too long":  {Remark: strPtr(strings.Repeat("x", 65))},
		"bad message perm": {MessagePerm: strPtr("mute")},
		"bad moment perm":  {MomentPerm: strPtr("private")},
		"empty is allowed": {Remark: strPtr("")},
	}
	for name, patch := range cases {
		err := patch.Validate()
		if name == "empty is allowed" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
			continue
		}
		if code := codeOf(t, err); code != apperrors.InvalidArgument {
			t.Errorf("%s: code = %s", name, code)
		}
	}
}

func TestSettingsPatchApplyKeepsOmittedFields(t *testing.T) {
	base := Settings{Remark: "old", MessagePerm: PermBlocked, MomentPerm: MomentHidden, MomentNotify: false}
	got := SettingsPatch{Remark: strPtr("new")}.Apply(base)
	want := Settings{Remark: "new", MessagePerm: PermBlocked, MomentPerm: MomentHidden, MomentNotify: false}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if d := DefaultSettings(); d.MessagePerm != PermNormal || d.MomentPerm != MomentVisible || !d.MomentNotify || d.Remark != "" {
		t.Fatalf("defaults are wrong: %+v", d)
	}
}

// R20: tag names are 1..32 characters.
func TestR20TagNameBounds(t *testing.T) {
	if err := validateTagName(strings.Repeat("标", 32)); err != nil {
		t.Fatalf("32 runes rejected: %v", err)
	}
	if code := codeOf(t, validateTagName(strings.Repeat("标", 33))); code != apperrors.InvalidArgument {
		t.Fatalf("33 runes: code = %s", code)
	}
	if code := codeOf(t, validateTagName("")); code != apperrors.InvalidArgument {
		t.Fatalf("empty: code = %s", code)
	}
	if code := codeOf(t, validateTagName(" pad ")); code != apperrors.InvalidArgument {
		t.Fatalf("edge whitespace: code = %s", code)
	}
}

// R18 + contract §4.4: every blocking direction wins, and no_message only
// silences the side it was set against.
func TestR18DecideSend(t *testing.T) {
	normal := DefaultSettings()
	blocked := Settings{MessagePerm: PermBlocked, MomentPerm: MomentVisible, MomentNotify: true}
	noMessage := Settings{MessagePerm: PermNoMessage, MomentPerm: MomentVisible, MomentNotify: true}

	cases := []struct {
		name     string
		from, to Settings
		want     bool
		reason   string
	}{
		{"both normal", normal, normal, true, ""},
		{"recipient blocked sender", normal, blocked, false, ReasonBlockedByRecipient},
		{"sender blocked recipient", blocked, normal, false, ReasonSenderBlocked},
		{"recipient silenced sender", normal, noMessage, false, ReasonRecipientNoMessage},
		{"sender silenced recipient", noMessage, normal, true, ""},
		{"mutual block", blocked, blocked, false, ReasonBlockedByRecipient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := decideSend(c.from, c.to)
			if got != c.want || reason != c.reason {
				t.Fatalf("got (%v,%q), want (%v,%q)", got, reason, c.want, c.reason)
			}
		})
	}
}

// R6: the rejection cooldown is exactly 24 hours.
func TestR6CooldownRemaining(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		rejectedAt time.Time
		want       time.Duration
	}{
		{now, RejectCooldown},
		{now.Add(-time.Hour), RejectCooldown - time.Hour},
		{now.Add(-RejectCooldown), 0},
		{now.Add(-RejectCooldown - time.Minute), 0},
		{now.Add(time.Minute), RejectCooldown + time.Minute},
	}
	for _, c := range cases {
		if got := cooldownRemaining(c.rejectedAt, now); got != c.want {
			t.Errorf("rejected at %v: got %v, want %v", c.rejectedAt, got, c.want)
		}
	}
}

// R26: search matches nickname or remark, case-insensitively.
func TestR26MatchesSearch(t *testing.T) {
	cases := []struct {
		q, nickname, remark string
		want                bool
	}{
		{"ali", "Alice", "", true},
		{"ALICE", "alice", "", true},
		{"ce", "Alice", "", true},
		{"bob", "Alice", "Bobby", true},
		{"carol", "Alice", "Bobby", false},
		{"", "Alice", "", false},
	}
	for _, c := range cases {
		if got := matchesSearch(c.q, c.nickname, c.remark); got != c.want {
			t.Errorf("matchesSearch(%q,%q,%q) = %v, want %v", c.q, c.nickname, c.remark, got, c.want)
		}
	}
}

// R1: the QR token carries the owner and a hard 7-day expiry, and a tampered
// or foreign-signed token is indistinguishable from an unknown user.
func TestR1QRTokenRoundTrip(t *testing.T) {
	codec := sigtoken.New("secret-a")
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	token, exp, err := encodeQR(codec, 42, now)
	if err != nil {
		t.Fatalf("encodeQR: %v", err)
	}
	if want := now.Add(QRTokenTTL); !exp.Equal(want) {
		t.Fatalf("expiry = %v, want %v", exp, want)
	}
	got, err := decodeQR(codec, token, now)
	if err != nil {
		t.Fatalf("decodeQR: %v", err)
	}
	if got != 42 {
		t.Fatalf("user = %d, want 42", got)
	}
	if _, err := decodeQR(codec, token, now.Add(QRTokenTTL+time.Second)); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("expired token must be RESOURCE_UNAVAILABLE")
	}
	if _, err := decodeQR(sigtoken.New("secret-b"), token, now); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatal("token signed with another key must be rejected")
	}
	if _, err := decodeQR(codec, token+"x", now); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatal("tampered token must be rejected")
	}
}

// R21: batches are validated, de-duplicated and bounded.
func TestR21DedupeIDs(t *testing.T) {
	got, err := dedupeIDs([]int64{3, 1, 3, 2})
	if err != nil {
		t.Fatalf("dedupeIDs: %v", err)
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("got %v", got)
	}
	if code := codeOf(t, mustErr(dedupeIDs([]int64{0}))); code != apperrors.InvalidArgument {
		t.Fatalf("zero id: code = %s", code)
	}
	tooMany := make([]int64, maxTagMembersPerCall+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if code := codeOf(t, mustErr(dedupeIDs(tooMany))); code != apperrors.InvalidArgument {
		t.Fatalf("oversized batch: code = %s", code)
	}
}

func mustErr(_ []int64, err error) error { return err }

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
