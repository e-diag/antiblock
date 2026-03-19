package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaintenanceWaitRepository очередь tg_id для рассылки после окончания техработ.
type MaintenanceWaitRepository interface {
	AddTGID(tgID int64) error
	Count() (int64, error)
	ListTGIDs() ([]int64, error)
	Clear() error
}

type maintenanceWaitRepository struct {
	db *gorm.DB
}

func NewMaintenanceWaitRepository(db *gorm.DB) MaintenanceWaitRepository {
	return &maintenanceWaitRepository{db: db}
}

func (r *maintenanceWaitRepository) AddTGID(tgID int64) error {
	if tgID == 0 {
		return nil
	}
	u := domain.MaintenanceWaitUser{TgID: tgID}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&u).Error
}

func (r *maintenanceWaitRepository) Count() (int64, error) {
	var n int64
	err := r.db.Model(&domain.MaintenanceWaitUser{}).Count(&n).Error
	return n, err
}

func (r *maintenanceWaitRepository) ListTGIDs() ([]int64, error) {
	var rows []domain.MaintenanceWaitUser
	if err := r.db.Order("tg_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.TgID)
	}
	return out, nil
}

func (r *maintenanceWaitRepository) Clear() error {
	return r.db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&domain.MaintenanceWaitUser{}).Error
}
