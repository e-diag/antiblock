package domain

import "time"

// PremiumServer — VPS для Premium-пользователей, управляемый менеджером.
// Один сервер обслуживает нескольких Premium-юзеров через разные floating IP.
type PremiumServer struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// Name например "premium-1"
	Name string `gorm:"size:255;not null" json:"name"`

	// IP основной адрес VPS (для SSH и/или Docker управления).
	IP string `gorm:"size:45;not null" json:"ip"`

	// TimewebID — ID сервера в TimeWeb (0 если добавлен вручную).
	TimewebID int `gorm:"default:0" json:"timeweb_id"`

	// IsActive — принимает ли новых пользователей.
	IsActive bool `gorm:"default:true" json:"is_active"`

	// CertPath — путь к TLS-сертификатам (сохраняется для совместимости с legacy-дизайном).
	CertPath string `gorm:"size:500" json:"cert_path"`

	// DockerPort — порт Docker (сохраняется для совместимости с legacy-дизайном).
	DockerPort int `gorm:"default:2376" json:"docker_port"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PremiumServer) TableName() string { return "premium_servers" }

