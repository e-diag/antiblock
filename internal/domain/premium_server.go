package domain

import "time"

// PremiumServer — VPS для Premium-пользователей (пул: несколько активных одновременно).
// Каждый сервер обслуживает нескольких Premium-юзеров через разные floating IP.
type PremiumServer struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// Name например "premium-1"
	Name string `gorm:"size:255;not null" json:"name"`

	// IP основной адрес VPS (для SSH и/или Docker управления).
	IP string `gorm:"size:45;not null" json:"ip"`

	// TimewebID — ID сервера в TimeWeb (0 если добавлен вручную).
	TimewebID int `gorm:"default:0" json:"timeweb_id"`

	// IsActive — участвует ли сервер в пуле (новые FIP выдаются только на активных).
	IsActive bool `gorm:"default:true" json:"is_active"`

	// FIPCountToday — сколько раз сегодня (UTC) на этом сервере создавали floating IP (лимит 10/сервер/сутки в приложении).
	FIPCountToday int `gorm:"default:0" json:"fip_count_today"`
	// FIPCountDate — дата (UTC, начало суток), за которую актуален FIPCountToday; при смене даты счётчик сбрасывается в IncrementFIPCount.
	FIPCountDate *time.Time `gorm:"column:fip_count_date" json:"fip_count_date,omitempty"`

	// CertPath — путь к TLS-сертификатам (сохраняется для совместимости с legacy-дизайном).
	CertPath string `gorm:"size:500" json:"cert_path"`

	// DockerPort — порт Docker (сохраняется для совместимости с legacy-дизайном).
	DockerPort int `gorm:"default:2376" json:"docker_port"`
	// SSHHostKey — base64 публичного ключа SSH-сервера (для верификации host key).
	SSHHostKey string `gorm:"type:text" json:"ssh_host_key,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PremiumServer) TableName() string { return "premium_servers" }

