package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// PremiumServerRepository хранит/читает VPS для Premium-юзеров.
type PremiumServerRepository interface {
	Create(s *domain.PremiumServer) error
	GetActive() (*domain.PremiumServer, error) // первый активный сервер (по created_at)
	GetAllActive() ([]*domain.PremiumServer, error)
	GetByTimewebID(timewebID int) (*domain.PremiumServer, error)
	GetByID(id uint) (*domain.PremiumServer, error)
	GetAll() ([]*domain.PremiumServer, error)
	Update(s *domain.PremiumServer) error
	Delete(id uint) error
	UpdateSSHHostKey(id uint, hostKey string) error
	UpdateSSHPassword(id uint, password string) error
	// IncrementFIPCount атомарно увеличивает счётчик FIP за текущие сутки UTC; при смене даты сбрасывает в 1.
	IncrementFIPCount(serverID uint) error
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

func (r *premiumServerRepository) GetAllActive() ([]*domain.PremiumServer, error) {
	var servers []*domain.PremiumServer
	err := r.db.Where("is_active = ?", true).Order("created_at ASC").Find(&servers).Error
	return servers, err
}

func (r *premiumServerRepository) GetByTimewebID(timewebID int) (*domain.PremiumServer, error) {
	if timewebID <= 0 {
		return nil, nil
	}
	var s domain.PremiumServer
	err := r.db.Where("timeweb_id = ?", timewebID).First(&s).Error
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

func (r *premiumServerRepository) Delete(id uint) error {
	if id == 0 {
		return nil
	}
	return r.db.Delete(&domain.PremiumServer{}, id).Error
}

func (r *premiumServerRepository) UpdateSSHHostKey(id uint, hostKey string) error {
	return r.db.Model(&domain.PremiumServer{}).
		Where("id = ?", id).
		Update("ssh_host_key", hostKey).Error
}

func (r *premiumServerRepository) UpdateSSHPassword(id uint, password string) error {
	return r.db.Model(&domain.PremiumServer{}).
		Where("id = ?", id).
		Update("ssh_password", password).Error
}

func (r *premiumServerRepository) IncrementFIPCount(serverID uint) error {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return r.db.Exec(`
		UPDATE premium_servers SET
			fip_count_today = CASE
				WHEN fip_count_date IS NULL OR fip_count_date < ?
				THEN 1
				ELSE fip_count_today + 1
			END,
			fip_count_date = ?
		WHERE id = ?
	`, today, today, serverID).Error
}
