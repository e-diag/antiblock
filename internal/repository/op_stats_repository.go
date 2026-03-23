package repository

import (
	"time"

	"gorm.io/gorm"
)

type OPChannelClick struct {
	ID        uint      `gorm:"primaryKey"`
	Channel   string    `gorm:"size:255;not null;index"`
	UserTGID  int64     `gorm:"not null"`
	ClickedAt time.Time `gorm:"autoCreateTime"`
}

func (OPChannelClick) TableName() string { return "op_channel_clicks" }

type OPStatsRepository interface {
	RecordClick(channel string, userTGID int64) error
	GetClicksByChannel() (map[string]int64, error)
}

type opStatsRepository struct{ db *gorm.DB }

func NewOPStatsRepository(db *gorm.DB) OPStatsRepository {
	return &opStatsRepository{db: db}
}

func (r *opStatsRepository) RecordClick(channel string, userTGID int64) error {
	return r.db.Exec(
		"INSERT INTO op_channel_clicks (channel, user_tg_id) VALUES (?, ?)",
		channel, userTGID,
	).Error
}

type channelCount struct {
	Channel string
	Count   int64
}

func (r *opStatsRepository) GetClicksByChannel() (map[string]int64, error) {
	var rows []channelCount
	err := r.db.Raw(
		"SELECT channel, COUNT(*) as count FROM op_channel_clicks GROUP BY channel ORDER BY count DESC",
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make(map[string]int64, len(rows))
	for _, row := range rows {
		result[row.Channel] = row.Count
	}
	return result, nil
}
