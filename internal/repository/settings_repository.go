package repository

import (
	"fmt"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// SettingsRepository хранит ключ-значение (канал ОП, счётчик подписок)
type SettingsRepository interface {
	Get(key string) (string, error)
	Set(key, value string) error
	// Increment атомарно увеличивает числовое значение на delta.
	// Если ключ отсутствует, создаёт запись со значением delta.
	Increment(key string, delta int) error
}

type settingsRepository struct {
	db *gorm.DB
}

func NewSettingsRepository(db *gorm.DB) SettingsRepository {
	return &settingsRepository{db: db}
}

func (r *settingsRepository) Get(key string) (string, error) {
	var s domain.AppSetting
	tx := r.db.Where("key = ?", key).Limit(1).Find(&s)
	if tx.Error != nil {
		return "", tx.Error
	}
	if tx.RowsAffected == 0 || s.Key == "" {
		return "", nil
	}
	return s.Value, nil
}

func (r *settingsRepository) Set(key, value string) error {
	return r.db.Save(&domain.AppSetting{Key: key, Value: value}).Error
}

func (r *settingsRepository) Increment(key string, delta int) error {
	result := r.db.Exec(`
		INSERT INTO app_settings ("key", value) VALUES (?, ?)
		ON CONFLICT ("key") DO UPDATE SET value = (COALESCE(NULLIF(app_settings.value, ''), '0')::int + ?)::text
	`, key, fmt.Sprintf("%d", delta), delta)
	return result.Error
}
