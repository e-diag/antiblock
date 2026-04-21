package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/joho/godotenv"
	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/database"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
	"gorm.io/gorm"
)

type legacyRow struct {
	ProxyID      uint
	OwnerID      uint
	IP           string
	Port         int
	Secret       string
	SecretEE     string
	Status       string
	TGID         int64
	IsPremium    bool
	PremiumUntil *time.Time
}

type rebuildPlan struct {
	ProGroups              []*domain.ProGroup
	ProSubscribersByGroup  map[uint][]*domain.ProSubscription
	LegacyToRecreate       []legacyTarget
	LegacyToDeactivate     []legacyTarget
	OrphanContainersToDrop []string
}

type legacyTarget struct {
	TGID      int64
	UserID    uint
	Proxy     *domain.ProxyNode
	RunSecret string
}

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Overload(".env.test")

	apply := flag.Bool("apply", false, "выполнить изменения (без флага — только dry-run)")
	flag.Parse()

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Database.Host == "" {
		log.Fatal("DB_HOST required")
	}
	if cfg.ProDocker.Host == "" || cfg.ProDocker.CertPath == "" {
		log.Fatal("pro_docker.host/cert_path required")
	}

	db, err := database.New(&cfg.Database)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	port := cfg.ProDocker.Port
	if port <= 0 {
		port = 2376
	}
	dockerMgr, err := docker.NewManagerTLS(cfg.ProDocker.Host, port, cfg.ProDocker.CertPath)
	if err != nil {
		log.Fatalf("docker TLS: %v", err)
	}

	proGroupRepo := repository.NewProGroupRepository(db.DB)
	proSubRepo := repository.NewProSubscriptionRepository(db.DB)
	proxyRepo := repository.NewProxyRepository(db.DB)
	userProxyRepo := repository.NewUserProxyRepository(db.DB)

	plan, err := buildPlan(db.DB, proGroupRepo, proSubRepo, proxyRepo, dockerMgr)
	if err != nil {
		log.Fatalf("build plan: %v", err)
	}

	log.Printf("rebuild_pro_legacy plan: pro_groups=%d legacy_recreate=%d legacy_deactivate=%d orphan_containers=%d",
		len(plan.ProGroups), len(plan.LegacyToRecreate), len(plan.LegacyToDeactivate), len(plan.OrphanContainersToDrop))
	if !*apply {
		if len(plan.OrphanContainersToDrop) > 0 {
			log.Printf("dry-run orphan containers to remove (%d): %s", len(plan.OrphanContainersToDrop), strings.Join(plan.OrphanContainersToDrop, ", "))
		}
		log.Println("dry-run mode (use -apply to execute)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// 1) Сносим контейнеры, которых не должно быть.
	var failures []string
	for _, name := range plan.OrphanContainersToDrop {
		if err := dockerMgr.RemoveUserContainer(ctx, name); err != nil {
			failures = append(failures, fmt.Sprintf("remove orphan %s: %v", name, err))
		}
	}

	// 2) Деактивируем legacy, которые уже не должны существовать.
	now := time.Now().UTC()
	for _, tgt := range plan.LegacyToDeactivate {
		tgID, userID, p := tgt.TGID, tgt.UserID, tgt.Proxy
		dockerMgr.RemoveUserPremiumEEContainers(ctx, tgID)
		p.Status = domain.ProxyStatusInactive
		p.UnreachableSince = nil
		p.LastRTTMs = nil
		p.LastCheck = &now
		if err := proxyRepo.Update(p); err != nil {
			failures = append(failures, fmt.Sprintf("deactivate legacy proxy_id=%d tg_id=%d: %v", p.ID, tgID, err))
			continue
		}
		_ = userProxyRepo.DeleteByIPPortSecret(p.IP, p.Port, p.Secret)
		if strings.TrimSpace(p.SecretEE) != "" {
			_ = userProxyRepo.DeleteByIPPortSecret(p.IP, p.Port, p.SecretEE)
			_ = userProxyRepo.DeleteByIPPortSecret(p.IP, p.Port+10000, p.SecretEE)
		}
		if userID > 0 {
			_ = userProxyRepo.DeleteByUserIDAndProxyType(userID, domain.ProxyTypePremium)
		}
	}

	// 3) Ротируем ключи всех активных Pro-групп, пересоздаём контейнеры и чистим user_proxies.
	for _, g := range plan.ProGroups {
		genCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		newEE1, err1 := dockerMgr.GenerateEESecretViaDocker(genCtx)
		newEE2, err2 := dockerMgr.GenerateEESecretViaDocker(genCtx)
		cancel()
		if err1 != nil || err2 != nil {
			failures = append(failures, fmt.Sprintf("pro group %d: generate ee secrets: %v; %v", g.ID, err1, err2))
			continue
		}

		runGroup := *g
		runGroup.SecretDD = newEE1
		runGroup.SecretEE = newEE2
		if err := dockerMgr.CreateProGroupEEContainers(&runGroup); err != nil {
			failures = append(failures, fmt.Sprintf("pro group %d (%s/%s): %v", g.ID, g.ContainerDD, g.ContainerEE, err))
			continue
		}

		g.SecretDD = newEE1
		g.SecretEE = newEE2
		if err := proGroupRepo.Update(g); err != nil {
			failures = append(failures, fmt.Sprintf("pro group %d update secrets: %v", g.ID, err))
			continue
		}

		for _, sub := range plan.ProSubscribersByGroup[g.ID] {
			if sub == nil {
				continue
			}
			_ = userProxyRepo.DeleteByUserIDAndProxyType(sub.UserID, domain.ProxyTypePro)
			_ = userProxyRepo.Create(&domain.UserProxy{
				UserID:    sub.UserID,
				IP:        g.ServerIP,
				Port:      g.PortDD,
				Secret:    g.SecretDD,
				ProxyType: domain.ProxyTypePro,
			})
			_ = userProxyRepo.Create(&domain.UserProxy{
				UserID:    sub.UserID,
				IP:        g.ServerIP,
				Port:      g.PortEE,
				Secret:    g.SecretEE,
				ProxyType: domain.ProxyTypePro,
			})
		}
	}

	// 4) Ротируем ключи legacy Premium, пересоздаём контейнеры и оставляем в БД только новые ключи.
	for _, tgt := range plan.LegacyToRecreate {
		tgID, userID, p := tgt.TGID, tgt.UserID, tgt.Proxy
		genCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		newEE, genErr := dockerMgr.GenerateEESecretViaDocker(genCtx)
		cancel()
		if genErr != nil {
			failures = append(failures, fmt.Sprintf("legacy tg_id=%d proxy_id=%d generate ee: %v", tgID, p.ID, genErr))
			continue
		}

		runProxy := *p
		runProxy.Secret = newEE
		if err := dockerMgr.CreateUserPremiumEEContainers(ctx, tgID, &runProxy); err != nil {
			failures = append(failures, fmt.Sprintf("legacy tg_id=%d proxy_id=%d: %v", tgID, p.ID, err))
			continue
		}
		p.Status = domain.ProxyStatusActive
		p.UnreachableSince = nil
		p.LastCheck = &now
		// Для legacy оставляем только свежий ee-ключ в обеих колонках.
		p.Secret = newEE
		p.SecretEE = newEE
		name := fmt.Sprintf(docker.UserContainerNameEE1, tgID)
		p.ContainerName = name
		if err := proxyRepo.Update(p); err != nil {
			failures = append(failures, fmt.Sprintf("update legacy proxy_id=%d tg_id=%d: %v", p.ID, tgID, err))
		}
		if userID > 0 {
			_ = userProxyRepo.DeleteByUserIDAndProxyType(userID, domain.ProxyTypePremium)
			_ = userProxyRepo.Create(&domain.UserProxy{
				UserID:    userID,
				IP:        p.IP,
				Port:      p.Port,
				Secret:    newEE,
				ProxyType: domain.ProxyTypePremium,
			})
		}
	}

	if len(failures) > 0 {
		for _, f := range failures {
			log.Printf("[rebuild] FAIL: %s", f)
		}
		log.Fatalf("rebuild_pro_legacy finished with %d failures", len(failures))
	}

	log.Printf("rebuild_pro_legacy done: pro_groups=%d legacy_recreated=%d legacy_deactivated=%d orphan_removed=%d",
		len(plan.ProGroups), len(plan.LegacyToRecreate), len(plan.LegacyToDeactivate), len(plan.OrphanContainersToDrop))
}

func buildPlan(
	gdb *gorm.DB,
	proGroupRepo repository.ProGroupRepository,
	proSubRepo repository.ProSubscriptionRepository,
	proxyRepo repository.ProxyRepository,
	dockerMgr *docker.Manager,
) (*rebuildPlan, error) {
	groups, err := proGroupRepo.GetActiveGroups()
	if err != nil {
		return nil, fmt.Errorf("GetActiveGroups: %w", err)
	}

	desiredProNames := make(map[string]struct{}, len(groups)*2)
	proSubscribersByGroup := make(map[uint][]*domain.ProSubscription, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		subs, err := proSubRepo.GetActiveByGroupID(g.ID)
		if err == nil && len(subs) > 0 {
			proSubscribersByGroup[g.ID] = subs
		}
		if strings.TrimSpace(g.ContainerDD) != "" {
			desiredProNames[g.ContainerDD] = struct{}{}
		}
		if strings.TrimSpace(g.ContainerEE) != "" {
			desiredProNames[g.ContainerEE] = struct{}{}
		}
	}

	var rows []legacyRow
	if err := gdb.Raw(`
		SELECT
			p.id                    AS proxy_id,
			p.owner_id              AS owner_id,
			p.ip                    AS ip,
			p.port                  AS port,
			p.secret                AS secret,
			p.secret_ee             AS secret_ee,
			p.status                AS status,
			u.tg_id                 AS tg_id,
			u.is_premium            AS is_premium,
			u.premium_until         AS premium_until
		FROM proxy_nodes p
		JOIN users u ON u.id = p.owner_id
		WHERE p.type = 'premium'
		  AND (COALESCE(NULLIF(TRIM(p.timeweb_floating_ip_id), ''), '0') = '0')
	`).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("scan legacy premium rows: %w", err)
	}

	var legacyToRecreate []legacyTarget
	var legacyToDeactivate []legacyTarget
	desiredLegacyNames := make(map[string]struct{})
	now := time.Now().UTC()
	seenTG := make(map[int64]uint)
	for _, r := range rows {
		p, err := proxyRepo.GetByID(r.ProxyID)
		if err != nil || p == nil {
			continue
		}

		shouldExist := r.IsPremium && r.PremiumUntil != nil && r.PremiumUntil.After(now)
		// Для nineseconds/mtg принимаем только ee-секрет.
		// Приоритет: secret_ee (исторический второй слот), fallback: secret если он уже ee.
		runSecret := ""
		if s2 := strings.TrimSpace(p.SecretEE); strings.HasPrefix(strings.ToLower(s2), "ee") {
			runSecret = s2
		} else if s1 := strings.TrimSpace(p.Secret); strings.HasPrefix(strings.ToLower(s1), "ee") {
			runSecret = s1
		}
		if shouldExist {
			if runSecret == "" {
				legacyToDeactivate = append(legacyToDeactivate, legacyTarget{TGID: r.TGID, UserID: r.OwnerID, Proxy: p})
				continue
			}
			if oldID, exists := seenTG[r.TGID]; exists {
				legacyToDeactivate = append(legacyToDeactivate, legacyTarget{TGID: r.TGID, UserID: r.OwnerID, Proxy: p})
				log.Printf("[rebuild] duplicate legacy premium rows for tg_id=%d (keep proxy_id=%d, deactivate proxy_id=%d)", r.TGID, oldID, p.ID)
				continue
			}
			seenTG[r.TGID] = p.ID
			legacyToRecreate = append(legacyToRecreate, legacyTarget{TGID: r.TGID, UserID: r.OwnerID, Proxy: p, RunSecret: runSecret})
			desiredLegacyNames[fmt.Sprintf(docker.UserContainerNameEE1, r.TGID)] = struct{}{}
			continue
		}
		legacyToDeactivate = append(legacyToDeactivate, legacyTarget{TGID: r.TGID, UserID: r.OwnerID, Proxy: p})
	}

	listCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := dockerMgr.GetClient().ContainerList(listCtx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("ContainerList: %w", err)
	}
	var orphans []string
	for _, c := range list {
		for _, raw := range c.Names {
			name := strings.TrimPrefix(raw, "/")
			if strings.HasPrefix(name, "mtg-pro-") {
				if _, ok := desiredProNames[name]; !ok {
					orphans = append(orphans, name)
				}
			}
			if strings.HasPrefix(name, "mtg-user-") {
				if _, ok := desiredLegacyNames[name]; !ok {
					orphans = append(orphans, name)
				}
			}
		}
	}

	sort.Slice(legacyToRecreate, func(i, j int) bool { return legacyToRecreate[i].TGID < legacyToRecreate[j].TGID })
	sort.Slice(legacyToDeactivate, func(i, j int) bool { return legacyToDeactivate[i].TGID < legacyToDeactivate[j].TGID })

	return &rebuildPlan{
		ProGroups:              groups,
		ProSubscribersByGroup:  proSubscribersByGroup,
		LegacyToRecreate:       legacyToRecreate,
		LegacyToDeactivate:     legacyToDeactivate,
		OrphanContainersToDrop: dedupeStrings(orphans),
	}, nil
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
