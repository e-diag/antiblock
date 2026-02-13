package domain

import "time"

// User представляет пользователя бота
type User struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	TGID         int64      `gorm:"uniqueIndex;not null" json:"tg_id"`
	Username     string     `gorm:"size:255" json:"username,omitempty"` // Telegram @username, может быть пустым
	IsPremium    bool       `gorm:"default:false" json:"is_premium"`
	PremiumUntil *time.Time `json:"premium_until,omitempty"`
	// LastActiveAt хранит время последнего продления подписки
	LastActiveAt          *time.Time `json:"last_active_at,omitempty"`
	ReferralID            *uint      `json:"referral_id,omitempty"`
	ForcedSubCounted      bool       `gorm:"default:false" json:"forced_sub_counted"`
	PremiumReminderSentAt *time.Time `json:"premium_reminder_sent_at,omitempty"` // когда отправлено напоминание за 7 дней до окончания
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
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
