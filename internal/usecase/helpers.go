package usecase

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// isDuplicateKeyError определяет, является ли ошибка нарушением уникального ограничения.
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
