package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// VPSProvisionRequestRepository хранит очередь заявок на создание Premium VPS.
type VPSProvisionRequestRepository interface {
	Create(r *domain.VPSProvisionRequest) error
	GetPending() (*domain.VPSProvisionRequest, error) // первая pending-заявка (очередь)
	GetByID(id uint) (*domain.VPSProvisionRequest, error)
	Update(r *domain.VPSProvisionRequest) error
}

type vpsProvisionRequestRepository struct {
	db *gorm.DB
}

func NewVPSProvisionRequestRepository(db *gorm.DB) VPSProvisionRequestRepository {
	return &vpsProvisionRequestRepository{db: db}
}

func (r *vpsProvisionRequestRepository) Create(req *domain.VPSProvisionRequest) error {
	return r.db.Create(req).Error
}

func (r *vpsProvisionRequestRepository) GetPending() (*domain.VPSProvisionRequest, error) {
	var req domain.VPSProvisionRequest
	err := r.db.Where("status = ?", "pending").Order("created_at ASC").First(&req).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (r *vpsProvisionRequestRepository) GetByID(id uint) (*domain.VPSProvisionRequest, error) {
	var req domain.VPSProvisionRequest
	err := r.db.First(&req, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (r *vpsProvisionRequestRepository) Update(req *domain.VPSProvisionRequest) error {
	return r.db.Save(req).Error
}

