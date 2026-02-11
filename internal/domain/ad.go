package domain

import "time"

// Ad представляет рекламное объявление (максимум одно активное, показ только бесплатным).
type Ad struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	Text            string     `gorm:"type:text;not null" json:"text"`
	MediaURL        *string    `json:"media_url,omitempty"`
	ButtonURL       *string    `json:"button_url,omitempty"`
	ButtonText      *string    `json:"button_text,omitempty"`
	ChannelLink     string     `gorm:"size:255" json:"channel_link"`     // ссылка на канал (t.me/...)
	ChannelUsername string     `gorm:"size:255" json:"channel_username"` // @username канала
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	Active          bool       `gorm:"default:true" json:"active"`
	Clicks          int        `gorm:"default:0" json:"clicks"`
	Impressions     int        `gorm:"default:0" json:"impressions"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// TableName задает имя таблицы для GORM
func (Ad) TableName() string {
	return "ads"
}
