package models

import "time"

// NotificationChannel is a delivery target for sync events. The transport is
// chosen by Type; Config is a JSON document whose shape depends on Type and is
// validated by internal/notify before persisting:
//
//	smtp      {"host":"...","port":587,"username":"...","password":"v1:...",
//	           "from":"...","to":"a@b.c","encryption":"starttls"}
//	webhook   {"url":"https://...","method":"POST","headers":{"X-Key":"v1:..."},
//	           "content_type":"application/json","body_template":"{...}"}
//	shoutrrr  {"url":"slack://token@channel"}
//
// Secrets inside Config are stored as individual AES-GCM ciphertexts and are
// masked in every API response (see docs/notifications.md).
type NotificationChannel struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	Name   string `gorm:"size:191" json:"name"`
	Type   string `gorm:"size:32" json:"type"`
	Config string `gorm:"type:text" json:"config"`
	// Events is a JSON array of subscribed event names (models.AllEvents).
	Events     string     `gorm:"type:text" json:"events"`
	Enabled    bool       `gorm:"default:true" json:"enabled"`
	LastStatus string     `gorm:"size:32" json:"last_status,omitempty"`
	LastError  string     `gorm:"type:text" json:"last_error,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// TableName pins the physical table name.
func (NotificationChannel) TableName() string { return "notification_channels" }
