package domain

import "time"

// PaymentEvent фиксирует обработку внешнего платежного события для идемпотентности.
type PaymentEvent struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Provider   string    `gorm:"size:32;not null;index:idx_payment_events_provider_external,unique" json:"provider"`
	ExternalID string    `gorm:"size:255;not null;index:idx_payment_events_provider_external,unique" json:"external_id"`
	Status     string    `gorm:"size:20;not null;default:'processing'" json:"status"` // processing|succeeded|failed
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (PaymentEvent) TableName() string {
	return "payment_events"
}
