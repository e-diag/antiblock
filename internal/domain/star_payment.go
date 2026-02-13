package domain

import "time"

// StarPayment — запись об оплате через Telegram Stars (премиум).
type StarPayment struct {
	ID                      uint      `gorm:"primaryKey" json:"id"`
	TGID                    int64     `gorm:"not null;index" json:"tg_id"`
	AmountTotal             int64     `gorm:"not null" json:"amount_total"`             // в минимальных единицах (как в API)
	Currency                string    `gorm:"size:10;not null" json:"currency"`         // напр. XTR
	DaysGranted             int       `gorm:"not null" json:"days_granted"`             // выданных дней премиума
	TelegramPaymentChargeID string    `gorm:"size:255" json:"telegram_payment_charge_id,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
}

func (StarPayment) TableName() string {
	return "star_payments"
}
