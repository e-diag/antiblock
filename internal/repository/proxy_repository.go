package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// ProxyRepository определяет интерфейс для работы с прокси-узлами
type ProxyRepository interface {
	Create(proxy *domain.ProxyNode) error
	GetByID(id uint) (*domain.ProxyNode, error)
	GetByOwnerID(ownerID uint) (*domain.ProxyNode, error)
	Update(proxy *domain.ProxyNode) error
	Delete(id uint) error
	GetAvailableByType(proxyType domain.ProxyType) ([]*domain.ProxyNode, error)
	GetAll() ([]*domain.ProxyNode, error)
	GetActive() ([]*domain.ProxyNode, error)
	Count() (int64, error)
	CountActive() (int64, error)
	// FindFirstFreePort возвращает первый свободный порт в диапазоне [minPort, maxPort],
	// который не используется ни одним прокси в таблице proxy_nodes.
	FindFirstFreePort(minPort, maxPort int) (int, error)
	// DeactivateUserProxy помечает персональный прокси пользователя как inactive (если он существует).
	DeactivateUserProxy(ownerID uint) error
	// CleanupExpiredPremiumProxies очищает персональные премиум-прокси пользователей,
	// у которых подписка истекла более cutoffAgo назад (по полю users.premium_until).
	CleanupExpiredPremiumProxies(cutoff time.Time) error
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

func (r *proxyRepository) GetByOwnerID(ownerID uint) (*domain.ProxyNode, error) {
	var proxy domain.ProxyNode
	err := r.db.Where("owner_id = ?", ownerID).First(&proxy).Error
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

func (r *proxyRepository) Delete(id uint) error {
	return r.db.Delete(&domain.ProxyNode{}, id).Error
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

// FindFirstFreePort реализует поиск первого свободного порта в заданном диапазоне,
// который не занят ни одной записью в proxy_nodes.
func (r *proxyRepository) FindFirstFreePort(minPort, maxPort int) (int, error) {
	if minPort <= 0 || maxPort <= 0 || minPort > maxPort {
		return 0, gorm.ErrInvalidDB
	}

	var ports []int
	if err := r.db.Model(&domain.ProxyNode{}).
		Where("port BETWEEN ? AND ?", minPort, maxPort).
		Pluck("port", &ports).Error; err != nil {
		return 0, err
	}

	used := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		used[p] = struct{}{}
	}

	for port := minPort; port <= maxPort; port++ {
		if _, exists := used[port]; !exists {
			return port, nil
		}
	}

	return 0, gorm.ErrRecordNotFound
}

// DeactivateUserProxy переводит персональный премиум-прокси пользователя в статус inactive,
// при этом связь с owner_id сохраняется.
func (r *proxyRepository) DeactivateUserProxy(ownerID uint) error {
	return r.db.Model(&domain.ProxyNode{}).
		Where("owner_id = ? AND type = ?", ownerID, domain.ProxyTypePremium).
		Where("status != ?", domain.ProxyStatusInactive).
		Update("status", domain.ProxyStatusInactive).Error
}

// CleanupExpiredPremiumProxies удаляет персональные премиум-прокси пользователей,
// у которых подписка истекла ранее cutoff (по полю users.premium_until).
// Подзапрос с JOIN к users устраняет ошибку "missing FROM-clause entry for table users"
// и совместим с PostgreSQL и SQLite.
func (r *proxyRepository) CleanupExpiredPremiumProxies(cutoff time.Time) error {
	return r.db.Exec(
		`DELETE FROM proxy_nodes
         WHERE type = ?
           AND owner_id IN (
             SELECT id FROM users
             WHERE premium_until IS NOT NULL AND premium_until < ?
           )`,
		domain.ProxyTypePremium,
		cutoff,
	).Error
}
