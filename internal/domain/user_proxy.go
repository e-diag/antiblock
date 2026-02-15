package domain

import "time"

// UserProxy — выданный пользователю прокси (сохранённые данные для «Мои прокси»).
type UserProxy struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	UserID     uint       `gorm:"not null;index" json:"user_id"`
	IP         string     `gorm:"size:45;not null" json:"ip"`
	Port       int        `gorm:"not null" json:"port"`
	Secret     string     `gorm:"not null" json:"secret"`
	ProxyType  ProxyType  `gorm:"type:varchar(20);not null" json:"proxy_type"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (UserProxy) TableName() string {
	return "user_proxies"
}
