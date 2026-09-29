// Package pagination provides generic opaque keyset cursors with limit
// clamping. SPEC-00 §2.3 R10; design-review D7.
package pagination

import (
	"strconv"

	"github.com/example/wechat/internal/platform/httpx"
	apperrors "github.com/example/wechat/internal/platform/errors"
)

// Defaults and clamps shared by all list endpoints (contract §17: page sizes).
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Page is the decoded cursor payload. Callers embed the fields they need by
// convention: every ordering MUST end with a unique tiebreaker (usually id).
// kind discriminates cursor shapes when a list supports several sortings.
type Page struct {
	Kind string `json:"k,omitempty"`
	Sort int64  `json:"s,omitempty"` // primary sort value (unix micros or an id)
	ID   int64  `json:"i,omitempty"` // unique tiebreaker (row id)
}

// Cursor helpers.

// Encode serializes a Page into an opaque cursor string.
func Encode(p Page) (string, error) { return httpx.EncodeCursor(p) }

// Decode parses an opaque cursor; empty string yields a zero Page (first page).
func Decode(s string) (Page, error) {
	var p Page
	if err := httpx.DecodeCursor(s, &p); err != nil {
		return Page{}, err
	}
	return p, nil
}

// Limit clamps a requested page size into [1, MaxLimit], defaulting when <= 0.
func Limit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// LimitWithMax clamps into [1, max], defaulting when <= 0.
func LimitWithMax(n, max int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > max {
		return max
	}
	return n
}

// QueryLimit reads ?limit= from a request string ("" → default).
func QueryLimit(s string) int {
	if s == "" {
		return DefaultLimit
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return DefaultLimit
	}
	return Limit(n)
}

// List is the single shape of every paginated response (ADR-007): the items
// plus the opaque cursor for the next page. has_more is derived, never
// hand-maintained, so no endpoint can contradict its own cursor.
type List[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// NewList wraps a page; a non-empty cursor is the only source of has_more.
func NewList[T any](items []T, nextCursor string) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{Items: items, NextCursor: nextCursor, HasMore: nextCursor != ""}
}

// BadCursor is the standard error for undecodable cursors.
func BadCursor(err error) error {
	return apperrors.Wrap(apperrors.InvalidArgument, "invalid cursor", err)
}
