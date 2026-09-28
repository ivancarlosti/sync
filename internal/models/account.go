package models

import "time"

// Setting is a single runtime configuration value. Values are stored as text
// (JSON when structured) and are intentionally free-form so new options can be
// added without a schema migration. See Admin > Settings.
type Setting struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Key       string    `gorm:"size:191;uniqueIndex" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (Setting) TableName() string { return "settings" }

// ConnectedAccount is one OAuth authorisation granted to a provider. Only one
// account per (provider, provider account id) pair is kept: reconnecting the
// same remote account rotates the stored tokens instead of inserting a row.
type ConnectedAccount struct {
	ID                uint   `gorm:"primaryKey" json:"id"`
	Provider          string `gorm:"size:32;index;uniqueIndex:idx_account_identity" json:"provider"`
	ProviderAccountID string `gorm:"size:191;uniqueIndex:idx_account_identity" json:"provider_account_id"`
	Email             string `gorm:"size:191;index" json:"email"`
	DisplayName       string `gorm:"size:191" json:"display_name"`
	AvatarURL         string `gorm:"size:512" json:"avatar_url"`
	// AccessToken/RefreshToken hold AES-GCM ciphertext, never a raw token, and
	// are therefore never serialised to the API (json:"-").
	AccessToken  string     `gorm:"type:text" json:"-"`
	RefreshToken string     `gorm:"type:text" json:"-"`
	TokenType    string     `gorm:"size:32" json:"-"`
	Scopes       string     `gorm:"type:text" json:"scopes"`
	ExpiresAt    time.Time  `json:"expires_at"`
	Status       string     `gorm:"size:32;default:connected" json:"status"`
	LastError    string     `gorm:"type:text" json:"last_error,omitempty"`
	RefreshedAt  *time.Time `json:"refreshed_at,omitempty"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// TableName pins the physical table name.
func (ConnectedAccount) TableName() string { return "connected_accounts" }

// Expired reports whether the access token is expired or expires within the
// two-minute safety margin used before every provider call.
func (a *ConnectedAccount) Expired(now time.Time) bool {
	return !a.ExpiresAt.IsZero() && now.Add(2*time.Minute).After(a.ExpiresAt)
}

// MaskEmail partly hides the local part of the address, for log lines where the
// account must be identifiable but not disclosed in full.
func (a *ConnectedAccount) MaskEmail() string {
	email := a.Email
	at := -1
	for i, r := range email {
		if r == '@' {
			at = i
			break
		}
	}
	if at <= 1 {
		return email
	}
	return email[:1] + "***" + email[at:]
}

// OAuthState stores the server side of an authorization-code flow (CSRF state
// plus the PKCE verifier) until the provider redirects back to Sync.
type OAuthState struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	State        string    `gorm:"size:128;uniqueIndex" json:"state"`
	Flow         string    `gorm:"size:32" json:"flow"`
	Provider     string    `gorm:"size:32" json:"provider"`
	CodeVerifier string    `gorm:"size:191" json:"-"`
	RedirectTo   string    `gorm:"size:512" json:"redirect_to"`
	ExpiresAt    time.Time `gorm:"index" json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (OAuthState) TableName() string { return "oauth_states" }

// Expired reports whether the state can no longer be redeemed.
func (s *OAuthState) Expired(now time.Time) bool { return now.After(s.ExpiresAt) }
