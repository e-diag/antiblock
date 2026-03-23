package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type YooKassaPaymentRepository interface {
	Create(p *domain.YooKassaPayment) error
}

type yooKassaPaymentRepository struct {
	db *gorm.DB
}

func NewYooKassaPaymentRepository(db *gorm.DB) YooKassaPaymentRepository {
	return &yooKassaPaymentRepository{db: db}
}

func (r *yooKassaPaymentRepository) Create(p *domain.YooKassaPayment) error {
	return r.db.Create(p).Error
}
