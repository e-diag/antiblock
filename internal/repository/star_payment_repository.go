package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type StarPaymentRepository interface {
	Create(p *domain.StarPayment) error
}

type starPaymentRepository struct {
	db *gorm.DB
}

func NewStarPaymentRepository(db *gorm.DB) StarPaymentRepository {
	return &starPaymentRepository{db: db}
}

func (r *starPaymentRepository) Create(p *domain.StarPayment) error {
	return r.db.Create(p).Error
}
