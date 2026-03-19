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
	// AppendPendingUserID атомарно добавляет tgID в очередь pending-заявки.
	// Если pending-заявки нет, создаёт новую.
	// Возвращает ID заявки и признак, что заявка была создана заново.
	AppendPendingUserID(tgID int64) (reqID uint, isNew bool, err error)
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

func (r *vpsProvisionRequestRepository) AppendPendingUserID(tgID int64) (reqID uint, isNew bool, err error) {
	var res struct {
		ID    uint
		IsNew bool `gorm:"column:is_new"`
	}
	err = r.db.Raw(`
        WITH existing AS (
            SELECT id FROM vps_provision_requests
            WHERE status = 'pending'
            ORDER BY created_at ASC
            LIMIT 1
            FOR UPDATE
        ),
        updated AS (
            UPDATE vps_provision_requests
            SET pending_user_ids = (
                SELECT jsonb_agg(DISTINCT val ORDER BY val)::text
                FROM jsonb_array_elements(
                    COALESCE(pending_user_ids::jsonb, '[]'::jsonb)
                    || jsonb_build_array(?::bigint)
                ) val
            ),
            updated_at = NOW()
            WHERE id = (SELECT id FROM existing)
            RETURNING id, false AS is_new
        ),
        inserted AS (
            INSERT INTO vps_provision_requests (status, pending_user_ids, created_at, updated_at)
            SELECT 'pending', jsonb_build_array(?::bigint)::text, NOW(), NOW()
            WHERE NOT EXISTS (SELECT 1 FROM existing)
            RETURNING id, true AS is_new
        )
        SELECT id, is_new FROM updated
        UNION ALL
        SELECT id, is_new FROM inserted
        LIMIT 1
    `, tgID, tgID).Scan(&res).Error
	if err != nil {
		return 0, false, err
	}
	return res.ID, res.IsNew, nil
}
