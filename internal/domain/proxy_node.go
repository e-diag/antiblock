package domain

import "time"

// ProxyType определяет тип прокси-сервера
type ProxyType string

const (
	ProxyTypeFree    ProxyType = "free"
	ProxyTypePremium ProxyType = "premium"
)

// ProxyStatus определяет статус прокси-сервера
type ProxyStatus string

const (
	ProxyStatusActive   ProxyStatus = "active"
	ProxyStatusBlocked  ProxyStatus = "blocked"
	ProxyStatusInactive ProxyStatus = "inactive"
)

// ProxyNode представляет прокси-узел MTProto
type ProxyNode struct {
	ID            uint        `gorm:"primaryKey" json:"id"`
	IP            string      `gorm:"not null" json:"ip"`
	Port          int         `gorm:"not null;uniqueIndex" json:"port"`
	Secret        string      `gorm:"not null" json:"secret"`
	Type          ProxyType   `gorm:"type:varchar(20);not null" json:"type"`
	// OwnerID задает владельца премиум-прокси (один прокси на пользователя)
	OwnerID       *uint       `gorm:"uniqueIndex:idx_premium_owner,where:type = 'premium'" json:"owner_id,omitempty"`
	ContainerName string      `gorm:"size:255" json:"container_name"`
	Status        ProxyStatus `gorm:"type:varchar(20);default:'active'" json:"status"`
	Load          int         `gorm:"default:0" json:"load"` // текущее количество пользователей (для free-прокси)
	LastRTTMs       *int       `json:"last_rtt_ms,omitempty"` // задержка в мс (по HealthCheck), nil если не измерялась
	UnreachableSince *time.Time `gorm:"column:unreachable_since" json:"unreachable_since,omitempty"` // для премиум: не отвечает (проверка каждые 5 мин до восстановления)
	LastCheck     *time.Time  `json:"last_check,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// IsAvailable проверяет, доступен ли прокси для использования
func (p *ProxyNode) IsAvailable() bool {
	return p.Status == ProxyStatusActive
}

// TableName задает имя таблицы для GORM
func (ProxyNode) TableName() string {
	return "proxy_nodes"
}
