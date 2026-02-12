package repository

import (
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// UserRepository определяет интерфейс для работы с пользователями
type UserRepository interface {
	Create(user *domain.User) error
	GetByTGID(tgID int64) (*domain.User, error)
	Update(user *domain.User) error
	GetAll() ([]*domain.User, error)
	GetPremiumUsers() ([]*domain.User, error)
	GetUsersForPremiumReminder(daysFrom, daysTo int) ([]*domain.User, error) // premium_until через daysFrom–daysTo дней, напоминание ещё не отправлялось
	Count() (int64, error)
}

type userRepository struct {
	db *gorm.DB
}

// NewUserRepository создает новый репозиторий пользователей
func NewUserRepository(db *gorm.DB) UserRepository {
	return &userRepository{db: db}
}

func (r *userRepository) Create(user *domain.User) error {
	return r.db.Create(user).Error
}

func (r *userRepository) GetByTGID(tgID int64) (*domain.User, error) {
	var user domain.User
	err := r.db.Where("tg_id = ?", tgID).First(&user).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *userRepository) Update(user *domain.User) error {
	return r.db.Save(user).Error
}

func (r *userRepository) GetAll() ([]*domain.User, error) {
	var users []*domain.User
	err := r.db.Find(&users).Error
	return users, err
}

func (r *userRepository) GetPremiumUsers() ([]*domain.User, error) {
	var users []*domain.User
	err := r.db.Where("is_premium = ?", true).Find(&users).Error
	return users, err
}

// GetUsersForPremiumReminder возвращает пользователей с активным премиумом, у которых premium_until
// попадает в интервал [now+daysFrom, now+daysTo] и напоминание ещё не отправлялось (premium_reminder_sent_at IS NULL).
func (r *userRepository) GetUsersForPremiumReminder(daysFrom, daysTo int) ([]*domain.User, error) {
	var users []*domain.User
	from := time.Now().UTC().AddDate(0, 0, daysFrom)
	to := time.Now().UTC().AddDate(0, 0, daysTo)
	err := r.db.Where("is_premium = ? AND premium_until IS NOT NULL AND premium_until >= ? AND premium_until <= ? AND premium_reminder_sent_at IS NULL",
		true, from, to).Find(&users).Error
	return users, err
}

func (r *userRepository) Count() (int64, error) {
	var count int64
	err := r.db.Model(&domain.User{}).Count(&count).Error
	return count, err
}
