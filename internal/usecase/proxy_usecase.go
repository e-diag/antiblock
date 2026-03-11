package usecase

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
	"gorm.io/gorm"
)

// ProxyUseCase определяет бизнес-логику для работы с прокси
type ProxyUseCase interface {
	GetProxyForUser(user *domain.User, preferFree bool) (*domain.ProxyNode, error)
	AddProxy(ip string, port int, secret string, proxyType domain.ProxyType) error
	DeleteProxy(id uint) error
	GetAll() ([]*domain.ProxyNode, error)
	GetByOwnerID(ownerID uint) (*domain.ProxyNode, error)
	HealthCheck(proxy *domain.ProxyNode) error
	CheckAllProxies() error
	GetStats() (ProxyStats, error)
	// HasAvailableFreeProxy возвращает true, если есть хотя бы один доступный free-прокси.
	HasAvailableFreeProxy() (bool, error)
	// GetActivePremiumProxies возвращает активные премиум-прокси (для проверки раз в 15 мин).
	GetActivePremiumProxies() ([]*domain.ProxyNode, error)
	// GetUnreachablePremiumProxies возвращает премиум-прокси с unreachable_since (перепроверка раз в 5 мин).
	GetUnreachablePremiumProxies() ([]*domain.ProxyNode, error)
	// CheckPremiumProxy проверяет премиум-прокси; при неудаче выставляет UnreachableSince, при успехе сбрасывает. Возвращает reachable.
	CheckPremiumProxy(proxy *domain.ProxyNode) (reachable bool, err error)
	// EnsurePremiumProxyForUser гарантирует персональный премиум-прокси: генерирует секрет (dd+32 hex),
	// выбирает свободный порт 20000-30000, создаёт/обновляет запись в proxy_nodes с serverIP.
	EnsurePremiumProxyForUser(user *domain.User, serverIP string) (*domain.ProxyNode, error)
	// GetAllFreeProxies возвращает все free-прокси (активные и неактивные) для мониторинга.
	GetAllFreeProxies() ([]*domain.ProxyNode, error)
	// CheckFreeProxy проверяет TCP-доступность free-прокси, обновляет статус и RTT в БД. Возвращает (reachable, rttMs).
	CheckFreeProxy(proxy *domain.ProxyNode) (reachable bool, rttMs int)
}

type ProxyStats struct {
	TotalProxies          int64
	ActiveProxies         int64
	FreeProxies           int64
	PremiumProxies        int64 // только активные премиум
	UnreachablePremiumCount int64
}

type proxyUseCase struct {
	proxyRepo      repository.ProxyRepository
	userProxyRepo  repository.UserProxyRepository
}

// ErrNoMoreFreeProxiesForUser возвращается, когда пользователь уже получил все доступные бесплатные прокси.
var ErrNoMoreFreeProxiesForUser = errors.New("user has received all available free proxies")

var secretRegexpHex = regexp.MustCompile(`^[0-9a-fA-F]+$`)
var secretRegexpGeneral = regexp.MustCompile(`^[A-Za-z0-9+/=_\-]{16,255}$`)

func validateSecret(secret string) error {
	if secret == "" {
		return errors.New("secret cannot be empty")
	}
	// Формат MTProto v2: строго строчный префикс "dd" + 32+ hex-символов.
	if strings.HasPrefix(secret, "dd") {
		hexPart := secret[2:]
		if len(hexPart) < 32 {
			return errors.New("invalid secret: hex part too short (min 32 chars after dd prefix)")
		}
		if !secretRegexpHex.MatchString(hexPart) {
			return errors.New("invalid secret: hex part contains non-hex characters")
		}
		return nil
	}
	if !secretRegexpGeneral.MatchString(secret) {
		return errors.New("invalid secret format (allowed: A-Za-z0-9+/=_-, length 16-255)")
	}
	return nil
}

// isDuplicateKeyError определяет, является ли ошибка нарушением уникального ограничения (дубликат).
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "23505") || strings.Contains(s, "duplicate key")
}

// NewProxyUseCase создает новый use case для прокси. userProxyRepo опционален: если задан, при выдаче free-прокси исключаются уже выданные этому пользователю.
func NewProxyUseCase(proxyRepo repository.ProxyRepository, userProxyRepo repository.UserProxyRepository) ProxyUseCase {
	return &proxyUseCase{proxyRepo: proxyRepo, userProxyRepo: userProxyRepo}
}

func (uc *proxyUseCase) GetProxyForUser(user *domain.User, preferFree bool) (*domain.ProxyNode, error) {
	if !preferFree && user.IsPremiumActive() {
		// Сначала выдаём персональный премиум-прокси пользователя (если есть)
		own, err := uc.proxyRepo.GetByOwnerID(user.ID)
		if err != nil {
			return nil, err
		}
		if own != nil && own.Status == domain.ProxyStatusActive {
			own.Load++
			_ = uc.proxyRepo.Update(own)
			return own, nil
		}
	}

	var proxyType domain.ProxyType
	if preferFree || !user.IsPremiumActive() {
		proxyType = domain.ProxyTypeFree
	} else {
		proxyType = domain.ProxyTypePremium
	}

	proxies, err := uc.proxyRepo.GetAvailableByType(proxyType)
	if err != nil {
		return nil, err
	}

	// Для free-прокси исключаем те, что пользователь уже получал (не выдаём один и тот же бесплатный прокси повторно).
	if proxyType == domain.ProxyTypeFree && uc.userProxyRepo != nil {
		issued, errIssued := uc.userProxyRepo.ListByUserID(user.ID)
		if errIssued == nil {
			issuedSet := make(map[string]struct{})
			for _, up := range issued {
				if up.ProxyType == domain.ProxyTypeFree {
					issuedSet[fmt.Sprintf("%s:%d", up.IP, up.Port)] = struct{}{}
				}
			}
			var filtered []*domain.ProxyNode
			for _, p := range proxies {
				if _, ok := issuedSet[fmt.Sprintf("%s:%d", p.IP, p.Port)]; !ok {
					filtered = append(filtered, p)
				}
			}
			proxies = filtered
			if len(proxies) == 0 {
				return nil, ErrNoMoreFreeProxiesForUser
			}
		}
	}

	if len(proxies) == 0 {
		return nil, fmt.Errorf("no available %s proxies", proxyType)
	}

	selectedProxy := proxies[0]
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

	err := uc.proxyRepo.Create(proxy)
	if err != nil && isDuplicateKeyError(err) {
		existing, _ := uc.proxyRepo.GetByIPPortSecret(ip, port, secret)
		if existing != nil {
			return fmt.Errorf("такой прокси уже добавлен (ID %d). Список: /proxies", existing.ID)
		}
		return fmt.Errorf("такой прокси уже есть в базе")
	}
	return err
}

func (uc *proxyUseCase) DeleteProxy(id uint) error {
	p, err := uc.proxyRepo.GetByID(id)
	if err != nil || p == nil {
		return fmt.Errorf("proxy not found")
	}
	// Очищаем записи «Мои прокси» по этому точному прокси (ip, port, secret).
	if uc.userProxyRepo != nil {
		_ = uc.userProxyRepo.DeleteByIPPortSecret(p.IP, p.Port, p.Secret)
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
	start := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", proxy.IP, proxy.Port), timeout)
	if err != nil {
		proxy.Status = domain.ProxyStatusInactive
		now := time.Now()
		proxy.LastCheck = &now
		proxy.LastRTTMs = nil
		return uc.proxyRepo.Update(proxy)
	}
	defer conn.Close()

	rttMs := int(time.Since(start).Milliseconds())
	proxy.LastRTTMs = &rttMs
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
	activePremium, err := uc.proxyRepo.CountActivePremium()
	if err != nil {
		return ProxyStats{}, err
	}
	unreachablePremium, err := uc.proxyRepo.CountUnreachablePremium()
	if err != nil {
		return ProxyStats{}, err
	}
	allProxies, err := uc.proxyRepo.GetAll()
	if err != nil {
		return ProxyStats{}, err
	}
	var freeCount int64
	for _, p := range allProxies {
		if p.Type == domain.ProxyTypeFree {
			freeCount++
		}
	}
	return ProxyStats{
		TotalProxies:           total,
		ActiveProxies:          active,
		FreeProxies:            freeCount,
		PremiumProxies:         activePremium,
		UnreachablePremiumCount: unreachablePremium,
	}, nil
}

func (uc *proxyUseCase) HasAvailableFreeProxy() (bool, error) {
	proxies, err := uc.proxyRepo.GetAvailableByType(domain.ProxyTypeFree)
	if err != nil {
		return false, err
	}
	return len(proxies) > 0, nil
}

func (uc *proxyUseCase) GetActivePremiumProxies() ([]*domain.ProxyNode, error) {
	return uc.proxyRepo.GetActivePremiumProxies()
}

func (uc *proxyUseCase) GetUnreachablePremiumProxies() ([]*domain.ProxyNode, error) {
	return uc.proxyRepo.GetUnreachablePremiumProxies()
}

// CheckPremiumProxy выполняет TCP-проверку премиум-прокси. При неудаче выставляет UnreachableSince (Status не меняет);
// при успехе сбрасывает UnreachableSince и выставляет Status = active.
func (uc *proxyUseCase) CheckPremiumProxy(proxy *domain.ProxyNode) (reachable bool, err error) {
	timeout := 5 * time.Second
	start := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", proxy.IP, proxy.Port), timeout)
	if err != nil {
		now := time.Now()
		// Устанавливаем UnreachableSince только при первом переходе в недоступное состояние, чтобы сохранить время первой потери связи.
		if proxy.UnreachableSince == nil {
			proxy.UnreachableSince = &now
		}
		proxy.LastCheck = &now
		proxy.LastRTTMs = nil
		_ = uc.proxyRepo.Update(proxy)
		return false, nil
	}
	conn.Close()
	rttMs := int(time.Since(start).Milliseconds())
	proxy.LastRTTMs = &rttMs
	proxy.UnreachableSince = nil
	if proxy.Status != domain.ProxyStatusActive {
		proxy.Status = domain.ProxyStatusActive
	}
	now := time.Now()
	proxy.LastCheck = &now
	if err := uc.proxyRepo.Update(proxy); err != nil {
		return true, err
	}
	return true, nil
}

// generatePremiumSecret возвращает секрет для mtg: префикс "dd" + 32 символа HEX (итого 34).
func generatePremiumSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dd" + hex.EncodeToString(b), nil
}

// EnsurePremiumProxyForUser гарантирует персональный премиум-прокси: генерирует секрет (dd+32 hex),
// выбирает свободный порт в [20000, 30000], создаёт или обновляет запись в proxy_nodes с serverIP.
func (uc *proxyUseCase) EnsurePremiumProxyForUser(user *domain.User, serverIP string) (*domain.ProxyNode, error) {
	if user == nil {
		return nil, errors.New("user is nil")
	}
	if serverIP == "" || net.ParseIP(serverIP) == nil {
		return nil, errors.New("invalid server IP address")
	}

	existing, err := uc.proxyRepo.GetByOwnerID(user.ID)
	if err != nil {
		log.Printf("[Premium proxy] GetByOwnerID user_id=%d: %v", user.ID, err)
		return nil, err
	}

	if existing != nil {
		existing.Type = domain.ProxyTypePremium
		existing.Status = domain.ProxyStatusActive
		existing.IP = serverIP
		if err := uc.proxyRepo.Update(existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	secret, err := generatePremiumSecret()
	if err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}

	const (
		minPort    = 20000
		maxPort    = 30000
		maxRetries = 5
	)

	ownerID := user.ID

	for attempt := 0; attempt < maxRetries; attempt++ {
		port, err := uc.proxyRepo.FindFirstFreePort(minPort, maxPort)
		if err != nil {
			log.Printf("[Premium proxy] FindFirstFreePort attempt=%d: %v", attempt+1, err)
			return nil, fmt.Errorf("failed to find free port: %w", err)
		}

		proxy := &domain.ProxyNode{
			IP:      serverIP,
			Port:    port,
			Secret:  secret,
			Type:    domain.ProxyTypePremium,
			Status:  domain.ProxyStatusActive,
			Load:    0,
			OwnerID: &ownerID,
		}

		if err := uc.proxyRepo.Create(proxy); err != nil {
			if isDuplicateKeyError(err) {
				continue
			}
			log.Printf("[Premium proxy] Create proxy failed: %v", err)
			return nil, err
		}

		return proxy, nil
	}

	log.Printf("[Premium proxy] failed to allocate port after %d retries for user_id=%d", maxRetries, user.ID)
	return nil, fmt.Errorf("failed to allocate unique port after %d retries", maxRetries)
}

func (uc *proxyUseCase) GetAllFreeProxies() ([]*domain.ProxyNode, error) {
	all, err := uc.proxyRepo.GetAll()
	if err != nil {
		return nil, err
	}
	var free []*domain.ProxyNode
	for _, p := range all {
		if p.Type == domain.ProxyTypeFree {
			free = append(free, p)
		}
	}
	return free, nil
}

func (uc *proxyUseCase) CheckFreeProxy(proxy *domain.ProxyNode) (reachable bool, rttMs int) {
	timeout := 5 * time.Second
	start := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", proxy.IP, proxy.Port), timeout)
	now := time.Now()
	proxy.LastCheck = &now

	if err != nil {
		proxy.Status = domain.ProxyStatusInactive
		proxy.LastRTTMs = nil
		_ = uc.proxyRepo.Update(proxy)
		return false, 0
	}
	conn.Close()

	ms := int(time.Since(start).Milliseconds())
	proxy.LastRTTMs = &ms
	proxy.Status = domain.ProxyStatusActive
	_ = uc.proxyRepo.Update(proxy)
	return true, ms
}
