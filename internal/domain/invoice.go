package domain

import "time"

// Invoice — счёт (CryptoPay), для связи invoice_id -> user при webhook
type Invoice struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	InvoiceID  int64     `gorm:"uniqueIndex;not null" json:"invoice_id"` // ID в CryptoPay
	UserID     int64     `gorm:"not null" json:"user_id"`               // TG user id
	Status     string    `gorm:"size:20;default:'pending'" json:"status"`
	Amount     float64   `json:"amount"`
	Currency   string    `gorm:"size:10" json:"currency"`
	CreatedAt  time.Time `json:"created_at"`
	PaidAt     *time.Time `json:"paid_at,omitempty"`
}

func (Invoice) TableName() string {
	return "invoices"
}
