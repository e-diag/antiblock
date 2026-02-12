package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// AdPinRepository интерфейс для хранения закреплений объявлений.
type AdPinRepository interface {
	Create(pin *domain.AdPin) error
	ListByAdID(adID uint) ([]*domain.AdPin, error)
	DeleteByAdID(adID uint) error
}

type adPinRepository struct {
	db *gorm.DB
}

func NewAdPinRepository(db *gorm.DB) AdPinRepository {
	return &adPinRepository{db: db}
}

func (r *adPinRepository) Create(pin *domain.AdPin) error {
	return r.db.Create(pin).Error
}

func (r *adPinRepository) ListByAdID(adID uint) ([]*domain.AdPin, error) {
	var pins []*domain.AdPin
	err := r.db.Where("ad_id = ?", adID).Find(&pins).Error
	return pins, err
}

func (r *adPinRepository) DeleteByAdID(adID uint) error {
	return r.db.Where("ad_id = ?", adID).Delete(&domain.AdPin{}).Error
}
