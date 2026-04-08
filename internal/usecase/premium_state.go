package usecase

import "strings"

func IsTimewebFloatingIDSet(v string) bool {
	s := strings.TrimSpace(v)
	return s != "" && s != "0"
}
