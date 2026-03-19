package domain

import "time"

// ProGroup — одна активная группа на календарный день UTC: новые покупатели Pro и продления сидят в группе «сегодня».
// Инфраструктура живёт cycleDays; истёкшие группы прошлых дней переносят подписчиков в группу текущего дня.
type ProGroup struct {
	ID                      uint      `gorm:"primaryKey" json:"id"`
	Date                    time.Time `gorm:"not null;index" json:"date"` // 00:00 UTC (в БД не более одной active на этот день)
	ServerIP                string    `gorm:"size:45;not null" json:"server_ip"`
	InfrastructureExpiresAt time.Time `gorm:"not null;index" json:"infrastructure_expires_at"` // после этого — ротация контейнеров
	PortDD      int       `gorm:"not null" json:"port_dd"` // порт dd-контейнера
	PortEE      int       `gorm:"not null" json:"port_ee"` // порт ee-контейнера (PortDD+10000)
	SecretDD    string    `gorm:"not null" json:"secret_dd"`
	SecretEE    string    `gorm:"not null" json:"secret_ee"`
	ContainerDD string    `gorm:"size:255" json:"container_dd"`
	ContainerEE string    `gorm:"size:255" json:"container_ee"`
	Status      ProxyStatus `gorm:"type:varchar(20);default:'active'" json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (ProGroup) TableName() string { return "pro_groups" }

// ProSubscription — подписка пользователя на Pro-группу.
type ProSubscription struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     uint      `gorm:"not null;index" json:"user_id"`
	ProGroupID uint      `gorm:"not null;index" json:"pro_group_id"`
	ExpiresAt  time.Time `gorm:"not null" json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func (ProSubscription) TableName() string { return "pro_subscriptions" }

