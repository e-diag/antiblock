// Command restart_premium_mtg — пересоздаёт все активные TimeWeb Premium контейнеры mtg (ee1/ee2)
// с текущим флагом simple-run --proxy из конфига или флага -mtg-proxy.
//
// Запуск (из корня репозитория, с .env и доступом к БД):
//
//	go run ./cmd/restart_premium_mtg -dry-run
//	go run ./cmd/restart_premium_mtg -mtg-proxy=socks5://127.0.0.1:1080
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/database"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Overload(".env.test")

	dryRun := flag.Bool("dry-run", false, "только список прокси по серверам, без SSH")
	mtgProxy := flag.String("mtg-proxy", "", "переопределить upstream для --proxy=… (иначе premium_mtg_upstream_proxy / TIMEWEB_PREMIUM_MTG_PROXY)")
	allowEmptyProxy := flag.Bool("allow-empty-proxy", false, "перезапуск без флага --proxy (как при пустом premium_mtg_upstream_proxy)")
	timeout := flag.Duration("timeout", 90*time.Minute, "общий таймаут контекста")
	flag.Parse()

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Database.Host == "" {
		log.Fatalf("DB_HOST required")
	}
	if cfg.Timeweb.APIToken == "" {
		log.Fatal("TIMEWEB_API_TOKEN обязателен (ensureSSHRootPassword через API)")
	}

	db, err := database.New(&cfg.Database)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	proxyURL := strings.TrimSpace(cfg.Timeweb.PremiumMtgUpstreamProxy)
	if strings.TrimSpace(*mtgProxy) != "" {
		proxyURL = strings.TrimSpace(*mtgProxy)
	}
	if proxyURL == "" && !*allowEmptyProxy {
		log.Fatal("укажите upstream: premium_mtg_upstream_proxy, TIMEWEB_PREMIUM_MTG_PROXY или -mtg-proxy; либо явно -allow-empty-proxy")
	}
	if proxyURL != "" {
		if err := timeweb.ValidateMtgUpstreamProxyURL(proxyURL); err != nil {
			log.Fatalf("mtg-proxy: %v", err)
		}
	}

	proxyRepo := repository.NewProxyRepository(db.DB)
	userRepo := repository.NewUserRepository(db.DB)
	serverRepo := repository.NewPremiumServerRepository(db.DB)
	vpsReqRepo := repository.NewVPSProvisionRequestRepository(db.DB)

	proxies, err := proxyRepo.GetActivePremiumProxies()
	if err != nil {
		log.Fatalf("GetActivePremiumProxies: %v", err)
	}

	byServer := make(map[uint][]usecase.PremiumMtgRestartItem)
	var skip []string

	for _, px := range proxies {
		if px == nil {
			continue
		}
		if domain.IsLegacyPremiumProxy(px) {
			skip = append(skip, fmt.Sprintf("proxy_id=%d legacy Pro Docker", px.ID))
			continue
		}
		if err := usecase.ValidatePremiumMtgRestartProxy(px); err != nil {
			skip = append(skip, fmt.Sprintf("proxy_id=%d: %v", px.ID, err))
			continue
		}
		if px.OwnerID == nil {
			skip = append(skip, fmt.Sprintf("proxy_id=%d: нет owner_id", px.ID))
			continue
		}
		u, errU := userRepo.GetByID(*px.OwnerID)
		if errU != nil || u == nil {
			skip = append(skip, fmt.Sprintf("proxy_id=%d: user id=%d не найден (%v)", px.ID, *px.OwnerID, errU))
			continue
		}
		sid := *px.PremiumServerID
		byServer[sid] = append(byServer[sid], usecase.PremiumMtgRestartItem{User: u, Proxy: px})
	}

	serverIDs := make([]uint, 0, len(byServer))
	for sid := range byServer {
		serverIDs = append(serverIDs, sid)
	}
	sort.Slice(serverIDs, func(i, j int) bool { return serverIDs[i] < serverIDs[j] })

	totalRestart := 0
	for _, sid := range serverIDs {
		totalRestart += len(byServer[sid])
	}

	log.Printf("restart_premium_mtg: активных premium в БД=%d, к перезапуску=%d на %d серверах, пропусков=%d, dry_run=%v, mtg_proxy=%q",
		len(proxies), totalRestart, len(byServer), len(skip), *dryRun, proxyURL)
	for _, s := range skip {
		log.Printf("  skip: %s", s)
	}

	if *dryRun {
		for _, sid := range serverIDs {
			items := byServer[sid]
			log.Printf("  server_id=%d: %d прокси", sid, len(items))
			for _, it := range items {
				log.Printf("    proxy_id=%d tg_id=%d fip=%s", it.Proxy.ID, it.User.TGID, it.Proxy.FloatingIP)
			}
		}
		os.Exit(0)
	}

	if totalRestart == 0 {
		log.Println("нечего перезапускать")
		os.Exit(0)
	}

	twClient := timeweb.NewClient(cfg.Timeweb.APIToken)
	prov := usecase.NewPremiumProvisioner(
		twClient,
		serverRepo,
		vpsReqRepo,
		cfg.Timeweb.SSHUser,
		cfg.Timeweb.SSHKeyPath,
		cfg.Timeweb.SSHKeyID,
		cfg.Timeweb.AvailabilityZone,
		time.Duration(cfg.Timeweb.PremiumSSHMinIntervalSeconds)*time.Second,
		proxyURL,
	)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	for _, sid := range serverIDs {
		srv, errS := serverRepo.GetByID(sid)
		if errS != nil || srv == nil {
			log.Fatalf("premium_server id=%d: %v", sid, errS)
		}
		items := byServer[sid]
		log.Printf("restart_premium_mtg: сервер id=%d ip=%s — %d прокси", srv.ID, srv.IP, len(items))
		if err := prov.RestartPremiumMtgBatchOnServer(ctx, srv, items); err != nil {
			log.Fatalf("server id=%d ip=%s: %v", sid, srv.IP, err)
		}
	}
	log.Println("restart_premium_mtg: готово")
}
