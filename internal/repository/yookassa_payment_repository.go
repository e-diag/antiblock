package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type YooKassaPaymentRepository interface {
	Create(p *domain.YooKassaPayment) error
	ExistsByProviderPaymentChargeID(providerPaymentChargeID string) (bool, error)
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

func (r *yooKassaPaymentRepository) ExistsByProviderPaymentChargeID(providerPaymentChargeID string) (bool, error) {
	if providerPaymentChargeID == "" {
		return false, nil
	}
	var cnt int64
	err := r.db.Model(&domain.YooKassaPayment{}).
		Where("provider_payment_charge_id = ?", providerPaymentChargeID).
		Count(&cnt).Error
	return cnt > 0, err
}
