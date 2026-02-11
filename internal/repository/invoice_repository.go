package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type InvoiceRepository interface {
	Create(inv *domain.Invoice) error
	GetByInvoiceID(invoiceID int64) (*domain.Invoice, error)
	Update(inv *domain.Invoice) error
}

type invoiceRepository struct {
	db *gorm.DB
}

func NewInvoiceRepository(db *gorm.DB) InvoiceRepository {
	return &invoiceRepository{db: db}
}

func (r *invoiceRepository) Create(inv *domain.Invoice) error {
	return r.db.Create(inv).Error
}

func (r *invoiceRepository) GetByInvoiceID(invoiceID int64) (*domain.Invoice, error) {
	var inv domain.Invoice
	err := r.db.Where("invoice_id = ?", invoiceID).First(&inv).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (r *invoiceRepository) Update(inv *domain.Invoice) error {
	return r.db.Save(inv).Error
}
