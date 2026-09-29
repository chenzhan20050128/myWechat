package auth

// RegisterRequest is the POST /api/v1/auth/register body (SPEC-01 §4).
type RegisterRequest struct {
	Phone       string     `json:"phone"`
	Password    string     `json:"password"`
	AccountName string     `json:"account_name"`
	Nickname    string     `json:"nickname"`
	Device      DeviceInfo `json:"device"`
}

// LoginRequest is the POST /api/v1/auth/login body.
type LoginRequest struct {
	LoginID  string     `json:"login_id"` // phone or account_name
	Password string     `json:"password"`
	Device   DeviceInfo `json:"device"`
}

// RefreshRequest is the POST /api/v1/auth/refresh body.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// ChangePasswordRequest is the POST /api/v1/auth/password body.
type ChangePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}
