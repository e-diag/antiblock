package domain

import "time"

// YooKassaInvoice — локальная запись о платеже ЮКассы (Smart Payment) для чистки "висящих" оплат.
type YooKassaInvoice struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	PaymentID string    `gorm:"size:64;uniqueIndex;not null" json:"payment_id"` // id платежа в ЮКассе (uuid-like)
	TGID      int64     `gorm:"not null;index" json:"tg_id"`
	TariffType string   `gorm:"size:20;not null" json:"tariff_type"` // pro|premium
	AmountRub int       `gorm:"not null" json:"amount_rub"`
	DaysGranted int     `gorm:"not null" json:"days_granted"`
	Status    string    `gorm:"size:20;default:'pending'" json:"status"` // pending|paid|cancelled
	ChatID    int64     `gorm:"default:0" json:"chat_id"`
	MessageID int64     `gorm:"default:0" json:"message_id"`
	CreatedAt time.Time `json:"created_at"`
	PaidAt    *time.Time `json:"paid_at,omitempty"`
}

func (YooKassaInvoice) TableName() string {
	return "yookassa_invoices"
}

