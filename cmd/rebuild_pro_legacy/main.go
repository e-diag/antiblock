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
	LegacyToRecreate       []legacyTarget
	LegacyToDeactivate     []legacyTarget
	OrphanContainersToDrop []string
}

type legacyTarget struct {
	TGID      int64
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
	proxyRepo := repository.NewProxyRepository(db.DB)
	userProxyRepo := repository.NewUserProxyRepository(db.DB)

	plan, err := buildPlan(db.DB, proGroupRepo, proxyRepo, dockerMgr)
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
		tgID, p := tgt.TGID, tgt.Proxy
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
	}

	// 3) Пересоздаём все активные Pro-группы.
	for _, g := range plan.ProGroups {
		if err := dockerMgr.CreateProGroupEEContainers(g); err != nil {
			failures = append(failures, fmt.Sprintf("pro group %d (%s/%s): %v", g.ID, g.ContainerDD, g.ContainerEE, err))
		}
	}

	// 4) Пересоздаём legacy Premium по активным пользователям.
	for _, tgt := range plan.LegacyToRecreate {
		tgID, p := tgt.TGID, tgt.Proxy
		runProxy := *p
		runProxy.Secret = tgt.RunSecret
		if err := dockerMgr.CreateUserPremiumEEContainers(ctx, tgID, &runProxy); err != nil {
			failures = append(failures, fmt.Sprintf("legacy tg_id=%d proxy_id=%d: %v", tgID, p.ID, err))
			continue
		}
		p.Status = domain.ProxyStatusActive
		p.UnreachableSince = nil
		p.LastCheck = &now
		name := fmt.Sprintf(docker.UserContainerNameEE1, tgID)
		p.ContainerName = name
		if err := proxyRepo.Update(p); err != nil {
			failures = append(failures, fmt.Sprintf("update legacy proxy_id=%d tg_id=%d: %v", p.ID, tgID, err))
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
	proxyRepo repository.ProxyRepository,
	dockerMgr *docker.Manager,
) (*rebuildPlan, error) {
	groups, err := proGroupRepo.GetActiveGroups()
	if err != nil {
		return nil, fmt.Errorf("GetActiveGroups: %w", err)
	}

	desiredProNames := make(map[string]struct{}, len(groups)*2)
	for _, g := range groups {
		if g == nil {
			continue
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
		runSecret := strings.TrimSpace(p.Secret)
		if runSecret == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.SecretEE)), "ee") {
			runSecret = strings.TrimSpace(p.SecretEE)
		}
		if shouldExist {
			if runSecret == "" {
				legacyToDeactivate = append(legacyToDeactivate, legacyTarget{TGID: r.TGID, Proxy: p})
				continue
			}
			if oldID, exists := seenTG[r.TGID]; exists {
				legacyToDeactivate = append(legacyToDeactivate, legacyTarget{TGID: r.TGID, Proxy: p})
				log.Printf("[rebuild] duplicate legacy premium rows for tg_id=%d (keep proxy_id=%d, deactivate proxy_id=%d)", r.TGID, oldID, p.ID)
				continue
			}
			seenTG[r.TGID] = p.ID
			legacyToRecreate = append(legacyToRecreate, legacyTarget{TGID: r.TGID, Proxy: p, RunSecret: runSecret})
			desiredLegacyNames[fmt.Sprintf(docker.UserContainerNameEE1, r.TGID)] = struct{}{}
			continue
		}
		legacyToDeactivate = append(legacyToDeactivate, legacyTarget{TGID: r.TGID, Proxy: p})
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
