package domain

import "time"

// User представляет пользователя бота
type User struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	TGID         int64      `gorm:"uniqueIndex;not null" json:"tg_id"`
	IsPremium    bool       `gorm:"default:false" json:"is_premium"`
	PremiumUntil *time.Time `json:"premium_until,omitempty"`
	// LastActiveAt хранит время последнего продления подписки
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	ReferralID   *uint      `json:"referral_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// IsPremiumActive проверяет, активна ли премиум подписка
func (u *User) IsPremiumActive() bool {
	if !u.IsPremium {
		return false
	}
	if u.PremiumUntil == nil {
		return false
	}
	return u.PremiumUntil.After(time.Now().UTC())
}

// TableName задает имя таблицы для GORM
func (User) TableName() string {
	return "users"
}
