package group

import (
	"testing"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
)

func ptrTime(t time.Time) *time.Time { return &t }

func TestCanSend(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		role      string
		mutedUntil *time.Time
		wantErr   bool
	}{
		{"owner immune", RoleOwner, ptrTime(now.Add(time.Hour)), false},
		{"admin immune", RoleAdmin, ptrTime(now.Add(time.Hour)), false},
		{"member no mute", RoleMember, nil, false},
		{"member muted", RoleMember, ptrTime(now.Add(time.Hour)), true},
		{"member permanent mute", RoleMember, ptrTime(time.Time{}), true},
		{"member expired mute", RoleMember, ptrTime(now.Add(-time.Hour)), false},
		{"unknown role with no mute", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := canSend(c.role, c.mutedUntil, now)
			if c.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

func TestMuteUntil(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in       string
		wantNil  bool
		wantDiff time.Duration
		wantErr  bool
	}{
		{"10m", false, 10 * time.Minute, false},
		{"1h", false, time.Hour, false},
		{"1d", false, 24 * time.Hour, false},
		{"forever", true, 0, false},
		{"30m", false, 0, true},
		{"", false, 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := muteUntil(c.in, now)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %v", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want time, got nil")
			}
			if d := got.Sub(now); d != c.wantDiff {
				t.Fatalf("want diff %v, got %v", c.wantDiff, d)
			}
		})
	}
}

func TestResolveAssignees(t *testing.T) {
	ids, err := resolveAssignees([]int64{1, 2, 1, 3, 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("want 3 deduped, got %v", ids)
	}
	if ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("unexpected order: %v", ids)
	}
	if _, err := resolveAssignees(nil); err == nil {
		t.Fatalf("want error for empty")
	}
	big := make([]int64, 21)
	for i := range big {
		big[i] = int64(i + 1)
	}
	if _, err := resolveAssignees(big); err == nil {
		t.Fatalf("want error for >20")
	}
	if _, err := resolveAssignees([]int64{0, -1}); err == nil {
		t.Fatalf("want error for non-positive")
	}
}

func TestValidateGroupName(t *testing.T) {
	if err := validateGroupName(""); err == nil {
		t.Fatal("empty name should fail")
	}
	if err := validateGroupName(string(make([]rune, MaxGroupnameRunes+1))); err == nil {
		t.Fatal("long name should fail")
	}
	if err := validateGroupName("hello\nworld"); err == nil {
		t.Fatal("control char should fail")
	}
	if err := validateGroupName("老友记"); err != nil {
		t.Fatalf("valid name should pass: %v", err)
	}
}

func TestValidateTitle(t *testing.T) {
	if err := validateTitle(""); err == nil {
		t.Fatal("empty title should fail")
	}
	if err := validateTitle("valid title"); err != nil {
		t.Fatalf("valid title should pass: %v", err)
	}
}

func TestValidateDescription(t *testing.T) {
	if err := validateDescription(string(make([]rune, MaxDescRunes+1))); err == nil {
		t.Fatal("long description should fail")
	}
	if err := validateDescription("short"); err != nil {
		t.Fatalf("short desc should pass: %v", err)
	}
}

func TestReasonOf(t *testing.T) {
	if reasonOf("quit") != LeftQuit {
		t.Fatal("quit not normalized")
	}
	if reasonOf("removed") != LeftRemoved {
		t.Fatal("removed not normalized")
	}
	if reasonOf("dissolved") != LeftDissolved {
		t.Fatal("dissolved not normalized")
	}
	if reasonOf("garbage") != "" {
		t.Fatal("garbage should normalize to empty")
	}
}

func TestCanSendForbiddenCode(t *testing.T) {
	now := time.Now()
	err := canSend(RoleMember, ptrTime(now.Add(time.Hour)), now)
	if err == nil {
		t.Fatal("muted should error")
	}
	if ae, ok := err.(*apperrors.AppError); !ok || ae.Code != apperrors.Forbidden {
		t.Fatalf("want Forbidden, got %T %v", err, err)
	}
}
