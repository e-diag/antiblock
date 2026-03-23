package domain

import "time"

// MaintenanceWaitUser — пользователь (tg_id), который обратился к боту во время технических работ;
// после отключения режима ему отправляется уведомление.
type MaintenanceWaitUser struct {
	TgID      int64     `gorm:"primaryKey;column:tg_id" json:"tg_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (MaintenanceWaitUser) TableName() string {
	return "maintenance_wait_users"
}
