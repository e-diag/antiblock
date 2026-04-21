package usecase

import (
	"errors"
	"testing"

	"github.com/yourusername/antiblock/internal/domain"
)

type userRepoStub struct {
	createFn    func(user *domain.User) error
	getByTGIDFn func(tgID int64) (*domain.User, error)
	updateFn    func(user *domain.User) error
}

func (s *userRepoStub) Create(user *domain.User) error {
	if s.createFn != nil {
		return s.createFn(user)
	}
	return nil
}

func (s *userRepoStub) GetByTGID(tgID int64) (*domain.User, error) {
	if s.getByTGIDFn != nil {
		return s.getByTGIDFn(tgID)
	}
	return nil, nil
}

func (s *userRepoStub) GetByID(id uint) (*domain.User, error) { return nil, nil }
func (s *userRepoStub) Update(user *domain.User) error {
	if s.updateFn != nil {
		return s.updateFn(user)
	}
	return nil
}
func (s *userRepoStub) GetAll() ([]*domain.User, error) { return nil, nil }
func (s *userRepoStub) GetPremiumUsers() ([]*domain.User, error) {
	return nil, nil
}
func (s *userRepoStub) ListPaidActiveUserIDs() ([]uint, error) { return nil, nil }
func (s *userRepoStub) GetUsersForPremiumReminder(daysFrom, daysTo int) ([]*domain.User, error) {
	return nil, nil
}
func (s *userRepoStub) Count() (int64, error) { return 0, nil }

func TestGetOrCreateUser_CreateSuccess(t *testing.T) {
	stub := &userRepoStub{}
	uc := &userUseCase{userRepo: stub}

	u, err := uc.GetOrCreateUser(123, "alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if u == nil || u.TGID != 123 || u.Username != "alice" {
		t.Fatalf("unexpected user: %+v", u)
	}
}

func TestGetOrCreateUser_DuplicateKeyFallback(t *testing.T) {
	existing := &domain.User{ID: 77, TGID: 123, Username: "alice"}
	getCalls := 0
	stub := &userRepoStub{
		getByTGIDFn: func(tgID int64) (*domain.User, error) {
			getCalls++
			if getCalls == 1 {
				return nil, nil
			}
			return existing, nil
		},
		createFn: func(user *domain.User) error {
			return errors.New(`pq: duplicate key value violates unique constraint "idx_users_tg_id"`)
		},
	}
	uc := &userUseCase{userRepo: stub}

	u, err := uc.GetOrCreateUser(123, "alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if u == nil || u.ID != existing.ID || u.TGID != existing.TGID {
		t.Fatalf("expected existing user, got: %+v", u)
	}
}

func TestGetOrCreateUser_DuplicateKeyButStillMissing_ReturnsError(t *testing.T) {
	stub := &userRepoStub{
		getByTGIDFn: func(tgID int64) (*domain.User, error) {
			return nil, nil
		},
		createFn: func(user *domain.User) error {
			return errors.New(`pq: duplicate key value violates unique constraint "idx_users_tg_id"`)
		},
	}
	uc := &userUseCase{userRepo: stub}

	_, err := uc.GetOrCreateUser(123, "alice")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetOrCreateUser_CreateNonDuplicateError_ReturnsError(t *testing.T) {
	stub := &userRepoStub{
		getByTGIDFn: func(tgID int64) (*domain.User, error) {
			return nil, nil
		},
		createFn: func(user *domain.User) error {
			return errors.New("db is down")
		},
	}
	uc := &userUseCase{userRepo: stub}

	_, err := uc.GetOrCreateUser(123, "alice")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

