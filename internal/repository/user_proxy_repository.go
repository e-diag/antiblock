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
	// DeleteByIPPort удаляет все записи выданных прокси с указанными ip и port (при удалении узла из proxy_nodes).
	DeleteByIPPort(ip string, port int) error
	// DeleteByIPPortSecret удаляет записи по точному совпадению ip, port, secret (один прокси).
	DeleteByIPPortSecret(ip string, port int, secret string) error
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

func (r *userProxyRepository) DeleteByIPPort(ip string, port int) error {
	return r.db.Where("ip = ? AND port = ?", ip, port).Delete(&domain.UserProxy{}).Error
}

func (r *userProxyRepository) DeleteByIPPortSecret(ip string, port int, secret string) error {
	return r.db.Where("ip = ? AND port = ? AND secret = ?", ip, port, secret).Delete(&domain.UserProxy{}).Error
}
