package httpx

import (
	"encoding/json"
	"strconv"

	apperrors "github.com/example/wechat/internal/platform/errors"
)

// IDList is a JSON array of numeric ids. ADR-005 requires ids to travel as
// decimal strings in JSON (JS 2^53 safety), so each element must be quoted;
// a bare number is rejected rather than silently accepted.
type IDList []int64

// UnmarshalJSON implements the ADR-005 encoding for id arrays.
func (l *IDList) UnmarshalJSON(data []byte) error {
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "ids must be an array of decimal strings", err)
	}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return apperrors.Wrap(apperrors.InvalidArgument, "invalid id in list", err)
		}
		out = append(out, n)
	}
	*l = out
	return nil
}
