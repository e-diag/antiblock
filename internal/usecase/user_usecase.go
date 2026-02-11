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

// UserUseCase определяет бизнес-логику для работы с пользователями
type UserUseCase interface {
	GetOrCreateUser(tgID int64) (*domain.User, error)
	ActivatePremium(tgID int64, durationDays int) error
	RevokePremium(tgID int64) error
	CheckExpiredPremiums() error
	// CleanupExpiredProxies очищает персональные прокси, у которых подписка истекла более graceDays дней назад.
	CleanupExpiredProxies(graceDays int) error
}

type userUseCase struct {
	userRepo  repository.UserRepository
	proxyRepo repository.ProxyRepository
	dockerMgr *docker.Manager
}

// NewUserUseCase создает новый use case для пользователей
func NewUserUseCase(userRepo repository.UserRepository, proxyRepo repository.ProxyRepository, dockerMgr *docker.Manager) UserUseCase {
	return &userUseCase{
		userRepo:  userRepo,
		proxyRepo: proxyRepo,
		dockerMgr: dockerMgr,
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
	premiumUntil := now.AddDate(0, 0, durationDays)
	user.IsPremium = true
	user.PremiumUntil = &premiumUntil
	user.LastActiveAt = &now

	if err := uc.userRepo.Update(user); err != nil {
		return err
	}

	// При каждом продлении/выдаче премиума проверяем и при необходимости создаём Docker‑контейнер.
	if uc.dockerMgr != nil && uc.proxyRepo != nil {
		_ = uc.ensurePremiumContainer(tgID, user)
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

	user.IsPremium = false
	// premium_until сохраняем как дату окончания, она нужна для очистки
	// через 60+ дней, поэтому не обнуляем поле.

	return uc.userRepo.Update(user)
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
