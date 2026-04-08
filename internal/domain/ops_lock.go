package domain

import "time"

// OpsLock межпроцессная блокировка фоновых paid-операций.
type OpsLock struct {
	LockKey   string    `gorm:"primaryKey;size:64" json:"lock_key"`
	OwnerID   string    `gorm:"size:128;not null" json:"owner_id"`
	ExpiresAt time.Time `gorm:"not null;index" json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OpsLock) TableName() string {
	return "ops_locks"
}
