package domain

import "time"

// Invoice — счёт (CryptoPay/xRocket), для связи invoice_id -> user при webhook
type Invoice struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	InvoiceID  int64     `gorm:"uniqueIndex;not null" json:"invoice_id"` // ID в платёжной системе
	UserID     int64     `gorm:"not null" json:"user_id"`               // TG user id (= chat_id в личке)
	Status     string    `gorm:"size:20;default:'pending'" json:"status"`
	Amount     float64   `json:"amount"`
	Currency   string    `gorm:"size:10" json:"currency"`
	CreatedAt  time.Time `json:"created_at"`
	PaidAt     *time.Time `json:"paid_at,omitempty"`
	ChatID     int64     `gorm:"default:0" json:"chat_id"`     // чат, куда отправлено сообщение с инвойсом (0 = не задано)
	MessageID  int64     `gorm:"default:0" json:"message_id"` // ID сообщения с инвойсом для удаления после оплаты
}

func (Invoice) TableName() string {
	return "invoices"
}
