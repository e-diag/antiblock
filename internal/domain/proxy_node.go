package domain

import "time"

// ProxyType определяет тип прокси-сервера
type ProxyType string

const (
	ProxyTypeFree    ProxyType = "Free"
	ProxyTypePremium ProxyType = "Premium"
)

// ProxyStatus определяет статус прокси-сервера
type ProxyStatus string

const (
	ProxyStatusActive  ProxyStatus = "Active"
	ProxyStatusBlocked ProxyStatus = "Blocked"
	ProxyStatusInactive ProxyStatus = "Inactive"
)

// ProxyNode представляет прокси-узел MTProto
type ProxyNode struct {
	ID        uint        `gorm:"primaryKey" json:"id"`
	IP        string      `gorm:"not null" json:"ip"`
	Port      int         `gorm:"not null" json:"port"`
	Secret    string      `gorm:"not null" json:"secret"`
	Type      ProxyType   `gorm:"type:varchar(20);not null" json:"type"`
	Status    ProxyStatus `gorm:"type:varchar(20);default:'Active'" json:"status"`
	Load      int         `gorm:"default:0" json:"load"` // текущее количество пользователей
	LastCheck *time.Time  `json:"last_check,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// IsAvailable проверяет, доступен ли прокси для использования
func (p *ProxyNode) IsAvailable() bool {
	return p.Status == ProxyStatusActive
}

// TableName задает имя таблицы для GORM
func (ProxyNode) TableName() string {
	return "proxy_nodes"
}
