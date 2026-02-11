package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// SettingsRepository хранит ключ-значение (канал ОП, счётчик подписок)
type SettingsRepository interface {
	Get(key string) (string, error)
	Set(key, value string) error
}

type settingsRepository struct {
	db *gorm.DB
}

func NewSettingsRepository(db *gorm.DB) SettingsRepository {
	return &settingsRepository{db: db}
}

func (r *settingsRepository) Get(key string) (string, error) {
	var s domain.AppSetting
	err := r.db.Where("key = ?", key).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return s.Value, nil
}

func (r *settingsRepository) Set(key, value string) error {
	return r.db.Save(&domain.AppSetting{Key: key, Value: value}).Error
}
