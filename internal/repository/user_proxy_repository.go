package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// UserProxyRepository — выданные пользователю прокси («Мои прокси»).
type UserProxyRepository interface {
	Create(up *domain.UserProxy) error
	ListByUserID(userID uint) ([]*domain.UserProxy, error)
	GetByID(id uint) (*domain.UserProxy, error)
}

type userProxyRepository struct {
	db *gorm.DB
}

func NewUserProxyRepository(db *gorm.DB) UserProxyRepository {
	return &userProxyRepository{db: db}
}

func (r *userProxyRepository) Create(up *domain.UserProxy) error {
	return r.db.Create(up).Error
}

func (r *userProxyRepository) ListByUserID(userID uint) ([]*domain.UserProxy, error) {
	var list []*domain.UserProxy
	err := r.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&list).Error
	return list, err
}

func (r *userProxyRepository) GetByID(id uint) (*domain.UserProxy, error) {
	var up domain.UserProxy
	err := r.db.First(&up, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &up, nil
}
