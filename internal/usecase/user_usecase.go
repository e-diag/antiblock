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

// PremiumVPSQueueReason — зачем пользователь попал в очередь (текст уведомления админам).
type PremiumVPSQueueReason string

const (
	PremiumQueueReasonFloatingIPLimit PremiumVPSQueueReason = "floating_ip_limit"
	PremiumQueueReasonNoActiveServer  PremiumVPSQueueReason = "no_active_server"
	// PremiumQueueReasonNeedSetup — нет применимого пути без VPS (например TimeWeb выключен).
	PremiumQueueReasonNeedSetup PremiumVPSQueueReason = "need_setup"
)

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
	userRepo                     repository.UserRepository
	proxyRepo                    repository.ProxyRepository
	proxyUC                      ProxyUseCase
	dockerMgr                    *docker.Manager
	premiumServerIP              string
	userProxyRepo                repository.UserProxyRepository
	premiumProvisioner           *PremiumProvisioner
	appCtx                       context.Context
	onPremiumProxyReady          func(tgID int64, proxy *domain.ProxyNode)                           // при успешном создании контейнера
	onPremiumProxyFailed         func(tgID int64, err error)                                         // при неудаче после всех попыток
	onPremiumProvisioningStarted func(tgID int64)                                                    // в начале долгой TimeWeb-настройки (сообщение «подождите»)
	onPremiumVPSRequested        func(req *domain.VPSProvisionRequest, reason PremiumVPSQueueReason) // уведомить админов о необходимости нового Premium VPS
}

// clearTimewebFloatingStateAfterDeprovision очищает stale-привязку FIP в proxy_nodes,
// чтобы при следующей активации Premium создавался новый floating IP, а не делался Restart на удалённом.
func (uc *userUseCase) clearTimewebFloatingStateAfterDeprovision(proxy *domain.ProxyNode) {
	if uc == nil || uc.proxyRepo == nil || proxy == nil {
		return
	}
	changed := false
	if strings.TrimSpace(proxy.TimewebFloatingIPID) != "" {
		proxy.TimewebFloatingIPID = ""
		changed = true
	}
	if strings.TrimSpace(proxy.FloatingIP) != "" {
		proxy.FloatingIP = ""
		changed = true
	}
	// IP для TimeWeb Premium должен быть только персональный FIP, после удаления FIP очищаем.
	if strings.TrimSpace(proxy.IP) != "" {
		proxy.IP = ""
		changed = true
	}
	if !changed {
		return
	}
	if err := uc.proxyRepo.Update(proxy); err != nil {
		log.Printf("[Premium] clearTimewebFloatingStateAfterDeprovision proxy_id=%d: %v", proxy.ID, err)
	}
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
func SetOnPremiumVPSRequested(uc UserUseCase, f func(req *domain.VPSProvisionRequest, reason PremiumVPSQueueReason)) {
	if u, ok := uc.(*userUseCase); ok {
		u.onPremiumVPSRequested = f
	}
}

// SetOnPremiumProvisioningStarted вызывается в начале асинхронной выдачи TimeWeb Premium (после оплаты), чтобы пользователь сразу видел «идёт настройка».
func SetOnPremiumProvisioningStarted(uc UserUseCase, f func(tgID int64)) {
	if u, ok := uc.(*userUseCase); ok {
		u.onPremiumProvisioningStarted = f
	}
}

// premiumTimewebProxyReadyToShow true, если прокси уже полностью выдан (повторный запрос ключей без SSH/рестарта).
func premiumTimewebProxyReadyToShow(p *domain.ProxyNode) bool {
	if p == nil || p.Type != domain.ProxyTypePremium {
		return false
	}
	fipID := strings.TrimSpace(p.TimewebFloatingIPID)
	if fipID == "" || fipID == "0" {
		return false
	}
	if p.Status != domain.ProxyStatusActive {
		return false
	}
	if strings.TrimSpace(p.Secret) == "" || strings.TrimSpace(p.SecretEE) == "" {
		return false
	}
	clientIP := strings.TrimSpace(p.FloatingIP)
	if clientIP == "" {
		clientIP = strings.TrimSpace(p.IP)
	}
	return clientIP != ""
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
	log.Printf("[Premium] ActivatePremium: start tg_id=%d duration_days=%d", tgID, durationDays)
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

	log.Printf("[Premium] ActivatePremium tg_id=%d: DB updated premium_until=%s", tgID, premiumUntil.Format(time.RFC3339))
	log.Printf("[Premium] ActivatePremium tg_id=%d: launching async activatePremiumContainerAsync", tgID)
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

// hasPremiumTimeWeb — настроен TimeWeb API (токен), можно выдавать floating IP.
func (uc *userUseCase) hasPremiumTimeWeb() bool {
	return uc.premiumProvisioner != nil && uc.premiumProvisioner.IsConfigured()
}

// migrateLegacyPremiumToTimeweb деактивирует строку legacy в БД и снимает Docker на Pro-сервере перед выдачей через TimeWeb.
func (uc *userUseCase) migrateLegacyPremiumToTimeweb(tgID int64, user *domain.User, existing *domain.ProxyNode) error {
	if existing == nil || !uc.isLegacyPremiumRecord(existing) {
		return nil
	}
	log.Printf("[Premium] migrating legacy proxy_id=%d port=%d → TimeWeb floating IP tg_id=%d user_id=%d",
		existing.ID, existing.Port, tgID, user.ID)
	existing.Status = domain.ProxyStatusInactive
	if err := uc.proxyRepo.Update(existing); err != nil {
		log.Printf("[Premium] migrate legacy tg_id=%d: deactivate proxy row failed: %v", tgID, err)
		return err
	}
	if uc.dockerMgr != nil {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = uc.dockerMgr.RemoveUserContainer(cleanCtx, fmt.Sprintf(docker.UserContainerNameDD, tgID))
		_ = uc.dockerMgr.RemoveUserContainerEE(cleanCtx, tgID)
		log.Printf("[Premium] legacy docker containers removed tg_id=%d before TimeWeb provision", tgID)
	}
	return nil
}

// premiumTimeWebActivateOrRenew — общая логика: restart / placeholder / миграция legacy + новый FIP / новая выдача.
// Должна вызываться только при uc.hasPremiumTimeWeb() && uc.proxyRepo != nil.
func (uc *userUseCase) premiumTimeWebActivateOrRenew(ctx context.Context, tgID int64, user *domain.User) (*domain.ProxyNode, error) {
	existing, err := uc.proxyRepo.GetByOwnerID(user.ID)
	if err != nil {
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: GetByOwnerID err=%v", tgID, err)
		return nil, fmt.Errorf("get premium proxy: %w", err)
	}
	if existing != nil && existing.Type != domain.ProxyTypePremium {
		existing = nil
	}

	var fipID, st string
	var psid any
	var isLeg bool
	if existing != nil {
		fipID = existing.TimewebFloatingIPID
		st = string(existing.Status)
		isLeg = uc.isLegacyPremiumRecord(existing)
		if existing.PremiumServerID != nil {
			psid = *existing.PremiumServerID
		}
	}
	log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d user_id=%d has_row=%v legacy=%v fip_id=%q premium_srv=%v status=%s",
		tgID, user.ID, existing != nil, isLeg, fipID, psid, st)

	if existing != nil && strings.TrimSpace(existing.TimewebFloatingIPID) != "" {
		if premiumTimewebProxyReadyToShow(existing) {
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: path=already active (same keys), skip SSH", tgID)
			return existing, nil
		}
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: path=RestartContainers floating_ip=%s", tgID, existing.FloatingIP)
		if err := uc.premiumProvisioner.RestartContainersForUser(ctx, user, existing); err != nil {
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: RestartContainers FAILED: %v", tgID, err)
			// Не делаем жёсткую ошибку: помечаем прокси как недоступный,
			// чтобы пользователь снова запросил и получил ожидание/те же ключи.
			existing.Status = domain.ProxyStatusInactive
			if uc.proxyRepo != nil {
				_ = uc.proxyRepo.Update(existing)
			}
			return existing, nil
		}
		return existing, nil
	}

	if existing != nil && uc.isLegacyPremiumRecord(existing) {
		if err := uc.migrateLegacyPremiumToTimeweb(tgID, user, existing); err != nil {
			return nil, err
		}
		existing, err = uc.proxyRepo.GetByOwnerID(user.ID)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.Type != domain.ProxyTypePremium {
			existing = nil
		}
	}

	if existing != nil && existing.PremiumServerID != nil && *existing.PremiumServerID != 0 &&
		strings.TrimSpace(existing.TimewebFloatingIPID) == "" && !uc.isLegacyPremiumRecord(existing) {
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: path=ProvisionExistingProxy proxy_id=%d", tgID, existing.ID)
		proxy, provErr := uc.premiumProvisioner.ProvisionExistingProxyForUser(ctx, user, existing)
		if provErr != nil {
			if errors.Is(provErr, ErrNoActivePremiumServer) {
				log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: no active premium server at ProvisionExistingProxy — enqueueing user", tgID)
				if enqErr := uc.enqueueUserForNewServer(tgID, PremiumQueueReasonNoActiveServer); enqErr != nil {
					log.Printf("[Premium] enqueueUserForNewServer tg_id=%d: %v", tgID, enqErr)
				}
				return nil, ErrNoActivePremiumServer
			}
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: ProvisionExistingProxy FAILED: %v", tgID, provErr)
			return proxy, provErr
		}
		if err := uc.upsertPremiumProxy(proxy); err != nil {
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: upsert after ProvisionExisting FAILED: %v", tgID, err)
			return proxy, fmt.Errorf("upsert premium proxy: %w", err)
		}
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: ProvisionExisting OK proxy_id=%d ip=%s", tgID, proxy.ID, proxy.IP)
		return proxy, nil
	}

	log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: path=ProvisionForUser (new secrets + floating IP)", tgID)
	secretDD, err := generatePremiumSecret()
	if err != nil {
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: generatePremiumSecret FAILED: %v", tgID, err)
		return nil, err
	}
	// EE — как в Pro: через тот же Docker API (nineseconds/mtg:2 generate-secret). Если Pro Docker выключен — ee сгенерируется на Premium VPS после EnsureDockerInstalled.
	var secretEE string
	if uc.dockerMgr != nil && uc.dockerMgr.GetClient() != nil {
		ee, errEE := uc.dockerMgr.GenerateEESecretViaDocker(ctx)
		if errEE != nil {
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: GenerateEESecretViaDocker (Pro) failed, ee на VPS: %v", tgID, errEE)
		} else {
			secretEE = ee
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: ee secret from Pro Docker (same as Pro tariff)", tgID)
		}
	}
	proxy, err := uc.premiumProvisioner.ProvisionForUser(ctx, user, secretDD, secretEE)
	if err != nil {
		if errors.Is(err, ErrNoActivePremiumServer) {
			log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: no active premium server at ProvisionForUser — enqueueing user", tgID)
			if enqErr := uc.enqueueUserForNewServer(tgID, PremiumQueueReasonNoActiveServer); enqErr != nil {
				log.Printf("[Premium] enqueueUserForNewServer tg_id=%d: %v", tgID, enqErr)
			}
			return nil, ErrNoActivePremiumServer
		}
		if proxy != nil {
			if errUp := uc.upsertPremiumProxy(proxy); errUp != nil {
				log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: save placeholder after error: %v", tgID, errUp)
			}
		}
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: ProvisionForUser FAILED: %v", tgID, err)
		return proxy, err
	}
	if err := uc.upsertPremiumProxy(proxy); err != nil {
		log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: upsert after ProvisionForUser FAILED: %v", tgID, err)
		return proxy, fmt.Errorf("upsert premium proxy: %w", err)
	}
	log.Printf("[Premium] premiumTimeWebActivateOrRenew tg_id=%d: ProvisionForUser OK proxy_id=%d ip=%s", tgID, proxy.ID, proxy.IP)
	return proxy, nil
}

// activatePremiumContainerAsync: при настроенном TimeWeb — всегда путь floating IP (в т.ч. миграция legacy);
// без TimeWeb — только legacy Docker или очередь админу.
func (uc *userUseCase) activatePremiumContainerAsync(tgID int64, user *domain.User) {
	if uc.proxyRepo == nil {
		log.Printf("[Premium] activatePremiumContainerAsync tg_id=%d: proxyRepo nil, skip", tgID)
		return
	}

	if uc.hasPremiumTimeWeb() {
		if uc.onPremiumProvisioningStarted != nil {
			uc.onPremiumProvisioningStarted(tgID)
		}
		ctx, cancel := context.WithTimeout(uc.appCtx, 15*time.Minute)
		defer cancel()
		proxy, err := uc.premiumTimeWebActivateOrRenew(ctx, tgID, user)
		if err != nil {
			if errors.Is(err, ErrFloatingIPDailyLimit) {
				_ = uc.enqueueUserForNewServer(tgID, PremiumQueueReasonFloatingIPLimit)
			}
			log.Printf("[Premium] activatePremiumContainerAsync tg_id=%d: premiumTimeWebActivateOrRenew err=%v", tgID, err)
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

	existing, _ := uc.proxyRepo.GetByOwnerID(user.ID)
	if existing != nil && existing.Type != domain.ProxyTypePremium {
		existing = nil
	}

	if uc.isLegacyPremiumActive(existing) {
		log.Printf("[Premium] activatePremiumContainerAsync tg_id=%d: TimeWeb off, legacy active — notify only", tgID)
		if uc.onPremiumProxyReady != nil {
			uc.onPremiumProxyReady(tgID, existing)
		}
		return
	}
	if existing != nil && uc.isLegacyPremiumRecord(existing) && uc.dockerMgr != nil {
		log.Printf("[Premium] activatePremiumContainerAsync tg_id=%d: TimeWeb off, legacy docker path port=%d", tgID, existing.Port)
		if err := uc.ensurePremiumContainer(tgID, user); err != nil {
			log.Printf("[Premium] activatePremiumContainerAsync tg_id=%d: ensurePremiumContainer: %v", tgID, err)
			if uc.onPremiumProxyFailed != nil {
				uc.onPremiumProxyFailed(tgID, err)
			}
			return
		}
		if uc.onPremiumProxyReady != nil {
			uc.onPremiumProxyReady(tgID, existing)
		}
		return
	}

	log.Printf("[Premium] activatePremiumContainerAsync tg_id=%d: no TimeWeb and no applicable legacy path — enqueue admin", tgID)
	_ = uc.enqueueUserForNewServer(tgID, PremiumQueueReasonNeedSetup)
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
	if proxy != nil && !legacy && uc.hasPremiumTimeWeb() {
		depCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := uc.premiumProvisioner.DeprovisionForUser(depCtx, user, proxy); err != nil {
			log.Printf("[Premium] DeprovisionForUser user_id=%d: %v", user.ID, err)
		} else {
			uc.clearTimewebFloatingStateAfterDeprovision(proxy)
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
	log.Printf("[Premium] RetryPremiumProxyCreation: start tg_id=%d", tgID)
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: GetByTGID err=%v", tgID, err)
		return nil, fmt.Errorf("get user: %w", err)
	}
	if user == nil {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: user not found in DB", tgID)
		return nil, errors.New("user not found")
	}
	if !user.IsPremiumActive() {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d user_id=%d: premium not active", tgID, user.ID)
		return nil, errors.New("user is not premium")
	}

	if uc.hasPremiumTimeWeb() {
		if uc.proxyRepo == nil {
			log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: proxyRepo is nil", tgID)
			return nil, errors.New("premium proxy repo is nil")
		}
		existing, errEx := uc.proxyRepo.GetByOwnerID(user.ID)
		if errEx != nil {
			log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: GetByOwnerID err=%v", tgID, errEx)
			return nil, fmt.Errorf("get premium proxy: %w", errEx)
		}
		if existing != nil && existing.Type != domain.ProxyTypePremium {
			existing = nil
		}
		if premiumTimewebProxyReadyToShow(existing) {
			log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: proxy already active — returning same keys", tgID)
			return existing, nil
		}
		ctx, cancel := context.WithTimeout(uc.appCtx, 15*time.Minute)
		defer cancel()
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d user_id=%d: TimeWeb path (same as activatePremiumContainerAsync)", tgID, user.ID)
		proxy, err := uc.premiumTimeWebActivateOrRenew(ctx, tgID, user)
		if err != nil {
			if errors.Is(err, ErrFloatingIPDailyLimit) {
				_ = uc.enqueueUserForNewServer(tgID, PremiumQueueReasonFloatingIPLimit)
				if uc.onPremiumProxyFailed != nil {
					uc.onPremiumProxyFailed(tgID, ErrFloatingIPDailyLimit)
				}
			}
			log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: premiumTimeWebActivateOrRenew failed: %v", tgID, err)
			return nil, err
		}
		if proxy == nil {
			log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: success but proxy is nil", tgID)
			return nil, errors.New("premium proxy missing after provision")
		}
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: OK proxy_id=%d ip=%s fip_id=%q", tgID, proxy.ID, proxy.IP, proxy.TimewebFloatingIPID)
		return proxy, nil
	}

	if uc.proxyRepo == nil {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: no TimeWeb and proxyRepo nil", tgID)
		return nil, errors.New("premium proxy repo is nil")
	}
	existing, errEx := uc.proxyRepo.GetByOwnerID(user.ID)
	if errEx != nil {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d user_id=%d: GetByOwnerID err=%v (no TimeWeb)", tgID, user.ID, errEx)
		return nil, fmt.Errorf("get premium proxy row: %w", errEx)
	}
	if existing != nil && existing.Type != domain.ProxyTypePremium {
		existing = nil
	}
	if existing == nil {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d user_id=%d: no proxy row (need row or TIMEWEB_API_TOKEN)", tgID, user.ID)
		return nil, errors.New("premium proxy not found")
	}
	if uc.isLegacyPremiumRecord(existing) && uc.dockerMgr != nil {
		log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: legacy docker fallback port=%d status=%s", tgID, existing.Port, existing.Status)
		if err := uc.ensurePremiumContainer(tgID, user); err != nil {
			log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: ensurePremiumContainer failed: %v", tgID, err)
			return nil, err
		}
		return existing, nil
	}
	log.Printf("[Premium] RetryPremiumProxyCreation tg_id=%d: no path (legacy=%v dockerMgr=%v) — set TIMEWEB_API_TOKEN",
		tgID, uc.isLegacyPremiumRecord(existing), uc.dockerMgr != nil)
	return nil, errors.New("настройте TimeWeb (TIMEWEB_API_TOKEN) для создания Premium")
}

func (uc *userUseCase) CheckExpiredPremiums() error {
	users, err := uc.userRepo.GetPremiumUsers()
	if err != nil {
		return err
	}

	log.Printf("[Premium] CheckExpiredPremiums: scanning %d premium users", len(users))
	now := time.Now().UTC()
	for _, user := range users {
		if user.PremiumUntil != nil && user.PremiumUntil.Before(now) {
			log.Printf("[Premium] CheckExpiredPremiums: tg_id=%d user_id=%d expired_at=%s — deprovisioning",
				user.TGID, user.ID, user.PremiumUntil.Format(time.RFC3339))
			var proxy *domain.ProxyNode
			if uc.proxyRepo != nil {
				proxy, _ = uc.proxyRepo.GetByOwnerID(user.ID)
			}

			legacy := proxy != nil && uc.isLegacyPremiumRecord(proxy)
			if proxy != nil && !legacy && uc.hasPremiumTimeWeb() {
				depCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := uc.premiumProvisioner.DeprovisionForUser(depCtx, user, proxy); err != nil {
					log.Printf("[Premium] DeprovisionForUser user_id=%d: %v", user.ID, err)
				} else {
					uc.clearTimewebFloatingStateAfterDeprovision(proxy)
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
				log.Printf("[Premium] CheckExpiredPremiums: tg_id=%d DB update failed: %v", user.TGID, err)
				continue
			}
			log.Printf("[Premium] CheckExpiredPremiums: tg_id=%d deprovision + DB OK", user.TGID)
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
				if uc.hasPremiumTimeWeb() {
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
		return errors.New("docker manager not available")
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
		log.Printf("[Premium] ensurePremiumContainer tg_id=%d CreateUserContainer name=%s port=%d: %v", tgID, name, proxy.Port, err)
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

func (uc *userUseCase) enqueueUserForNewServer(tgID int64, reason PremiumVPSQueueReason) error {
	if uc.premiumProvisioner == nil || uc.premiumProvisioner.provisionReqRepo == nil {
		log.Printf("[Premium] enqueueUserForNewServer tg_id=%d reason=%s: skipped (no provisioner or repo)", tgID, reason)
		return nil
	}
	reqID, isNew, err := uc.premiumProvisioner.provisionReqRepo.AppendPendingUserID(tgID)
	if err != nil {
		log.Printf("[Premium] enqueueUserForNewServer tg_id=%d reason=%s: AppendPendingUserID err=%v", tgID, reason, err)
		return err
	}
	log.Printf("[Premium] enqueueUserForNewServer tg_id=%d reason=%s: reqID=%d isNew=%v", tgID, reason, reqID, isNew)
	if isNew && uc.onPremiumVPSRequested != nil {
		req, err := uc.premiumProvisioner.provisionReqRepo.GetByID(reqID)
		if err == nil && req != nil {
			uc.onPremiumVPSRequested(req, reason)
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
