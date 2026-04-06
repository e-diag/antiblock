package usecase

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
	"gorm.io/gorm"
)

func testDBCompensation(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&domain.AppSetting{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestCompensate14DaysTransactionalIdempotent(t *testing.T) {
	t.Parallel()
	db := testDBCompensation(t)
	sr := repository.NewSettingsRepository(db)
	if err := sr.Set(SettingPaidCompensation14dV1, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	p := &PaidOps{Settings: sr}
	err := Compensate14DaysTransactional(db, p)
	if err != ErrPaidCompensationAlreadyDone {
		t.Fatalf("expected ErrPaidCompensationAlreadyDone, got %v", err)
	}
}
