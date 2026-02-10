package usecase

import (
	"errors"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
)

// UserUseCase определяет бизнес-логику для работы с пользователями
type UserUseCase interface {
	GetOrCreateUser(tgID int64) (*domain.User, error)
	ActivatePremium(tgID int64, durationDays int) error
	RevokePremium(tgID int64) error
	CheckExpiredPremiums() error
}

type userUseCase struct {
	userRepo repository.UserRepository
}

// NewUserUseCase создает новый use case для пользователей
func NewUserUseCase(userRepo repository.UserRepository) UserUseCase {
	return &userUseCase{userRepo: userRepo}
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

	premiumUntil := time.Now().AddDate(0, 0, durationDays)
	user.IsPremium = true
	user.PremiumUntil = &premiumUntil

	return uc.userRepo.Update(user)
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
	user.PremiumUntil = nil

	return uc.userRepo.Update(user)
}

func (uc *userUseCase) CheckExpiredPremiums() error {
	users, err := uc.userRepo.GetPremiumUsers()
	if err != nil {
		return err
	}

	now := time.Now()
	for _, user := range users {
		if user.PremiumUntil != nil && user.PremiumUntil.Before(now) {
			if err := uc.RevokePremium(user.TGID); err != nil {
				// Логируем ошибку, но продолжаем обработку других пользователей
				continue
			}
		}
	}

	return nil
}
