package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// PremiumServerRepository хранит/читает VPS для Premium-юзеров.
type PremiumServerRepository interface {
	Create(s *domain.PremiumServer) error
	GetActive() (*domain.PremiumServer, error) // первый активный сервер
	GetByID(id uint) (*domain.PremiumServer, error)
	GetAll() ([]*domain.PremiumServer, error)
	Update(s *domain.PremiumServer) error
	UpdateSSHHostKey(id uint, hostKey string) error
}

type premiumServerRepository struct {
	db *gorm.DB
}

func NewPremiumServerRepository(db *gorm.DB) PremiumServerRepository {
	return &premiumServerRepository{db: db}
}

func (r *premiumServerRepository) Create(s *domain.PremiumServer) error {
	return r.db.Create(s).Error
}

func (r *premiumServerRepository) GetActive() (*domain.PremiumServer, error) {
	var s domain.PremiumServer
	err := r.db.Where("is_active = ?", true).Order("created_at ASC").First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *premiumServerRepository) GetByID(id uint) (*domain.PremiumServer, error) {
	var s domain.PremiumServer
	err := r.db.First(&s, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *premiumServerRepository) GetAll() ([]*domain.PremiumServer, error) {
	var all []*domain.PremiumServer
	if err := r.db.Find(&all).Error; err != nil {
		return nil, err
	}
	return all, nil
}

func (r *premiumServerRepository) Update(s *domain.PremiumServer) error {
	return r.db.Save(s).Error
}

func (r *premiumServerRepository) UpdateSSHHostKey(id uint, hostKey string) error {
	return r.db.Model(&domain.PremiumServer{}).
		Where("id = ?", id).
		Update("ssh_host_key", hostKey).Error
}
