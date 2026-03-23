package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

type ProGroupRepository interface {
	Create(g *domain.ProGroup) error
	// GetByDate — активная группа за календарный день UTC (date в [day, day+24h)).
	GetByDate(date time.Time) (*domain.ProGroup, error)
	// ListGroupsNeedingRotation — active и infrastructure_expires_at <= now.
	ListGroupsNeedingRotation(now time.Time) ([]*domain.ProGroup, error)
	GetActiveGroups() ([]*domain.ProGroup, error)
	Update(g *domain.ProGroup) error
	Delete(id uint) error
	GetByID(id uint) (*domain.ProGroup, error)
}

type ProSubscriptionRepository interface {
	Create(s *domain.ProSubscription) error
	Update(s *domain.ProSubscription) error
	GetByUserID(userID uint) (*domain.ProSubscription, error)
	GetActiveByGroupID(groupID uint) ([]*domain.ProSubscription, error)
	CountActiveByGroupID(groupID uint) (int64, error)
	ExpireByUserID(userID uint) error
}

type proGroupRepository struct{ db *gorm.DB }
type proSubscriptionRepository struct{ db *gorm.DB }

func NewProGroupRepository(db *gorm.DB) ProGroupRepository {
	return &proGroupRepository{db: db}
}
func NewProSubscriptionRepository(db *gorm.DB) ProSubscriptionRepository {
	return &proSubscriptionRepository{db: db}
}

func (r *proGroupRepository) Create(g *domain.ProGroup) error { return r.db.Create(g).Error }

func (r *proGroupRepository) GetByID(id uint) (*domain.ProGroup, error) {
	var g domain.ProGroup
	err := r.db.First(&g, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &g, err
}

func (r *proGroupRepository) GetByDate(date time.Time) (*domain.ProGroup, error) {
	var g domain.ProGroup
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	err := r.db.Where("date >= ? AND date < ? AND status = ?", dayStart, dayEnd, domain.ProxyStatusActive).First(&g).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &g, err
}

func (r *proGroupRepository) ListGroupsNeedingRotation(now time.Time) ([]*domain.ProGroup, error) {
	var groups []*domain.ProGroup
	err := r.db.Where("status = ? AND infrastructure_expires_at <= ?", domain.ProxyStatusActive, now).
		Order("id ASC").Find(&groups).Error
	return groups, err
}

func (r *proGroupRepository) GetActiveGroups() ([]*domain.ProGroup, error) {
	var groups []*domain.ProGroup
	err := r.db.Where("status = ?", domain.ProxyStatusActive).Find(&groups).Error
	return groups, err
}

func (r *proGroupRepository) Update(g *domain.ProGroup) error { return r.db.Save(g).Error }
func (r *proGroupRepository) Delete(id uint) error            { return r.db.Delete(&domain.ProGroup{}, id).Error }

func (r *proSubscriptionRepository) Create(s *domain.ProSubscription) error { return r.db.Create(s).Error }
func (r *proSubscriptionRepository) Update(s *domain.ProSubscription) error { return r.db.Save(s).Error }

func (r *proSubscriptionRepository) GetByUserID(userID uint) (*domain.ProSubscription, error) {
	var s domain.ProSubscription
	err := r.db.Where("user_id = ? AND expires_at > ?", userID, time.Now()).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &s, err
}

func (r *proSubscriptionRepository) GetActiveByGroupID(groupID uint) ([]*domain.ProSubscription, error) {
	var subs []*domain.ProSubscription
	err := r.db.Where("pro_group_id = ? AND expires_at > ?", groupID, time.Now()).Find(&subs).Error
	return subs, err
}

func (r *proSubscriptionRepository) CountActiveByGroupID(groupID uint) (int64, error) {
	var count int64
	err := r.db.Model(&domain.ProSubscription{}).Where("pro_group_id = ? AND expires_at > ?", groupID, time.Now()).Count(&count).Error
	return count, err
}

func (r *proSubscriptionRepository) ExpireByUserID(userID uint) error {
	return r.db.Model(&domain.ProSubscription{}).Where("user_id = ?", userID).Update("expires_at", time.Now()).Error
}

