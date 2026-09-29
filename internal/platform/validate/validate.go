// Package validate centralizes input normalization and validation rules so
// every module shares one canonical implementation (design-review D6, D11).
package validate

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReservedAccountNames are rejected for account_name/nickname to prevent
// impersonation of system identities (SPEC-01 R5).
var ReservedAccountNames = map[string]struct{}{
	"admin": {}, "system": {}, "wechat": {}, "official": {}, "support": {}, "filehelper": {},
}

// NormalizePhone strips spaces/dashes, keeps a leading +. Returns "" if any
// remaining char is not a digit. (D6: normalize-at-write.)
func NormalizePhone(in string) string {
	var b strings.Builder
	for i, r := range in {
		switch {
		case r == ' ' || r == '-' || r == '(' || r == ')':
			continue
		case r == '+' && i == 0:
			b.WriteRune(r)
		case unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	return b.String()
}

// Phone reports whether p is a plausible phone identifier (E.164-ish).
func Phone(p string) bool {
	if len(p) < 6 || len(p) > 16 {
		return false
	}
	digits := 0
	for i, r := range p {
		if r == '+' {
			if i != 0 {
				return false
			}
			continue
		}
		if !unicode.IsDigit(r) {
			return false
		}
		digits++
	}
	return digits >= 5
}

// NormalizeAccountName trims, lowercases. Returns "" when invalid.
func NormalizeAccountName(in string) string {
	s := strings.ToLower(strings.TrimSpace(in))
	if len(s) < 6 || len(s) > 20 {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return ""
		}
	}
	return s
}

// AccountName reports validity of a normalized account name.
func AccountName(s string) bool {
	if _, reserved := ReservedAccountNames[s]; reserved {
		return false
	}
	return NormalizeAccountName(s) == s
}

// Password enforces 8..32 runes with at least one letter and one digit (SPEC-01 R2).
func Password(pw string) bool {
	n := utf8.RuneCountInString(pw)
	if n < 8 || n > 32 {
		return false
	}
	var hasLetter, hasDigit bool
	for _, r := range pw {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

// RuneRange reports whether s has between min and max runes.
func RuneRange(s string, min, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= min && n <= max
}

// NoControl reports whether s contains no control characters and no
// leading/trailing whitespace (SPEC-01 R5).
func NoControl(s string) bool {
	if s != strings.TrimSpace(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// NotReserved reports whether s (already normalized) is not a system reserved word.
func NotReserved(s string) bool {
	_, bad := ReservedAccountNames[s]
	return !bad
}

// LowerHex64 reports whether s is a 64-char lowercase hex string (sha256).
func LowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// LikeEscape escapes LIKE wildcards in user search input (mysqlx R13:
// parameterized LIKE with escaped %, _ and \). Contract §12.4.
func LikeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// OneOf reports whether s is one of the allowed values.
func OneOf(s string, allowed ...string) bool {
	for _, a := range allowed {
		if s == a {
			return true
		}
	}
	return false
}
