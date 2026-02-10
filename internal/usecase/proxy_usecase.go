package usecase

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
)

// ProxyUseCase определяет бизнес-логику для работы с прокси
type ProxyUseCase interface {
	GetProxyForUser(user *domain.User) (*domain.ProxyNode, error)
	AddProxy(ip string, port int, secret string, proxyType domain.ProxyType) error
	HealthCheck(proxy *domain.ProxyNode) error
	CheckAllProxies() error
	GetStats() (ProxyStats, error)
}

type ProxyStats struct {
	TotalProxies  int64
	ActiveProxies  int64
	FreeProxies    int64
	PremiumProxies int64
}

type proxyUseCase struct {
	proxyRepo repository.ProxyRepository
}

// NewProxyUseCase создает новый use case для прокси
func NewProxyUseCase(proxyRepo repository.ProxyRepository) ProxyUseCase {
	return &proxyUseCase{proxyRepo: proxyRepo}
}

func (uc *proxyUseCase) GetProxyForUser(user *domain.User) (*domain.ProxyNode, error) {
	var proxyType domain.ProxyType
	if user.IsPremiumActive() {
		proxyType = domain.ProxyTypePremium
	} else {
		proxyType = domain.ProxyTypeFree
	}

	proxies, err := uc.proxyRepo.GetAvailableByType(proxyType)
	if err != nil {
		return nil, err
	}

	if len(proxies) == 0 {
		return nil, fmt.Errorf("no available %s proxies", proxyType)
	}

	// Выбираем наименее загруженный прокси
	selectedProxy := proxies[0]
	
	// Увеличиваем нагрузку
	selectedProxy.Load++
	if err := uc.proxyRepo.Update(selectedProxy); err != nil {
		return nil, err
	}

	return selectedProxy, nil
}

func (uc *proxyUseCase) AddProxy(ip string, port int, secret string, proxyType domain.ProxyType) error {
	// Валидация IP
	if net.ParseIP(ip) == nil {
		return errors.New("invalid IP address")
	}

	// Валидация порта
	if port < 1 || port > 65535 {
		return errors.New("invalid port number")
	}

	// Валидация секрета
	if secret == "" {
		return errors.New("secret cannot be empty")
	}

	proxy := &domain.ProxyNode{
		IP:     ip,
		Port:   port,
		Secret: secret,
		Type:   proxyType,
		Status: domain.ProxyStatusActive,
		Load:   0,
	}

	return uc.proxyRepo.Create(proxy)
}

func (uc *proxyUseCase) HealthCheck(proxy *domain.ProxyNode) error {
	timeout := 5 * time.Second
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", proxy.IP, proxy.Port), timeout)
	if err != nil {
		// Прокси недоступен
		proxy.Status = domain.ProxyStatusInactive
		now := time.Now()
		proxy.LastCheck = &now
		return uc.proxyRepo.Update(proxy)
	}
	defer conn.Close()

	// Прокси доступен
	if proxy.Status != domain.ProxyStatusActive {
		proxy.Status = domain.ProxyStatusActive
	}
	now := time.Now()
	proxy.LastCheck = &now
	return uc.proxyRepo.Update(proxy)
}

func (uc *proxyUseCase) CheckAllProxies() error {
	proxies, err := uc.proxyRepo.GetAll()
	if err != nil {
		return err
	}

	for _, proxy := range proxies {
		_ = uc.HealthCheck(proxy) // Игнорируем ошибки отдельных проверок
	}

	return nil
}

func (uc *proxyUseCase) GetStats() (ProxyStats, error) {
	total, err := uc.proxyRepo.Count()
	if err != nil {
		return ProxyStats{}, err
	}

	active, err := uc.proxyRepo.CountActive()
	if err != nil {
		return ProxyStats{}, err
	}

	allProxies, err := uc.proxyRepo.GetAll()
	if err != nil {
		return ProxyStats{}, err
	}

	var freeCount, premiumCount int64
	for _, p := range allProxies {
		if p.Type == domain.ProxyTypeFree {
			freeCount++
		} else {
			premiumCount++
		}
	}

	return ProxyStats{
		TotalProxies:   total,
		ActiveProxies:  active,
		FreeProxies:    freeCount,
		PremiumProxies: premiumCount,
	}, nil
}
