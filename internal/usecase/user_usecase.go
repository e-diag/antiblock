package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
)

// ErrPremiumProxyCreationFailed возвращается из ActivatePremium, если персональный прокси
// не удалось создать после всех попыток (премиум в БД уже выдан).
var ErrPremiumProxyCreationFailed = errors.New("premium proxy creation failed after retries")

// UserUseCase определяет бизнес-логику для работы с пользователями
type UserUseCase interface {
	GetOrCreateUser(tgID int64) (*domain.User, error)
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
	userRepo        repository.UserRepository
	proxyRepo       repository.ProxyRepository
	proxyUC         ProxyUseCase
	dockerMgr       *docker.Manager
	premiumServerIP string // IP премиум-сервера для записи в proxy_nodes; пусто — персональные прокси не создаём
}

// NewUserUseCase создает новый use case для пользователей.
// premiumServerIP — IP сервера для персональных премиум-прокси (если пусто, создание прокси/контейнеров не выполняется).
func NewUserUseCase(userRepo repository.UserRepository, proxyRepo repository.ProxyRepository, proxyUC ProxyUseCase, dockerMgr *docker.Manager, premiumServerIP string) UserUseCase {
	return &userUseCase{
		userRepo:        userRepo,
		proxyRepo:       proxyRepo,
		proxyUC:         proxyUC,
		dockerMgr:       dockerMgr,
		premiumServerIP: premiumServerIP,
	}
}

func (uc *userUseCase) GetOrCreateUser(tgID int64) (*domain.User, error) {
	user, err := uc.userRepo.GetByTGID(tgID)
	if err != nil {
		return nil, err
	}

	if user == nil {
		user = &domain.User{
			TGID:      tgID,
			IsPremium: false,
		}
		if err := uc.userRepo.Create(user); err != nil {
			return nil, err
		}
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
	user.PremiumReminderSentAt = nil // сброс, чтобы пользователь снова мог получить напоминание за 7 дней до следующего окончания

	if err := uc.userRepo.Update(user); err != nil {
		return err
	}

	// Создаём персональный премиум-прокси и контейнер (до 3 попыток). При неудаче премиум уже выдан — возвращаем ErrPremiumProxyCreationFailed.
	if uc.premiumServerIP != "" && uc.proxyUC != nil {
		const maxAttempts = 3
		var lastErr error
		for attempt := 0; attempt < maxAttempts; attempt++ {
			_, err := uc.proxyUC.EnsurePremiumProxyForUser(user, uc.premiumServerIP)
			if err != nil {
				lastErr = err
				continue
			}
			if uc.dockerMgr != nil {
				if err := uc.ensurePremiumContainer(tgID, user); err != nil {
					lastErr = err
					continue
				}
			}
			return nil
		}
		return fmt.Errorf("%w: %v", ErrPremiumProxyCreationFailed, lastErr)
	}

	return nil
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
	if uc.proxyRepo != nil {
		_ = uc.proxyRepo.DeactivateUserProxy(user.ID)
	}
	if uc.dockerMgr != nil {
		name := fmt.Sprintf(docker.UserContainerName, tgID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = uc.dockerMgr.RemoveUserContainer(ctx, name)
		cancel()
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
	if uc.premiumServerIP == "" || uc.proxyUC == nil {
		return nil, errors.New("premium proxy not configured")
	}
	proxy, err := uc.proxyUC.EnsurePremiumProxyForUser(user, uc.premiumServerIP)
	if err != nil {
		return nil, err
	}
	if uc.dockerMgr != nil {
		if err := uc.ensurePremiumContainer(tgID, user); err != nil {
			return nil, err
		}
	}
	return proxy, nil
}

func (uc *userUseCase) CheckExpiredPremiums() error {
	users, err := uc.userRepo.GetPremiumUsers()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, user := range users {
		if user.PremiumUntil != nil && user.PremiumUntil.Before(now) {
			// Деактивируем персональный премиум-прокси пользователя (если есть)
			if uc.proxyRepo != nil {
				_ = uc.proxyRepo.DeactivateUserProxy(user.ID)
			}

			// Останавливаем и удаляем Docker‑контейнер пользователя
			if uc.dockerMgr != nil {
				name := fmt.Sprintf(docker.UserContainerName, user.TGID)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = uc.dockerMgr.RemoveUserContainer(ctx, name)
				cancel()
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

	// Дополнительно пытаемся удалить Docker‑контейнеры для "заброшенных" подписок.
	if uc.dockerMgr != nil {
		users, err := uc.userRepo.GetAll()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			for _, u := range users {
				if u.PremiumUntil != nil && u.PremiumUntil.Before(cutoff) {
					name := fmt.Sprintf(docker.UserContainerName, u.TGID)
					_ = uc.dockerMgr.RemoveUserContainer(ctx, name)
				}
			}
			cancel()
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
	if user == nil || uc.proxyRepo == nil || uc.dockerMgr == nil {
		return nil
	}

	proxy, err := uc.proxyRepo.GetByOwnerID(user.ID)
	if err != nil || proxy == nil {
		return err
	}

	name := fmt.Sprintf(docker.UserContainerName, tgID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	running, err := uc.dockerMgr.IsContainerRunning(ctx, name)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	return uc.dockerMgr.CreateUserContainer(ctx, tgID, proxy)
}
