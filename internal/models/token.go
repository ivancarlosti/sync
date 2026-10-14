package models

import "time"

// APIToken is a programmatic credential used against the API (see
// docs/authentication.md).
//
// Several tokens can be alive at once, each with its own name, scope set and
// optional expiry, so a cost tool or a script can be revoked without touching any
// other caller. Only public metadata is stored beside the secret: `Prefix` is the
// indexed half that identifies the token during authentication, while the secret
// half is kept as a SHA-256 digest in `Hash`. The secret leaves the server exactly
// once, in the answer that creates or regenerates the token — it cannot be read
// back afterwards.
type APIToken struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:191" json:"name"`
	// Prefix is the public, indexed half of `sync_<prefix>.<secret>`.
	Prefix string `gorm:"size:32;uniqueIndex" json:"prefix"`
	// Hash is the SHA-256 of the secret half; it is never serialised.
	Hash string `gorm:"size:64" json:"-"`
	// Scopes is a JSON array of the granted scopes (`read` covers every GET).
	Scopes string `gorm:"type:text" json:"-"`
	// CreatedBy is the operator that minted the token, useful in an audit.
	CreatedBy string `gorm:"size:191" json:"created_by,omitempty"`

	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `gorm:"size:64" json:"last_used_ip,omitempty"`
	// Requests counts the authenticated calls, so an unused token stands out.
	Requests int64 `json:"requests"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (APIToken) TableName() string { return "api_tokens" }

// Scope is one capability granted to an API token.
const (
	// ScopeRead allows the safe endpoints (every GET).
	ScopeRead = "read"
	// ScopeWrite allows everything, including the mutating endpoints.
	ScopeWrite = "write"
)

// AllScopes lists every scope in the order the UI displays them.
var AllScopes = []string{ScopeRead, ScopeWrite}

// ValidScope reports whether value is a supported scope.
func ValidScope(value string) bool {
	return value == ScopeRead || value == ScopeWrite
}
