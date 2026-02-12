package domain

// AdPin хранит факт закрепления объявления у пользователя (для последующего открепления).
type AdPin struct {
	ID        uint  `gorm:"primaryKey" json:"id"`
	AdID      uint  `gorm:"not null;index:idx_ad_pins_ad_id" json:"ad_id"`
	UserID    int64 `gorm:"not null" json:"user_id"`     // TGID пользователя
	ChatID    int64 `gorm:"not null" json:"chat_id"`      // ID чата (в личке = UserID)
	MessageID int   `gorm:"not null" json:"message_id"`   // ID закреплённого сообщения
}

func (AdPin) TableName() string {
	return "ad_pins"
}
