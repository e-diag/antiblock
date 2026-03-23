package domain

import "time"

// YooKassaPayment — запись об оплате через ЮKassa (Telegram Payments API).
type YooKassaPayment struct {
	ID                        uint      `gorm:"primaryKey" json:"id"`
	TGID                      int64     `gorm:"not null;index" json:"tg_id"`
	TariffType                string    `gorm:"size:20;not null" json:"tariff_type"` // "pro" или "premium"
	AmountRub                 int       `gorm:"not null" json:"amount_rub"`          // сумма в рублях
	DaysGranted               int       `gorm:"not null" json:"days_granted"`
	TelegramPaymentChargeID   string    `gorm:"size:255" json:"telegram_payment_charge_id"`
	ProviderPaymentChargeID   string    `gorm:"size:255" json:"provider_payment_charge_id"` // ID в ЮKassa
	CreatedAt                 time.Time `json:"created_at"`
}

func (YooKassaPayment) TableName() string {
	return "yookassa_payments"
}
