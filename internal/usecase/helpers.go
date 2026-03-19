package usecase

import (
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"
)

// parsePendingUserIDs десериализует JSON-массив tg_id из строки.
func parsePendingUserIDs(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

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
