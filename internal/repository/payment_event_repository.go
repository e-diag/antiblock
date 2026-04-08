package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type PaymentEventRepository interface {
	TryStart(provider, externalID string) (started bool, err error)
	MarkSucceeded(provider, externalID string) error
	MarkFailed(provider, externalID string) error
	Get(provider, externalID string) (*domain.PaymentEvent, error)
}

type paymentEventRepository struct {
	db *gorm.DB
}

func NewPaymentEventRepository(db *gorm.DB) PaymentEventRepository {
	return &paymentEventRepository{db: db}
}

func (r *paymentEventRepository) TryStart(provider, externalID string) (bool, error) {
	if provider == "" || externalID == "" {
		return false, nil
	}
	res := r.db.Exec(`
		INSERT INTO payment_events (provider, external_id, status, created_at, updated_at)
		VALUES (?, ?, 'processing', NOW(), NOW())
		ON CONFLICT (provider, external_id) DO UPDATE
		SET status = 'processing',
			updated_at = NOW()
		WHERE payment_events.status = 'failed'
	`, provider, externalID)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *paymentEventRepository) MarkSucceeded(provider, externalID string) error {
	now := time.Now().UTC()
	return r.db.Model(&domain.PaymentEvent{}).
		Where("provider = ? AND external_id = ?", provider, externalID).
		Updates(map[string]any{"status": "succeeded", "updated_at": now}).Error
}

func (r *paymentEventRepository) MarkFailed(provider, externalID string) error {
	now := time.Now().UTC()
	return r.db.Model(&domain.PaymentEvent{}).
		Where("provider = ? AND external_id = ?", provider, externalID).
		Updates(map[string]any{"status": "failed", "updated_at": now}).Error
}

func (r *paymentEventRepository) Get(provider, externalID string) (*domain.PaymentEvent, error) {
	var e domain.PaymentEvent
	err := r.db.Where("provider = ? AND external_id = ?", provider, externalID).First(&e).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

