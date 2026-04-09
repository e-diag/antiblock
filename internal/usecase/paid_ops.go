package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
)

// Ключи app_settings: одноразовые операции для платных тарифов.
const (
	SettingPaidMigrationDDToEEV1   = "paid_migration_dd_to_ee_v1"
	SettingPaidCompensation14dV1   = "paid_compensation_14d_v1"
)

var (
	ErrPaidMigrationAlreadyDone    = errors.New("миграция dd→ee (v1 маркер) уже отмечена как выполненная")
	ErrPaidCompensationAlreadyDone = errors.New("компенсация +14 дней уже была начислена")
)

// PaidOps одноразовые операции миграции прокси и компенсации подписок (без циклических импортов с handler).
type PaidOps struct {
	Settings        repository.SettingsRepository
	Users           repository.UserRepository
	Proxies         repository.ProxyRepository
	UserProxies     repository.UserProxyRepository
	Subs            repository.ProSubscriptionRepository
	ProxyUC         ProxyUseCase
	ProUC           ProUseCase
	Docker          *docker.Manager
	PremiumServerIP string
	Provisioner     *PremiumProvisioner
	Locker          *OpsLocker
	LockOwner       string
}

func (p *PaidOps) effectiveLockOwner() string {
	if p != nil && strings.TrimSpace(p.LockOwner) != "" {
		return strings.TrimSpace(p.LockOwner)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return host + ":" + strconv.Itoa(os.Getpid()) + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// MigratePaidProxiesToEE (v1) — устаревший одношотовый прогон; сохранён для совместимости маркера в БД.
// Актуальная пошаговая миграция: MigrationV2OneStep + paidops -migrate-paid-ee-v2-step.
func (p *PaidOps) MigratePaidProxiesToEE(ctx context.Context) error {
	if p == nil || p.Settings == nil {
		return fmt.Errorf("settings required")
	}
	if v, _ := p.Settings.Get(SettingPaidMigrationDDToEEV1); v == "done" {
		return ErrPaidMigrationAlreadyDone
	}
	return fmt.Errorf("устарело: используйте пошаговую миграцию v2 (paidops -migrate-paid-ee-v2-step / -migrate-paid-ee-v2-daemon)")
}

// isLegacyPremiumProxy экспортируется для тестов пакета.
func isLegacyPremiumProxy(p *domain.ProxyNode) bool {
	if p == nil {
		return false
	}
	fip := strings.TrimSpace(p.TimewebFloatingIPID)
	if fip != "" && fip != "0" {
		return false
	}
	// TimeWeb премиум использует фиксированные порты 8443/443.
	// Legacy premium (Docker на Pro-сервере) живёт в диапазоне 20000+.
	if p.Port != domain.PremiumPortEE1 && p.Port != domain.PremiumPortEE2 {
		return true
	}
	if p.PremiumServerID != nil && *p.PremiumServerID != 0 {
		return false
	}
	return true
}
