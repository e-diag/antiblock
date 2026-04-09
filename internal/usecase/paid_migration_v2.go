package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
)

const (
	SettingPaidMigrationV2State = "paid_migration_v2_state"
	SettingPaidMigrationV2Done  = "paid_migration_v2_done"
	paidOpsMigrationLockKey     = "paidops:migration:v2:step"
)

// MigrationV2State — прогресс пошаговой миграции dd→ee (resume после сбоя).
type MigrationV2State struct {
	Phase string `json:"phase"` // pro_groups | legacy_premium | timeweb_premium | done

	ProGroupIDs   []uint `json:"pro_group_ids"`
	LegacyUserIDs []uint `json:"legacy_user_ids"`
	TwUserIDs     []uint `json:"tw_user_ids"`

	ProIdx    int `json:"pro_idx"`
	LegacyIdx int `json:"legacy_idx"`
	TwIdx     int `json:"tw_idx"`

	OK            int    `json:"ok"`
	Err           int    `json:"err"`
	ErrUserSample []uint `json:"err_user_sample"`

	StartedAt    string `json:"started_at"`
	LastReportAt int64  `json:"last_report_at"` // unix seconds
}

var ErrMigrationV2AlreadyDone = errors.New("миграция v2 уже завершена (маркер paid_migration_v2_done)")

// migrationV2Load загружает состояние или создаёт новое (списки кандидатов).
func (p *PaidOps) migrationV2Load(ctx context.Context) (*MigrationV2State, error) {
	if p == nil || p.Settings == nil {
		return nil, fmt.Errorf("paid ops required")
	}
	if v, _ := p.Settings.Get(SettingPaidMigrationV2Done); v == "done" {
		return nil, ErrMigrationV2AlreadyDone
	}
	raw, _ := p.Settings.Get(SettingPaidMigrationV2State)
	if strings.TrimSpace(raw) != "" {
		var st MigrationV2State
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, fmt.Errorf("parse migration v2 state: %w", err)
		}
		if st.Phase == "" {
			st.Phase = "pro_groups"
		}
		return &st, nil
	}
	st := &MigrationV2State{
		Phase:     "pro_groups",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if p.ProUC != nil {
		groups, err := p.ProUC.GetActiveGroups()
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g != nil {
				st.ProGroupIDs = append(st.ProGroupIDs, g.ID)
			}
		}
	}
	if p.Users != nil && p.Proxies != nil {
		users, err := p.Users.GetPremiumUsers()
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		for _, u := range users {
			if u == nil || !u.IsPremiumActive() || u.PremiumUntil != nil && u.PremiumUntil.Before(now) {
				continue
			}
			proxy, err := p.Proxies.GetByOwnerID(u.ID)
			if err != nil || proxy == nil || proxy.Type != domain.ProxyTypePremium {
				continue
			}
			if isLegacyPremiumProxy(proxy) {
				st.LegacyUserIDs = append(st.LegacyUserIDs, u.ID)
			} else {
				st.TwUserIDs = append(st.TwUserIDs, u.ID)
			}
		}
	}
	return st, nil
}

func (p *PaidOps) migrationV2Save(st *MigrationV2State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return p.Settings.Set(SettingPaidMigrationV2State, string(raw))
}

// MigrationV2OneStep выполняет ровно одну единицу работы (одна Pro-группа или один Premium-пользователь).
func (p *PaidOps) MigrationV2OneStep(ctx context.Context) (*MigrationV2State, bool, error) {
	lockOwner := p.effectiveLockOwner()
	if p != nil && p.Locker != nil {
		if err := p.Locker.Acquire(paidOpsMigrationLockKey, lockOwner, 30*time.Minute); err != nil {
			return nil, false, err
		}
		defer p.Locker.Release(paidOpsMigrationLockKey, lockOwner)
	}
	st, err := p.migrationV2Load(ctx)
	if err != nil {
		return nil, false, err
	}
	if st.Phase == "done" {
		return st, false, nil
	}

	switch st.Phase {
	case "pro_groups":
		if st.ProIdx >= len(st.ProGroupIDs) {
			st.Phase = "legacy_premium"
			if err := p.migrationV2Save(st); err != nil {
				return st, true, err
			}
			return st, true, nil
		}
		gid := st.ProGroupIDs[st.ProIdx]
		g, err := p.ProUC.GetGroupByID(gid)
		if err != nil || g == nil {
			st.Err++
			p.pushErrSample(st, 0)
			log.Printf("[PaidOps] migration v2 pro group %d: not found: %v", gid, err)
		} else {
			if err := p.ProUC.MigrateOneProGroupToEEOnly(p.Docker, g); err != nil {
				st.Err++
				p.pushErrSample(st, 0)
				log.Printf("[PaidOps] migration v2 pro group %d: %v", gid, err)
			} else {
				st.OK++
			}
		}
		st.ProIdx++

	case "legacy_premium":
		if st.LegacyIdx >= len(st.LegacyUserIDs) {
			st.Phase = "timeweb_premium"
			if err := p.migrationV2Save(st); err != nil {
				return st, true, err
			}
			return st, true, nil
		}
		uid := st.LegacyUserIDs[st.LegacyIdx]
		u, err := p.Users.GetByID(uid)
		if err != nil || u == nil {
			st.Err++
			p.pushErrSample(st, uid)
		} else {
			proxy, err := p.Proxies.GetByOwnerID(u.ID)
			if err != nil || proxy == nil {
				st.Err++
				p.pushErrSample(st, uid)
			} else {
				if err := p.migrateOnePremiumOrdered(ctx, u, proxy); err != nil {
					st.Err++
					p.pushErrSample(st, uid)
					log.Printf("[PaidOps] migration v2 legacy premium user_id=%d: %v", uid, err)
				} else {
					st.OK++
				}
			}
		}
		st.LegacyIdx++

	case "timeweb_premium":
		if st.TwIdx >= len(st.TwUserIDs) {
			st.Phase = "done"
			if err := p.Settings.Set(SettingPaidMigrationV2Done, "done"); err != nil {
				return st, false, err
			}
			_ = p.migrationV2Save(st)
			return st, false, nil
		}
		uid := st.TwUserIDs[st.TwIdx]
		u, err := p.Users.GetByID(uid)
		if err != nil || u == nil {
			st.Err++
			p.pushErrSample(st, uid)
		} else {
			proxy, err := p.Proxies.GetByOwnerID(u.ID)
			if err != nil || proxy == nil {
				st.Err++
				p.pushErrSample(st, uid)
			} else {
				if err := p.migrateOneTimewebPremium(ctx, u, proxy); err != nil {
					st.Err++
					p.pushErrSample(st, uid)
					log.Printf("[PaidOps] migration v2 timeweb premium user_id=%d: %v", uid, err)
				} else {
					st.OK++
				}
			}
		}
		st.TwIdx++

	default:
		st.Phase = "pro_groups"
	}

	if err := p.migrationV2Save(st); err != nil {
		return st, true, err
	}
	return st, st.Phase != "done", nil
}

func (p *PaidOps) pushErrSample(st *MigrationV2State, uid uint) {
	if uid == 0 {
		return
	}
	if len(st.ErrUserSample) >= 12 {
		return
	}
	st.ErrUserSample = append(st.ErrUserSample, uid)
}

// migrateOnePremiumOrdered: сначала снимаем старые контейнеры, затем БД/секреты, затем поднимаем ee (порты из proxy_nodes сохраняются).
func (p *PaidOps) migrateOnePremiumOrdered(ctx context.Context, u *domain.User, proxy *domain.ProxyNode) error {
	if !isLegacyPremiumProxy(proxy) {
		return fmt.Errorf("not legacy premium")
	}
	if strings.TrimSpace(p.PremiumServerIP) == "" || net.ParseIP(p.PremiumServerIP) == nil {
		return fmt.Errorf("legacy premium: PREMIUM_SERVER_IP required")
	}
	subCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	p.Docker.RemoveUserPremiumEEContainers(subCtx, u.TGID)

	if _, err := p.ProxyUC.EnsurePremiumProxyForUser(u, p.PremiumServerIP, p.Docker); err != nil {
		return fmt.Errorf("EnsurePremiumProxyForUser: %w", err)
	}
	proxy2, err := p.Proxies.GetByOwnerID(u.ID)
	if err != nil || proxy2 == nil {
		return fmt.Errorf("reload proxy: %w", err)
	}
	subCtx2, cancel2 := context.WithTimeout(ctx, 45*time.Second)
	defer cancel2()
	if err := p.Docker.CreateUserPremiumEEContainers(subCtx2, u.TGID, proxy2); err != nil {
		return fmt.Errorf("CreateUserPremiumEEContainers: %w", err)
	}
	p.syncPremiumUserProxies(u, proxy2)
	return nil
}

func (p *PaidOps) migrateOneTimewebPremium(ctx context.Context, u *domain.User, proxy *domain.ProxyNode) error {
	if p.Provisioner == nil {
		return fmt.Errorf("timeweb: provisioner not configured")
	}
	clientIP := strings.TrimSpace(proxy.FloatingIP)
	if clientIP == "" {
		clientIP = strings.TrimSpace(proxy.IP)
	}
	if net.ParseIP(clientIP) == nil {
		return fmt.Errorf("timeweb: no client IP")
	}
	if _, err := p.ProxyUC.EnsurePremiumProxyForUser(u, clientIP, p.Docker); err != nil {
		return fmt.Errorf("EnsurePremiumProxyForUser: %w", err)
	}
	proxy2, err := p.Proxies.GetByOwnerID(u.ID)
	if err != nil || proxy2 == nil {
		return fmt.Errorf("reload proxy: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	if err := p.Provisioner.RestartContainersForUser(runCtx, u, proxy2); err != nil {
		return fmt.Errorf("RestartContainersForUser: %w", err)
	}
	p.syncPremiumUserProxies(u, proxy2)
	return nil
}

// syncPremiumUserProxies пересобирает «Мои прокси» для Premium-пользователя из актуальной записи proxy_nodes.
// Это устраняет рассинхрон после миграций/рестартов, когда контейнеры уже с новыми ee-секретами,
// а в user_proxies остаются старые dd/ee записи.
func (p *PaidOps) syncPremiumUserProxies(u *domain.User, proxy *domain.ProxyNode) {
	if p == nil || p.UserProxies == nil || u == nil || proxy == nil {
		return
	}
	if proxy.Type != domain.ProxyTypePremium {
		return
	}
	_ = p.UserProxies.DeleteByUserIDAndProxyType(u.ID, domain.ProxyTypePremium)

	clientIP := strings.TrimSpace(proxy.FloatingIP)
	if clientIP == "" {
		clientIP = strings.TrimSpace(proxy.IP)
	}
	if net.ParseIP(clientIP) == nil {
		log.Printf("[PaidOps] syncPremiumUserProxies user_id=%d: invalid client ip %q", u.ID, clientIP)
		return
	}

	port1 := proxy.Port
	port2 := 0
	if !isLegacyPremiumProxy(proxy) {
		port1 = domain.PremiumPortEE1
		port2 = domain.PremiumPortEE2
	}
	if port1 > 0 && strings.TrimSpace(proxy.Secret) != "" {
		_ = p.UserProxies.Create(&domain.UserProxy{
			UserID:    u.ID,
			IP:        clientIP,
			Port:      port1,
			Secret:    proxy.Secret,
			ProxyType: domain.ProxyTypePremium,
		})
	}
	if port2 > 0 && strings.TrimSpace(proxy.SecretEE) != "" {
		_ = p.UserProxies.Create(&domain.UserProxy{
			UserID:    u.ID,
			IP:        clientIP,
			Port:      port2,
			Secret:    proxy.SecretEE,
			ProxyType: domain.ProxyTypePremium,
		})
	}
}

// MigrationV2ProgressReportHTML — текст для менеджерского чата (HTML).
func MigrationV2ProgressReportHTML(st *MigrationV2State, contour string) string {
	if st == nil {
		return ""
	}
	leftPro := 0
	if st.Phase == "pro_groups" {
		leftPro = len(st.ProGroupIDs) - st.ProIdx
		if leftPro < 0 {
			leftPro = 0
		}
	}
	leftLeg := 0
	if st.Phase == "legacy_premium" {
		leftLeg = len(st.LegacyUserIDs) - st.LegacyIdx
		if leftLeg < 0 {
			leftLeg = 0
		}
	}
	leftTw := 0
	if st.Phase == "timeweb_premium" {
		leftTw = len(st.TwUserIDs) - st.TwIdx
		if leftTw < 0 {
			leftTw = 0
		}
	}
	phaseRu := map[string]string{
		"pro_groups":        "Pro (Docker)",
		"legacy_premium":    "Premium legacy (Docker)",
		"timeweb_premium":   "Premium TimeWeb",
		"done":              "завершено",
	}[st.Phase]
	if phaseRu == "" {
		phaseRu = st.Phase
	}
	var errSam string
	if len(st.ErrUserSample) > 0 {
		errSam = fmt.Sprintf("\nПримеры user_id с ошибками: <code>%v</code>", st.ErrUserSample)
	}
	return fmt.Sprintf(
		"🔄 <b>Миграция paid dd→ee (v2)</b>\n"+
			"📍 Контур: <b>%s</b>\n"+
			"▶️ Фаза: <b>%s</b>\n"+
			"✅ Успешно шагов: <b>%d</b>\n"+
			"❌ Ошибок: <b>%d</b>\n"+
			"⏳ Осталось: Pro-групп ~<b>%d</b>, legacy Premium ~<b>%d</b>, TimeWeb ~<b>%d</b>\n"+
			"🕐 Старт: <code>%s</code>%s",
		contour, phaseRu, st.OK, st.Err, leftPro, leftLeg, leftTw, st.StartedAt, errSam,
	)
}

// ResetPaidMigrationV2Markers сбрасывает маркер и состояние миграции v2 (повторный полный прогон).
func ResetPaidMigrationV2Markers(settings repository.SettingsRepository) error {
	if settings == nil {
		return fmt.Errorf("settings required")
	}
	if err := settings.Set(SettingPaidMigrationV2Done, ""); err != nil {
		return err
	}
	return settings.Set(SettingPaidMigrationV2State, "")
}
