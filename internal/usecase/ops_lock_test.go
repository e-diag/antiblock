package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

type busyLockRepo struct{}

func (busyLockRepo) TryAcquire(string, string, time.Duration, time.Time) (bool, error) { return false, nil }
func (busyLockRepo) Release(string, string) error                                       { return nil }

func TestMigrationV2OneStep_BlockedByLock(t *testing.T) {
	p := &PaidOps{
		Locker:    NewOpsLocker(busyLockRepo{}),
		LockOwner: "test",
	}
	_, _, err := p.MigrationV2OneStep(context.Background())
	if !errors.Is(err, ErrOpsLockBusy) {
		t.Fatalf("expected ErrOpsLockBusy, got %v", err)
	}
}

func TestCompensate14DaysTransactional_BlockedByLock(t *testing.T) {
	p := &PaidOps{
		Settings:  nil, // проверка на lock происходит после базовой валидации p/settings, поэтому зададим ниже корректно
		Locker:    NewOpsLocker(busyLockRepo{}),
		LockOwner: "test",
	}
	// Минимальный settings репозиторий для прохождения pre-check.
	p.Settings = &stubSettingsRepo{}
	err := Compensate14DaysTransactional(&gorm.DB{}, p)
	if !errors.Is(err, ErrOpsLockBusy) {
		t.Fatalf("expected ErrOpsLockBusy, got %v", err)
	}
}

type stubSettingsRepo struct{}

func (s *stubSettingsRepo) Get(string) (string, error)      { return "", nil }
func (s *stubSettingsRepo) Set(string, string) error         { return nil }
func (s *stubSettingsRepo) Increment(string, int) error      { return nil }
