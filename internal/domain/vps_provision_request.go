package domain

import "time"

// VPSProvisionRequest — заявка на создание нового Premium VPS.
// Создаётся ботом, когда исчерпан лимит floating IP или нет активных серверов.
// Менеджер подтверждает параметры через диалог в боте.
type VPSProvisionRequest struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// pending, confirmed, creating, done, cancelled
	Status string `gorm:"size:20;default:'pending'" json:"status"`

	// Параметры VPS — задаются менеджером в диалоге.
	Name     string `gorm:"size:255" json:"name"`
	ConfigID int    `gorm:"default:0" json:"config_id"`
	OSImageID string `gorm:"size:64;default:''" json:"os_image_id"`
	RegionID string `gorm:"size:50" json:"region_id"`

	// TimewebServerID — ID сервера в TimeWeb после POST /servers; нужен для возобновления, если упали на SSH/Docker.
	TimewebServerID int `gorm:"default:0" json:"timeweb_server_id"`

	// Очередь пользователей, ожидающих этот сервер.
	// Храним JSON-массив tg_id.
	PendingUserIDs string `gorm:"type:text" json:"pending_user_ids"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (VPSProvisionRequest) TableName() string { return "vps_provision_requests" }

