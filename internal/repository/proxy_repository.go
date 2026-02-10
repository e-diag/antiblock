package repository

import (
	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// ProxyRepository определяет интерфейс для работы с прокси-узлами
type ProxyRepository interface {
	Create(proxy *domain.ProxyNode) error
	GetByID(id uint) (*domain.ProxyNode, error)
	Update(proxy *domain.ProxyNode) error
	GetAvailableByType(proxyType domain.ProxyType) ([]*domain.ProxyNode, error)
	GetAll() ([]*domain.ProxyNode, error)
	GetActive() ([]*domain.ProxyNode, error)
	Count() (int64, error)
	CountActive() (int64, error)
}

type proxyRepository struct {
	db *gorm.DB
}

// NewProxyRepository создает новый репозиторий прокси-узлов
func NewProxyRepository(db *gorm.DB) ProxyRepository {
	return &proxyRepository{db: db}
}

func (r *proxyRepository) Create(proxy *domain.ProxyNode) error {
	return r.db.Create(proxy).Error
}

func (r *proxyRepository) GetByID(id uint) (*domain.ProxyNode, error) {
	var proxy domain.ProxyNode
	err := r.db.First(&proxy, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &proxy, nil
}

func (r *proxyRepository) Update(proxy *domain.ProxyNode) error {
	return r.db.Save(proxy).Error
}

func (r *proxyRepository) GetAvailableByType(proxyType domain.ProxyType) ([]*domain.ProxyNode, error) {
	var proxies []*domain.ProxyNode
	err := r.db.Where("type = ? AND status = ?", proxyType, domain.ProxyStatusActive).
		Order("load ASC"). // Выбираем наименее загруженные
		Find(&proxies).Error
	return proxies, err
}

func (r *proxyRepository) GetAll() ([]*domain.ProxyNode, error) {
	var proxies []*domain.ProxyNode
	err := r.db.Find(&proxies).Error
	return proxies, err
}

func (r *proxyRepository) GetActive() ([]*domain.ProxyNode, error) {
	var proxies []*domain.ProxyNode
	err := r.db.Where("status = ?", domain.ProxyStatusActive).Find(&proxies).Error
	return proxies, err
}

func (r *proxyRepository) Count() (int64, error) {
	var count int64
	err := r.db.Model(&domain.ProxyNode{}).Count(&count).Error
	return count, err
}

func (r *proxyRepository) CountActive() (int64, error) {
	var count int64
	err := r.db.Model(&domain.ProxyNode{}).
		Where("status = ?", domain.ProxyStatusActive).
		Count(&count).Error
	return count, err
}
