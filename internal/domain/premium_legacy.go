package domain

import "strings"

// IsLegacyPremiumProxy — премиум на Pro Docker (без привязки к TimeWeb floating IP).
// Совпадает с логикой user_usecase.isLegacyPremiumRecord.
func IsLegacyPremiumProxy(p *ProxyNode) bool {
	if p == nil || p.Type != ProxyTypePremium {
		return false
	}
	s := strings.TrimSpace(p.TimewebFloatingIPID)
	if s != "" && s != "0" {
		return false
	}
	if p.Port != PremiumPortEE1 && p.Port != PremiumPortEE2 {
		return true
	}
	if p.PremiumServerID != nil && *p.PremiumServerID != 0 {
		return false
	}
	return true
}
