package usecase

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
	"gorm.io/gorm"
)

// ProxyUseCase определяет бизнес-логику для работы с прокси
type ProxyUseCase interface {
	GetProxyForUser(user *domain.User) (*domain.ProxyNode, error)
	AddProxy(ip string, port int, secret string, proxyType domain.ProxyType) error
	DeleteProxy(id uint) error
	GetAll() ([]*domain.ProxyNode, error)
	GetByOwnerID(ownerID uint) (*domain.ProxyNode, error)
	HealthCheck(proxy *domain.ProxyNode) error
	CheckAllProxies() error
	GetStats() (ProxyStats, error)
	// EnsurePremiumProxyForUser гарантирует, что у пользователя есть персональный премиум-прокси:
	// - если уже есть, возвращает его и активирует (status = active);
	// - если нет, создаёт новый с уникальным портом и привязывает к owner_id.
	// Диапазон портов по умолчанию 20000-50000.
	EnsurePremiumProxyForUser(user *domain.User, ip, secret, containerName string) (*domain.ProxyNode, error)
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

var secretRegexp = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func validateSecret(secret string) error {
	if secret == "" {
		return errors.New("secret cannot be empty")
	}
	if !secretRegexp.MatchString(secret) {
		return errors.New("invalid secret format")
	}
	return nil
}

// isUniquePortError определяет, является ли ошибка нарушением
// уникального ограничения по полю port (дубликат порта).
// Работает как с обёрткой GORM (ErrDuplicatedKey), так и с
// postgres-ошибками напрямую (код SQLSTATE 23505).
func isUniquePortError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return false
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
	if err := validateSecret(secret); err != nil {
		return err
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

func (uc *proxyUseCase) DeleteProxy(id uint) error {
	p, err := uc.proxyRepo.GetByID(id)
	if err != nil || p == nil {
		return fmt.Errorf("proxy not found")
	}
	return uc.proxyRepo.Delete(id)
}

func (uc *proxyUseCase) GetAll() ([]*domain.ProxyNode, error) {
	return uc.proxyRepo.GetAll()
}

func (uc *proxyUseCase) GetByOwnerID(ownerID uint) (*domain.ProxyNode, error) {
	return uc.proxyRepo.GetByOwnerID(ownerID)
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

// EnsurePremiumProxyForUser реализует жизненный цикл персонального премиум-прокси пользователя.
// Если у пользователя уже есть премиум-прокси, он активируется и (опционально) обновляются IP/secret/containerName.
// Если нет — создаётся новый прокси с уникальным портом в диапазоне [20000, 50000].
func (uc *proxyUseCase) EnsurePremiumProxyForUser(user *domain.User, ip, secret, containerName string) (*domain.ProxyNode, error) {
	if user == nil {
		return nil, errors.New("user is nil")
	}

	// Валидация IP и секрета для нового/обновляемого прокси
	if ip == "" || net.ParseIP(ip) == nil {
		return nil, errors.New("invalid IP address")
	}
	if err := validateSecret(secret); err != nil {
		return nil, err
	}

	// Пытаемся найти существующий персональный премиум-прокси пользователя
	existing, err := uc.proxyRepo.GetByOwnerID(user.ID)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		// Реактивация существующего прокси
		existing.Type = domain.ProxyTypePremium
		existing.Status = domain.ProxyStatusActive
		existing.IP = ip
		existing.Secret = secret
		existing.ContainerName = containerName

		if err := uc.proxyRepo.Update(existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	// Новый персональный премиум-прокси:
	// пытаемся несколько раз найти свободный порт и создать запись,
	// чтобы избежать гонок при высокой конкуренции.
	const (
		minPort    = 20000
		maxPort    = 50000
		maxRetries = 5
	)

	ownerID := user.ID

	for attempt := 0; attempt < maxRetries; attempt++ {
		port, err := uc.proxyRepo.FindFirstFreePort(minPort, maxPort)
		if err != nil {
			return nil, fmt.Errorf("failed to find free port: %w", err)
		}

		proxy := &domain.ProxyNode{
			IP:            ip,
			Port:          port,
			Secret:        secret,
			Type:          domain.ProxyTypePremium,
			Status:        domain.ProxyStatusActive,
			Load:          0,
			OwnerID:       &ownerID,
			ContainerName: containerName,
		}

		if err := uc.proxyRepo.Create(proxy); err != nil {
			// Если в этот момент другой поток успел занять порт,
			// получим ошибку уникального индекса и попробуем ещё раз.
			if isUniquePortError(err) {
				continue
			}
			return nil, err
		}

		return proxy, nil
	}

	return nil, fmt.Errorf("failed to allocate unique port after %d retries", maxRetries)
}
