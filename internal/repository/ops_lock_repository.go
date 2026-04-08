package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type OpsLockRepository interface {
	TryAcquire(lockKey, ownerID string, ttl time.Duration, now time.Time) (bool, error)
	Release(lockKey, ownerID string) error
}

type opsLockRepository struct {
	db *gorm.DB
}

func NewOpsLockRepository(db *gorm.DB) OpsLockRepository {
	return &opsLockRepository{db: db}
}

func (r *opsLockRepository) TryAcquire(lockKey, ownerID string, ttl time.Duration, now time.Time) (bool, error) {
	if lockKey == "" || ownerID == "" || ttl <= 0 {
		return false, nil
	}
	expiresAt := now.UTC().Add(ttl)
	res := r.db.Exec(`
		INSERT INTO ops_locks (lock_key, owner_id, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), NOW())
		ON CONFLICT (lock_key) DO UPDATE
		SET owner_id = EXCLUDED.owner_id,
			expires_at = EXCLUDED.expires_at,
			updated_at = NOW()
		WHERE ops_locks.expires_at <= ?
	`, lockKey, ownerID, expiresAt, now.UTC())
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *opsLockRepository) Release(lockKey, ownerID string) error {
	return r.db.Where("lock_key = ? AND owner_id = ?", lockKey, ownerID).Delete(&domain.OpsLock{}).Error
}
