package usecase

import (
	"errors"
	"fmt"
	"time"

	"github.com/yourusername/antiblock/internal/repository"
)

var ErrOpsLockBusy = errors.New("operation is already running")

type OpsLocker struct {
	repo repository.OpsLockRepository
}

func NewOpsLocker(repo repository.OpsLockRepository) *OpsLocker {
	if repo == nil {
		return nil
	}
	return &OpsLocker{repo: repo}
}

func (l *OpsLocker) Acquire(lockKey, ownerID string, ttl time.Duration) error {
	if l == nil || l.repo == nil {
		return nil
	}
	ok, err := l.repo.TryAcquire(lockKey, ownerID, ttl, time.Now().UTC())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrOpsLockBusy, lockKey)
	}
	return nil
}

func (l *OpsLocker) Release(lockKey, ownerID string) {
	if l == nil || l.repo == nil {
		return
	}
	_ = l.repo.Release(lockKey, ownerID)
}
