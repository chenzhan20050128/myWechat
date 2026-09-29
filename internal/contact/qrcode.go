package contact

import (
	"encoding/json"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/sigtoken"
)

// qrPayload is the signed personal-QR payload (R1): owner id + hard expiry.
type qrPayload struct {
	UserID    int64 `json:"u"`
	ExpiresAt int64 `json:"e"`
}

// encodeQR builds a signed personal QR token valid for QRTokenTTL (R1).
func encodeQR(codec *sigtoken.Codec, userID int64, now time.Time) (string, time.Time, error) {
	exp := now.Add(QRTokenTTL)
	body, err := json.Marshal(qrPayload{UserID: userID, ExpiresAt: exp.Unix()})
	if err != nil {
		return "", time.Time{}, apperrors.Wrap(apperrors.InternalError, "encode qrcode token", err)
	}
	return codec.Sign(body), exp, nil
}

// decodeQR resolves a personal QR token to its owner. A tampered, malformed or
// expired token is indistinguishable from an unknown user (R24 semantics: the
// caller must not learn whether the token ever existed).
func decodeQR(codec *sigtoken.Codec, token string, now time.Time) (int64, error) {
	body, err := codec.Verify(token)
	if err != nil {
		return 0, apperrors.Unavail("invalid qrcode token")
	}
	var p qrPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, apperrors.Unavail("invalid qrcode token")
	}
	if p.UserID <= 0 || p.ExpiresAt <= now.Unix() {
		return 0, apperrors.Unavail("qrcode token expired")
	}
	return p.UserID, nil
}
