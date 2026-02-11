package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// AdRepository определяет интерфейс для работы с рекламой
type AdRepository interface {
	Create(ad *domain.Ad) error
	GetByID(id uint) (*domain.Ad, error)
	Update(ad *domain.Ad) error
	GetActive() ([]*domain.Ad, error)
	GetActiveOne() (*domain.Ad, error)   // не более одного активного объявления
	GetAll() ([]*domain.Ad, error)
	Delete(id uint) error
	DeactivateAll() error                // деактивировать все (при добавлении нового)
	IncrementClicks(id uint) error
	IncrementImpressions(id uint) error
}

type adRepository struct {
	db *gorm.DB
}

// NewAdRepository создает новый репозиторий рекламы
func NewAdRepository(db *gorm.DB) AdRepository {
	return &adRepository{db: db}
}

func (r *adRepository) Create(ad *domain.Ad) error {
	return r.db.Create(ad).Error
}

func (r *adRepository) GetByID(id uint) (*domain.Ad, error) {
	var ad domain.Ad
	err := r.db.First(&ad, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ad, nil
}

func (r *adRepository) Update(ad *domain.Ad) error {
	return r.db.Save(ad).Error
}

func (r *adRepository) GetActive() ([]*domain.Ad, error) {
	var ads []*domain.Ad
	err := r.db.Where("active = ?", true).Find(&ads).Error
	return ads, err
}

func (r *adRepository) GetActiveOne() (*domain.Ad, error) {
	var ad domain.Ad
	err := r.db.Where("active = ?", true).First(&ad).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ad, nil
}

func (r *adRepository) GetAll() ([]*domain.Ad, error) {
	var ads []*domain.Ad
	err := r.db.Find(&ads).Error
	return ads, err
}

func (r *adRepository) Delete(id uint) error {
	return r.db.Delete(&domain.Ad{}, id).Error
}

func (r *adRepository) DeactivateAll() error {
	return r.db.Model(&domain.Ad{}).Where("active = ?", true).Update("active", false).Error
}

func (r *adRepository) IncrementClicks(id uint) error {
	return r.db.Model(&domain.Ad{}).Where("id = ?", id).UpdateColumn("clicks", gorm.Expr("clicks + 1")).Error
}

func (r *adRepository) IncrementImpressions(id uint) error {
	return r.db.Model(&domain.Ad{}).Where("id = ?", id).UpdateColumn("impressions", gorm.Expr("impressions + 1")).Error
}
