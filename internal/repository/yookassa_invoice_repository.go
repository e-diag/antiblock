package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type YooKassaInvoiceRepository interface {
	Create(inv *domain.YooKassaInvoice) error
	GetByPaymentID(paymentID string) (*domain.YooKassaInvoice, error)
	ListPendingOlderThan(cutoff time.Time) ([]*domain.YooKassaInvoice, error)
	MarkPaid(paymentID string) error
	MarkCancelled(paymentID string) error
	DeleteByPaymentID(paymentID string) error
}

type yooKassaInvoiceRepository struct {
	db *gorm.DB
}

func NewYooKassaInvoiceRepository(db *gorm.DB) YooKassaInvoiceRepository {
	return &yooKassaInvoiceRepository{db: db}
}

func (r *yooKassaInvoiceRepository) Create(inv *domain.YooKassaInvoice) error {
	return r.db.Create(inv).Error
}

func (r *yooKassaInvoiceRepository) GetByPaymentID(paymentID string) (*domain.YooKassaInvoice, error) {
	var inv domain.YooKassaInvoice
	if err := r.db.Where("payment_id = ?", paymentID).First(&inv).Error; err != nil {
		return nil, err
	}
	return &inv, nil
}

func (r *yooKassaInvoiceRepository) ListPendingOlderThan(cutoff time.Time) ([]*domain.YooKassaInvoice, error) {
	var out []*domain.YooKassaInvoice
	err := r.db.Where("status = ? AND created_at < ?", "pending", cutoff).Order("created_at asc").Find(&out).Error
	return out, err
}

func (r *yooKassaInvoiceRepository) MarkPaid(paymentID string) error {
	now := time.Now()
	return r.db.Model(&domain.YooKassaInvoice{}).
		Where("payment_id = ?", paymentID).
		Updates(map[string]interface{}{"status": "paid", "paid_at": &now}).Error
}

func (r *yooKassaInvoiceRepository) MarkCancelled(paymentID string) error {
	return r.db.Model(&domain.YooKassaInvoice{}).
		Where("payment_id = ?", paymentID).
		Update("status", "cancelled").Error
}

func (r *yooKassaInvoiceRepository) DeleteByPaymentID(paymentID string) error {
	return r.db.Where("payment_id = ?", paymentID).Delete(&domain.YooKassaInvoice{}).Error
}

