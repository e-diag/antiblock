package domain

import "time"

// Ad представляет рекламное объявление
type Ad struct {
	ID       uint      `gorm:"primaryKey" json:"id"`
	Text     string    `gorm:"type:text" json:"text"`
	MediaURL *string   `json:"media_url,omitempty"`
	ButtonURL *string  `json:"button_url,omitempty"`
	ButtonText *string `json:"button_text,omitempty"`
	Active   bool      `gorm:"default:true" json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName задает имя таблицы для GORM
func (Ad) TableName() string {
	return "ads"
}
