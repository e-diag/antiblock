package domain

import "time"

// ProxyType определяет тип прокси-сервера
type ProxyType string

const (
	ProxyTypeFree    ProxyType = "free"
	ProxyTypePremium ProxyType = "premium"
	ProxyTypePro     ProxyType = "pro"
)

// PremiumPortEE1/EE2 — фиксированные порты для двух ee-прокси (nineseconds) при TimeWeb provisioning.
const (
	PremiumPortEE1 = 8443
	PremiumPortEE2 = 443
	// Устаревшие имена: ранее 8443 считался «dd», 443 — «ee»; оба порта теперь только ee.
	PremiumPortDD = PremiumPortEE1
	PremiumPortEE = PremiumPortEE2
)

// ProxyStatus определяет статус прокси-сервера
type ProxyStatus string

const (
	ProxyStatusActive   ProxyStatus = "active"
	ProxyStatusBlocked  ProxyStatus = "blocked"
	ProxyStatusInactive ProxyStatus = "inactive"
)

// ProxyNode представляет прокси-узел MTProto.
// У бесплатных прокси порты могут совпадать (один сервер — несколько ключей); уникальна комбинация (ip, port, secret).
// TimeWeb Premium: IP — адрес для клиента (tg://proxy), всегда персональный floating IP, не основной IP VPS.
type ProxyNode struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	IP     string `gorm:"not null;uniqueIndex:idx_proxy_ip_port_secret" json:"ip"`
	Port   int    `gorm:"not null;uniqueIndex:idx_proxy_ip_port_secret" json:"port"`
	Secret string `gorm:"not null;uniqueIndex:idx_proxy_ip_port_secret" json:"secret"`
	SecretEE string `gorm:"size:255" json:"secret_ee,omitempty"`
	Type          ProxyType   `gorm:"type:varchar(20);not null" json:"type"`

	// TimeWeb Premium: персональный floating IP пользователя.
	// По требованиям пользователя — пользователю выдаётся только floating IP.
	FloatingIP string `gorm:"size:45;index" json:"floating_ip,omitempty"`
	// TimewebFloatingIPID — ID floating IP в TimeWeb (нужен для unbind/delete).
	// Timeweb Cloud возвращает ID floating IP как string (UUID), поэтому храним строкой.
	TimewebFloatingIPID string `gorm:"size:64;default:'';index" json:"timeweb_floating_ip_id,omitempty"`
	// PremiumServerID — сервер в TimeWeb, к которому привязан floating IP.
	PremiumServerID *uint `gorm:"index" json:"premium_server_id,omitempty"`

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
