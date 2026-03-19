package usecase

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
)

// ErrPremiumProxyCreationFailed возвращается из ActivatePremium, если персональный прокси
// не удалось создать после всех попыток (премиум в БД уже выдан).
var ErrPremiumProxyCreationFailed = errors.New("premium proxy creation failed after retries")

// ErrProvisionerNotConfigured — TimeWeb API не настроен, новый Premium невозможен.
var ErrProvisionerNotConfigured = errors.New("provisioner not configured")

// UserUseCase определяет бизнес-логику для работы с пользователями
type UserUseCase interface {
	GetOrCreateUser(tgID int64, username string) (*domain.User, error)
	ActivatePremium(tgID int64, durationDays int) error
	// RetryPremiumProxyCreation повторно пытается создать прокси и контейнер для премиум-пользователя (для кнопки «Повторить»).
	RetryPremiumProxyCreation(tgID int64) (*domain.ProxyNode, error)
	RevokePremium(tgID int64) error
	CheckExpiredPremiums() error
	CleanupExpiredProxies(graceDays int) error
	GetUsersForPremiumReminder() ([]*domain.User, error)
	MarkPremiumReminderSent(tgID int64) error
}

type userUseCase struct {
	userRepo              repository.UserRepository
	proxyRepo             repository.ProxyRepository
	proxyUC               ProxyUseCase
	dockerMgr             *docker.Manager
	premiumServerIP       string
	userProxyRepo         repository.UserProxyRepository
	premiumProvisioner    *PremiumProvisioner
	appCtx                context.Context
	onPremiumProxyReady   func(tgID int64, proxy *domain.ProxyNode) // при успешном создании контейнера
	onPremiumProxyFailed  func(tgID int64, err error)               // при неудаче после всех попыток
	onPremiumVPSRequested func(req *domain.VPSProvisionRequest)     // уведомить админов о необходимости нового Premium VPS
}

// NewUserUseCase создает новый use case для пользователей.
// premiumServerIP — IP сервера для персональных премиум-прокси (если пусто, создание прокси/контейнеров не выполняется).
func NewUserUseCase(
	userRepo repository.UserRepository,
	proxyRepo repository.ProxyRepository,
	proxyUC ProxyUseCase,
	dockerMgr *docker.Manager,
	premiumServerIP string,
	userProxyRepo repository.UserProxyRepository,
	premiumProvisioner *PremiumProvisioner,
	appCtx context.Context,
) UserUseCase {
	if appCtx == nil {
		appCtx = context.Background()
	}
	return &userUseCase{
		userRepo:           userRepo,
		proxyRepo:          proxyRepo,
		proxyUC:            proxyUC,
		dockerMgr:          dockerMgr,
		premiumServerIP:    premiumServerIP,
		userProxyRepo:      userProxyRepo,
		premiumProvisioner: premiumProvisioner,
		appCtx:             appCtx,
	}
}

// SetOnPremiumProxyReady задаёт callback, вызываемый после успешного асинхронного создания премиум-прокси (например, отправка сообщения пользователю). Можно вызвать после создания бота в main.
func SetOnPremiumProxyReady(uc UserUseCase, f func(tgID int64, proxy *domain.ProxyNode)) {
	if u, ok := uc.(*userUseCase); ok {
		u.onPremiumProxyReady = f
	}
}

// SetOnPremiumProxyFailed задаёт callback, вызываемый при неудаче создания премиум-прокси после всех попыток (уведомление пользователя).
func SetOnPremiumProxyFailed(uc UserUseCase, f func(tgID int64, err error)) {
	if u, ok := uc.(*userUseCase); ok {
		u.onPremiumProxyFailed = f
	}
}

// SetOnPremiumVPSRequested уведомляет админов о необходимости создания нового Premium VPS.
func SetOnPremiumVPSRequested(uc UserUseCase, f func(req *domain.VPSProvisionRequest)) {
	if u, ok := uc.(*userUseCase); ok {
		u.onPremiumVPSRequested = f
	}
}

func (uc *userUseCase) GetOrCreateUser(tgID int64, username string) (*domain.User, error) {
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil {
		return nil, err
	}

	if user == nil {
		user = &domain.User{
			TGID:      tgID,
			Username:  username,
			IsPremium: false,
		}
		if err := uc.userRepo.Create(user); err != nil {
			return nil, err
		}
		return user, nil
	}

	// Обновляем username только если пришло НЕ пустое значение и оно изменилось,
	// чтобы не затирать уже сохранённый username пустой строкой.
	if username != "" && user.Username != username {
		user.Username = username
		_ = uc.userRepo.Update(user)
	}
	return user, nil
}

func (uc *userUseCase) ActivatePremium(tgID int64, durationDays int) error {
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil {
		return err
	}
	if user == nil {
		return errors.New("user not found")
	}

	// Подписка на durationDays (например 30). При повторной оплате (Stars или xRocket) — добавляем +durationDays к текущей дате окончания.
	now := time.Now().UTC()
	var premiumUntil time.Time
	if user.PremiumUntil != nil && user.PremiumUntil.After(now) {
		premiumUntil = user.PremiumUntil.AddDate(0, 0, durationDays)
	} else {
		premiumUntil = now.AddDate(0, 0, durationDays)
	}
	user.IsPremium = true
	user.PremiumUntil = &premiumUntil
	user.LastActiveAt = &now
	user.PremiumReminderSentAt = nil

	if err := uc.userRepo.Update(user); err != nil {
		return err
	}

	go uc.activatePremiumContainerAsync(tgID, user)
	return nil
}

func (uc *userUseCase) isLegacyPremiumRecord(p *domain.ProxyNode) bool {
	if p == nil || p.Type != domain.ProxyTypePremium {
		return false
	}
	fip := strings.TrimSpace(p.TimewebFloatingIPID)
	if fip != "" && fip != "0" {
		return false
	}
	if p.PremiumServerID != nil && *p.PremiumServerID != 0 {
		return false
	}
	return true
}

func (uc *userUseCase) isLegacyPremiumActive(p *domain.ProxyNode) bool {
	return uc.isLegacyPremiumRecord(p) && p.Status == domain.ProxyStatusActive
}

// activatePremiumContainerAsync: новый Premium через TimeWeb; legacy с активной подпиской — только уведомление;
// после истечения legacy — как первая покупка через TimeWeb.
func (uc *userUseCase) activatePremiumContainerAsync(tgID int64, user *domain.User) {
	existing, _ := uc.proxyRepo.GetByOwnerID(user.ID)
	if existing != nil && existing.Type != domain.ProxyTypePremium {
		existing = nil
	}

	if uc.premiumProvisioner != nil {
		ctx, cancel := context.WithTimeout(uc.appCtx, 15*time.Minute)
		defer cancel()

		if existing != nil && existing.TimewebFloatingIPID != "" {
			log.Printf("[Premium] renewal new-style tg_id=%d floating_ip=%s", tgID, existing.FloatingIP)
			if err := uc.premiumProvisioner.RestartContainersForUser(ctx, user, existing); err != nil {
				log.Printf("[Premium] RestartContainers tg_id=%d: %v (non-fatal)", tgID, err)
			}
			if uc.onPremiumProxyReady != nil {
				uc.onPremiumProxyReady(tgID, existing)
			}
			return
		}

		if existing != nil && existing.PremiumServerID != nil && *existing.PremiumServerID != 0 && existing.TimewebFloatingIPID == "" {
			proxy, err := uc.premiumProvisioner.ProvisionExistingProxyForUser(ctx, user, existing)
			if err != nil {
				if errors.Is(err, ErrFloatingIPDailyLimit) {
					_ = uc.enqueueUserForNewServer(tgID)
					if uc.onPremiumProxyFailed != nil {
						uc.onPremiumProxyFailed(tgID, ErrFloatingIPDailyLimit)
					}
					return
				}
				if uc.onPremiumProxyFailed != nil {
					uc.onPremiumProxyFailed(tgID, err)
				}
				return
			}
			if err := uc.upsertPremiumProxy(proxy); err != nil {
				log.Printf("[Premium] upsertPremiumProxy tg_id=%d: %v", tgID, err)
			}
			if uc.onPremiumProxyReady != nil {
				uc.onPremiumProxyReady(tgID, proxy)
			}
			return
		}

		if uc.isLegacyPremiumActive(existing) {
			if uc.onPremiumProxyReady != nil {
				uc.onPremiumProxyReady(tgID, existing)
			}
			return
		}

		if existing != nil && uc.isLegacyPremiumRecord(existing) {
			log.Printf("[Premium] legacy upgrade tg_id=%d → new Premium with floating IP", tgID)
			existing.Status = domain.ProxyStatusInactive
			_ = uc.proxyRepo.Update(existing)
		}

		secretDD, err := generatePremiumSecret()
		if err != nil {
			log.Printf("[Premium] generatePremiumSecret tg_id=%d: %v", tgID, err)
			if uc.onPremiumProxyFailed != nil {
				uc.onPremiumProxyFailed(tgID, err)
			}
			return
		}

		proxy, err := uc.premiumProvisioner.ProvisionForUser(ctx, user, secretDD)
		if err != nil {
			if proxy != nil {
				if errUp := uc.upsertPremiumProxy(proxy); errUp != nil {
					log.Printf("[Premium] save placeholder proxy tg_id=%d: %v", tgID, errUp)
				}
			}
			if errors.Is(err, ErrFloatingIPDailyLimit) {
				_ = uc.enqueueUserForNewServer(tgID)
			}
			log.Printf("[Premium] ProvisionForUser tg_id=%d: %v", tgID, err)
			if uc.onPremiumProxyFailed != nil {
				uc.onPremiumProxyFailed(tgID, err)
			}
			return
		}

		if err := uc.upsertPremiumProxy(proxy); err != nil {
			log.Printf("[Premium] save proxy tg_id=%d: %v", tgID, err)
			if uc.onPremiumProxyFailed != nil {
				uc.onPremiumProxyFailed(tgID, err)
			}
			return
		}
		if uc.onPremiumProxyReady != nil {
			uc.onPremiumProxyReady(tgID, proxy)
		}
		return
	}

	// TimeWeb не настроен
	if uc.isLegacyPremiumActive(existing) {
		if uc.onPremiumProxyReady != nil {
			uc.onPremiumProxyReady(tgID, existing)
		}
		return
	}

	log.Printf("[Premium] provisioner not configured, tg_id=%d queued", tgID)
	_ = uc.enqueueUserForNewServer(tgID)
	if uc.onPremiumProxyFailed != nil {
		uc.onPremiumProxyFailed(tgID, ErrProvisionerNotConfigured)
	}
}

func (uc *userUseCase) RevokePremium(tgID int64) error {
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil {
		return err
	}
	if user == nil {
		return errors.New("user not found")
	}

	// При отзыве премиума менеджером — деактивируем персональный прокси и удаляем контейнер на сервере (как при истечении подписки).
	var proxy *domain.ProxyNode
	if uc.proxyRepo != nil {
		proxy, _ = uc.proxyRepo.GetByOwnerID(user.ID)
	}
	legacy := proxy != nil && uc.isLegacyPremiumRecord(proxy)
	if proxy != nil && !legacy && uc.premiumProvisioner != nil {
		depCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := uc.premiumProvisioner.DeprovisionForUser(depCtx, user, proxy); err != nil {
			log.Printf("[Premium] DeprovisionForUser user_id=%d: %v", user.ID, err)
		}
		cancel()
	} else if legacy && uc.dockerMgr != nil {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = uc.dockerMgr.RemoveUserContainer(cleanCtx, fmt.Sprintf(docker.UserContainerNameDD, user.TGID))
		_ = uc.dockerMgr.RemoveUserContainerEE(cleanCtx, user.TGID)
		cancel()
		log.Printf("[Premium] legacy containers removed for tg_id=%d", user.TGID)
	}

	if proxy != nil && uc.userProxyRepo != nil {
		ddPort, eePort := proxy.Port, proxy.Port+10000
		if !legacy {
			ddPort = domain.PremiumPortDD
			eePort = domain.PremiumPortEE
		}
		_ = uc.userProxyRepo.DeleteByIPPortSecret(proxy.IP, ddPort, proxy.Secret)
		if proxy.SecretEE != "" {
			_ = uc.userProxyRepo.DeleteByIPPortSecret(proxy.IP, eePort, proxy.SecretEE)
		}
	}
	if uc.proxyRepo != nil {
		_ = uc.proxyRepo.DeactivateUserProxy(user.ID)
	}

	user.IsPremium = false
	// premium_until сохраняем как дату окончания, она нужна для очистки через 60+ дней.

	return uc.userRepo.Update(user)
}

// RetryPremiumProxyCreation повторно создаёт прокси и контейнер для премиум-пользователя (вызов с кнопки «Повторить»).
func (uc *userUseCase) RetryPremiumProxyCreation(tgID int64) (*domain.ProxyNode, error) {
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil || user == nil {
		return nil, errors.New("user not found")
	}
	if !user.IsPremiumActive() {
		return nil, errors.New("user is not premium")
	}
	if uc.premiumProvisioner != nil {
		if uc.proxyRepo == nil {
			return nil, errors.New("premium proxy repo is nil")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		existing, _ := uc.proxyRepo.GetByOwnerID(user.ID)
		if existing == nil {
			return nil, errors.New("premium proxy not found")
		}

		if existing.TimewebFloatingIPID != "" {
			if err := uc.premiumProvisioner.RestartContainersForUser(ctx, user, existing); err != nil {
				return nil, err
			}
			return existing, nil
		}

		if existing.PremiumServerID != nil && *existing.PremiumServerID != 0 {
			proxy, err := uc.premiumProvisioner.ProvisionExistingProxyForUser(ctx, user, existing)
			if err != nil {
				if errors.Is(err, ErrFloatingIPDailyLimit) {
					_ = uc.enqueueUserForNewServer(tgID)
					if uc.onPremiumProxyFailed != nil {
						uc.onPremiumProxyFailed(tgID, ErrFloatingIPDailyLimit)
					}
				}
				return nil, err
			}
			_ = uc.upsertPremiumProxy(proxy)
			return proxy, nil
		}

		if uc.isLegacyPremiumActive(existing) && uc.dockerMgr != nil {
			if err := uc.ensurePremiumContainer(tgID, user); err != nil {
				return nil, err
			}
			return existing, nil
		}
		return nil, errors.New("настройте TimeWeb (TIMEWEB_API_TOKEN) для создания Premium")
	}

	if uc.proxyRepo == nil {
		return nil, errors.New("premium proxy repo is nil")
	}
	existing, _ := uc.proxyRepo.GetByOwnerID(user.ID)
	if existing != nil && uc.isLegacyPremiumActive(existing) && uc.dockerMgr != nil {
		if err := uc.ensurePremiumContainer(tgID, user); err != nil {
			return nil, err
		}
		return existing, nil
	}
	return nil, errors.New("настройте TimeWeb (TIMEWEB_API_TOKEN) для создания Premium")
}

func (uc *userUseCase) CheckExpiredPremiums() error {
	users, err := uc.userRepo.GetPremiumUsers()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, user := range users {
		if user.PremiumUntil != nil && user.PremiumUntil.Before(now) {
			var proxy *domain.ProxyNode
			if uc.proxyRepo != nil {
				proxy, _ = uc.proxyRepo.GetByOwnerID(user.ID)
			}

			legacy := proxy != nil && uc.isLegacyPremiumRecord(proxy)
			if proxy != nil && !legacy && uc.premiumProvisioner != nil {
				depCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := uc.premiumProvisioner.DeprovisionForUser(depCtx, user, proxy); err != nil {
					log.Printf("[Premium] DeprovisionForUser user_id=%d: %v", user.ID, err)
				}
				cancel()
			} else if legacy && uc.dockerMgr != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = uc.dockerMgr.RemoveUserContainer(ctx, fmt.Sprintf(docker.UserContainerNameDD, user.TGID))
				_ = uc.dockerMgr.RemoveUserContainerEE(ctx, user.TGID)
				cancel()
				log.Printf("[Premium] legacy containers removed for tg_id=%d", user.TGID)
			}

			if uc.userProxyRepo != nil && proxy != nil {
				ddPort, eePort := proxy.Port, proxy.Port+10000
				if !legacy {
					ddPort = domain.PremiumPortDD
					eePort = domain.PremiumPortEE
				}
				_ = uc.userProxyRepo.DeleteByIPPortSecret(proxy.IP, ddPort, proxy.Secret)
				if proxy.SecretEE != "" {
					_ = uc.userProxyRepo.DeleteByIPPortSecret(proxy.IP, eePort, proxy.SecretEE)
				}
			}
			if uc.proxyRepo != nil {
				_ = uc.proxyRepo.DeactivateUserProxy(user.ID)
			}

			user.IsPremium = false
			if err := uc.userRepo.Update(user); err != nil {
				// Логируем ошибку, но продолжаем обработку других пользователей
				continue
			}
		}
	}

	return nil
}

// CleanupExpiredProxies находит персональные премиум-прокси пользователей,
// чья подписка истекла более чем graceDays дней назад, и удаляет их (порт становится свободным).
func (uc *userUseCase) CleanupExpiredProxies(graceDays int) error {
	if uc.proxyRepo == nil {
		return nil
	}
	if graceDays <= 0 {
		graceDays = 60
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -graceDays)

	// Дополнительно пытаемся удалить контейнеры/keys для "заброшенных" подписок.
	users, err := uc.userRepo.GetAll()
	if err == nil {
		for _, u := range users {
			if u.PremiumUntil == nil || !u.PremiumUntil.Before(cutoff) {
				continue
			}

			var proxy *domain.ProxyNode
			if uc.proxyRepo != nil {
				proxy, _ = uc.proxyRepo.GetByOwnerID(u.ID)
			}
			if proxy == nil {
				continue
			}

			if !uc.isLegacyPremiumRecord(proxy) {
				if uc.premiumProvisioner != nil {
					depCtx, depCancel := context.WithTimeout(context.Background(), 30*time.Second)
					_ = uc.premiumProvisioner.DeprovisionForUser(depCtx, u, proxy)
					depCancel()
				}
				continue
			}

			if uc.dockerMgr != nil {
				depCtx, depCancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = uc.dockerMgr.RemoveUserContainer(depCtx, fmt.Sprintf(docker.UserContainerNameDD, u.TGID))
				_ = uc.dockerMgr.RemoveUserContainerEE(depCtx, u.TGID)
				depCancel()
			}
		}
	}

	return uc.proxyRepo.CleanupExpiredPremiumProxies(cutoff)
}

// GetUsersForPremiumReminder возвращает пользователей, у которых подписка истекает через 6–7 дней и напоминание ещё не отправлялось.
func (uc *userUseCase) GetUsersForPremiumReminder() ([]*domain.User, error) {
	return uc.userRepo.GetUsersForPremiumReminder(6, 7)
}

// MarkPremiumReminderSent отмечает отправку напоминания за 7 дней до окончания подписки.
func (uc *userUseCase) MarkPremiumReminderSent(tgID int64) error {
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil {
		return err
	}
	if user == nil {
		return errors.New("user not found")
	}
	now := time.Now().UTC()
	user.PremiumReminderSentAt = &now
	return uc.userRepo.Update(user)
}

// ensurePremiumContainer гарантирует, что для пользователя с активным премиумом
// запущен Docker‑контейнер mtg-user-{tg_id} с параметрами из БД.
func (uc *userUseCase) ensurePremiumContainer(tgID int64, user *domain.User) error {
	if user == nil || uc.proxyRepo == nil {
		return nil
	}
	if uc.dockerMgr == nil {
		return errors.New("Docker manager not available")
	}

	proxy, err := uc.proxyRepo.GetByOwnerID(user.ID)
	if err != nil || proxy == nil {
		log.Printf("[Premium] ensurePremiumContainer tg_id=%d: no proxy for user_id=%d: %v", tgID, user.ID, err)
		return err
	}

	name := fmt.Sprintf(docker.UserContainerNameDD, tgID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	running, err := uc.dockerMgr.IsContainerRunning(ctx, name)
	if err != nil {
		log.Printf("[Premium] ensurePremiumContainer tg_id=%d IsContainerRunning %s: %v", tgID, name, err)
		return err
	}
	if running {
		// DD уже запущен — ee контейнер попробуем поднять отдельно (не фатально).
		if proxy.SecretEE != "" {
			if err := uc.dockerMgr.CreateUserContainerEE(ctx, tgID, proxy); err != nil {
				log.Printf("[Premium] CreateUserContainerEE tg_id=%d: %v (non-fatal)", tgID, err)
			}
		}
		return nil
	}

	if err := uc.dockerMgr.CreateUserContainer(ctx, tgID, proxy); err != nil {
		return err
	}
	// После успешного создания dd-контейнера — создаём ee-контейнер (не критично).
	if proxy.SecretEE != "" {
		if err := uc.dockerMgr.CreateUserContainerEE(ctx, tgID, proxy); err != nil {
			log.Printf("[Premium] CreateUserContainerEE tg_id=%d: %v (non-fatal)", tgID, err)
		}
	}
	return nil
}

func (uc *userUseCase) enqueueUserForNewServer(tgID int64) error {
	if uc.premiumProvisioner == nil || uc.premiumProvisioner.provisionReqRepo == nil {
		return nil
	}
	reqID, isNew, err := uc.premiumProvisioner.provisionReqRepo.AppendPendingUserID(tgID)
	if err != nil {
		return err
	}
	if isNew && uc.onPremiumVPSRequested != nil {
		req, err := uc.premiumProvisioner.provisionReqRepo.GetByID(reqID)
		if err == nil && req != nil {
			uc.onPremiumVPSRequested(req)
		}
	}
	return nil
}

func (uc *userUseCase) upsertPremiumProxy(proxy *domain.ProxyNode) error {
	if proxy == nil || proxy.OwnerID == nil {
		return errors.New("proxy/owner is nil")
	}

	existing, err := uc.proxyRepo.GetByOwnerID(*proxy.OwnerID)
	if err != nil {
		return err
	}

	if existing == nil {
		return uc.proxyRepo.Create(proxy)
	}

	// Переносим актуальные поля.
	existing.IP = proxy.IP
	existing.Port = proxy.Port
	existing.Secret = proxy.Secret
	existing.SecretEE = proxy.SecretEE
	existing.Type = proxy.Type
	existing.Status = proxy.Status
	existing.Load = proxy.Load
	existing.FloatingIP = proxy.FloatingIP
	existing.TimewebFloatingIPID = proxy.TimewebFloatingIPID
	existing.PremiumServerID = proxy.PremiumServerID

	return uc.proxyRepo.Update(existing)
}
