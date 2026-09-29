package media

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
)

func codeOf(t *testing.T, err error) apperrors.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return apperrors.AsApp(err).Code
}

// R3: single part up to 10MB, fixed 5MB parts above, exact totals.
func TestR3PlanChunks(t *testing.T) {
	cases := []struct {
		size  int64
		chunk int64
		total int
	}{
		{1, 1, 1},
		{10 << 20, 10 << 20, 1},
		{10<<20 + 1, ChunkSize, 3},
		{15 << 20, ChunkSize, 3},
		{200 << 20, ChunkSize, 40},
	}
	for _, c := range cases {
		plan, err := planChunks(c.size)
		if err != nil {
			t.Fatalf("size %d: %v", c.size, err)
		}
		if plan.ChunkSize != c.chunk || plan.TotalChunks != c.total {
			t.Errorf("size %d: plan = %+v, want chunk=%d total=%d", c.size, plan, c.chunk, c.total)
		}
	}
	for _, bad := range []int64{0, -1, MaxObjectSize + 1} {
		if code := codeOf(t, mustPlan(bad)); code != apperrors.InvalidArgument {
			t.Errorf("size %d: code = %s", bad, code)
		}
	}
}

func mustPlan(size int64) error {
	_, err := planChunks(size)
	return err
}

// R4: every part but the last must be exactly chunk_size.
func TestR4ExpectedChunkSize(t *testing.T) {
	cases := []struct {
		n           int
		want        int64
		description string
	}{
		{1, 5 << 20, "first of three"},
		{2, 5 << 20, "middle"},
		{3, 2 << 20, "last is the remainder"},
		{0, -1, "below range"},
		{4, -1, "above range"},
	}
	for _, c := range cases {
		if got := expectedChunkSize(c.n, 3, 5<<20, 12<<20); got != c.want {
			t.Errorf("%s: got %d, want %d", c.description, got, c.want)
		}
	}
	if got := expectedChunkSize(1, 1, 7, 7); got != 7 {
		t.Errorf("single part: got %d, want 7", got)
	}
}

// R8/R9: the sniffed type is authoritative whenever it is concrete.
func TestR8R9ResolveMIME(t *testing.T) {
	cases := []struct {
		name           string
		sniffed, declared string
		want           string
		wantErr        bool
	}{
		{"agreement", "image/png", "image/png", "image/png", false},
		{"declared generic", "image/png", "application/octet-stream", "image/png", false},
		{"generic bytes", "application/octet-stream", "application/octet-stream", "application/octet-stream", false},
		{"unsniffable format keeps declaration", "application/octet-stream", "audio/amr", "audio/amr", false},
		{"unsniffable format over real content", "image/png", "audio/amr", "", true},
		{"mismatch", "image/png", "image/jpeg", "", true},
		{"junk claiming to be an image", "application/octet-stream", "image/jpeg", "", true},
		{"not whitelisted declaration", "image/png", "image/bmp", "", true},
		{"not whitelisted content", "application/zip", "application/octet-stream", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveMIME(c.sniffed, c.declared)
			if c.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// R9: the server derives the type from the bytes, and a real PNG sniffs as PNG.
func TestR9SniffRealPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if got := sniffMIME(buf.Bytes()); got != "image/png" {
		t.Fatalf("sniffed %q, want image/png", got)
	}
	if _, err := resolveMIME(sniffMIME(buf.Bytes()), "image/jpeg"); err == nil {
		t.Fatal("a PNG declared as JPEG must be rejected")
	}
	if mime, err := resolveMIME(sniffMIME(buf.Bytes()), "image/png"); err != nil || mime != "image/png" {
		t.Fatalf("got (%q,%v)", mime, err)
	}
}

// R1/R2/R9 declaration validation.
func TestDeclarationValidation(t *testing.T) {
	for _, ok := range []string{PurposeMessage, PurposeMoment, PurposeFavorite, PurposeAvatar, PurposeTransfer} {
		if err := validatePurpose(ok); err != nil {
			t.Errorf("purpose %q rejected: %v", ok, err)
		}
	}
	if code := codeOf(t, validatePurpose("sticker")); code != apperrors.InvalidArgument {
		t.Errorf("purpose: code = %s", code)
	}
	if code := codeOf(t, validateFileName("")); code != apperrors.InvalidArgument {
		t.Error("empty file name")
	}
	if code := codeOf(t, validateFileName(strings.Repeat("a", 256))); code != apperrors.InvalidArgument {
		t.Error("overlong file name")
	}
	if code := codeOf(t, validateFileName(" has space ")); code != apperrors.InvalidArgument {
		t.Error("edge whitespace file name")
	}
	if err := validateFileName(strings.Repeat("a", 255)); err != nil {
		t.Errorf("255 chars must pass: %v", err)
	}
	for _, bad := range []string{"", "short", "not-hex!", "ABCDEF0000000000000000000000000000000000000000000000000000000000"} {
		if code := codeOf(t, ValidateSHA256(bad)); code != apperrors.InvalidArgument {
			t.Errorf("sha %q: code = %s", bad, code)
		}
	}
	if err := ValidateSHA256("a" + strings.Repeat("b", 63)); err != nil {
		t.Errorf("valid sha rejected: %v", err)
	}
}

// SPEC-01 R27 object-side avatar constraints.
func TestR27CheckAvatarObject(t *testing.T) {
	if err := checkAvatarObject(1024, "image/png", ObjectReady); err != nil {
		t.Fatalf("valid avatar rejected: %v", err)
	}
	if code := codeOf(t, checkAvatarObject(MaxAvatarSize+1, "image/png", ObjectReady)); code != apperrors.InvalidArgument {
		t.Errorf("oversize: code = %s", code)
	}
	if code := codeOf(t, checkAvatarObject(1024, "image/gif", ObjectReady)); code != apperrors.InvalidArgument {
		t.Errorf("gif avatar: code = %s", code)
	}
	if code := codeOf(t, checkAvatarObject(1024, "image/png", ObjectCleaned)); code != apperrors.ResourceUnavailable {
		t.Errorf("not ready: code = %s", code)
	}
}

// R10/R5: keys are server-generated only — no client bytes can reach a path.
func TestKeyShapes(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if got := objectKey(now, "01923e5f-6f1c-7cc3-9c2c-2f5a71d2f8b4"); got != "obj/2026/09/01923e5f-6f1c-7cc3-9c2c-2f5a71d2f8b4" {
		t.Fatalf("objectKey = %q", got)
	}
	if got := chunkKey("01923e5f-6f1c-7cc3-9c2c-2f5a71d2f8b4", 3); got != "tmp/01923e5f-6f1c-7cc3-9c2c-2f5a71d2f8b4/3" {
		t.Fatalf("chunkKey = %q", got)
	}
}
