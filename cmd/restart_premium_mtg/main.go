package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/database"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
)

type restartItem struct {
	User  *domain.User
	Proxy *domain.ProxyNode
}

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Overload(".env.test")

	dryRun := flag.Bool("dry-run", false, "показать план без SSH-перезапуска")
	timeout := flag.Duration("timeout", 90*time.Minute, "общий таймаут")
	flag.Parse()

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Database.Host == "" {
		log.Fatal("DB_HOST required")
	}

	db, err := database.New(&cfg.Database)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	proxyRepo := repository.NewProxyRepository(db.DB)
	userRepo := repository.NewUserRepository(db.DB)
	serverRepo := repository.NewPremiumServerRepository(db.DB)

	proxies, err := proxyRepo.GetActivePremiumProxies()
	if err != nil {
		log.Fatalf("GetActivePremiumProxies: %v", err)
	}

	byServer := map[uint][]restartItem{}
	skipped := 0
	for _, p := range proxies {
		if p == nil || domain.IsLegacyPremiumProxy(p) {
			skipped++
			continue
		}
		if p.PremiumServerID == nil || *p.PremiumServerID == 0 || p.OwnerID == nil || strings.TrimSpace(p.FloatingIP) == "" {
			skipped++
			continue
		}
		if strings.TrimSpace(p.Secret) == "" || strings.TrimSpace(p.SecretEE) == "" {
			skipped++
			continue
		}
		u, errUser := userRepo.GetByID(*p.OwnerID)
		if errUser != nil || u == nil {
			skipped++
			continue
		}
		byServer[*p.PremiumServerID] = append(byServer[*p.PremiumServerID], restartItem{User: u, Proxy: p})
	}

	serverIDs := make([]uint, 0, len(byServer))
	total := 0
	for sid, items := range byServer {
		serverIDs = append(serverIDs, sid)
		total += len(items)
	}
	sort.Slice(serverIDs, func(i, j int) bool { return serverIDs[i] < serverIDs[j] })

	log.Printf("restart_premium_mtg: active_premium=%d, to_restart=%d, servers=%d, skipped=%d, dry_run=%v",
		len(proxies), total, len(serverIDs), skipped, *dryRun)
	if *dryRun {
		for _, sid := range serverIDs {
			log.Printf("  server_id=%d: %d контейнеров", sid, len(byServer[sid]))
		}
		return
	}
	if total == 0 {
		log.Println("restart_premium_mtg: нечего перезапускать")
		return
	}

	if cfg.Timeweb.APIToken == "" {
		log.Fatal("TIMEWEB_API_TOKEN required for SSH password refresh")
	}
	twClient := timeweb.NewClient(cfg.Timeweb.APIToken)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var failures []string
	for _, sid := range serverIDs {
		server, errSrv := serverRepo.GetByID(sid)
		if errSrv != nil || server == nil {
			failures = append(failures, fmt.Sprintf("server_id=%d lookup: %v", sid, errSrv))
			continue
		}
		if err := ensureSSHPassword(ctx, twClient, serverRepo, server); err != nil {
			failures = append(failures, fmt.Sprintf("server_id=%d ensure ssh password: %v", sid, err))
			continue
		}
		client := newSSHClient(server, cfg.Timeweb.SSHUser, cfg.Timeweb.SSHKeyPath, serverRepo)
		if err := prepareServer(ctx, client); err != nil {
			failures = append(failures, fmt.Sprintf("server_id=%d prepare: %v", sid, err))
			continue
		}
		items := byServer[sid]
		log.Printf("server id=%d ip=%s: restart %d premium proxies", server.ID, server.IP, len(items))
		for i, it := range items {
			if err := client.EnsureHostLocalFloatingIP(ctx, it.Proxy.FloatingIP); err != nil {
				failures = append(failures, fmt.Sprintf("server_id=%d proxy_id=%d ensure_fip: %v", sid, it.Proxy.ID, err))
				continue
			}
			err := client.StartPremiumContainers(ctx, it.User.TGID, it.Proxy.FloatingIP, it.Proxy.Secret, it.Proxy.SecretEE)
			if err != nil {
				failures = append(failures, fmt.Sprintf("server_id=%d proxy_id=%d tg_id=%d: %v", sid, it.Proxy.ID, it.User.TGID, err))
				continue
			}
			log.Printf("server id=%d: %d/%d ok proxy_id=%d tg_id=%d", sid, i+1, len(items), it.Proxy.ID, it.User.TGID)
		}
	}

	if len(failures) > 0 {
		for _, f := range failures {
			log.Printf("FAIL: %s", f)
		}
		log.Fatalf("restart_premium_mtg: завершено с ошибками (%d)", len(failures))
	}
	log.Println("restart_premium_mtg: done")
}

func newSSHClient(server *domain.PremiumServer, sshUser, sshKeyPath string, serverRepo repository.PremiumServerRepository) *timeweb.SSHClient {
	keyPath := ""
	if strings.TrimSpace(server.SSHPassword) == "" {
		keyPath = strings.TrimSpace(sshKeyPath)
	}
	client := timeweb.NewSSHClient(server.IP, 22, sshUser, keyPath)
	if strings.TrimSpace(server.SSHPassword) != "" {
		client = client.WithPassword(server.SSHPassword)
	}
	if strings.TrimSpace(server.SSHHostKey) != "" {
		return client.WithKnownHostKey(server.SSHHostKey, nil)
	}
	serverID := server.ID
	serverIP := server.IP
	return client.WithKnownHostKey("", func(hostKey string) {
		if err := serverRepo.UpdateSSHHostKey(serverID, hostKey); err != nil {
			log.Printf("[SSH] failed to save host key for server %d: %v", serverID, err)
			return
		}
		log.Printf("[SSH] host key saved for server %s (id=%d)", serverIP, serverID)
	})
}

func prepareServer(ctx context.Context, client *timeweb.SSHClient) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := client.WaitSSHReady(waitCtx); err != nil {
		return err
	}
	if err := client.EnsureDockerInstalled(waitCtx); err != nil {
		return err
	}
	select {
	case <-waitCtx.Done():
		return waitCtx.Err()
	case <-time.After(3 * time.Second):
	}
	if err := client.EnsurePremiumHostTuning(waitCtx); err != nil {
		return err
	}
	if err := client.PullPremiumMtgImages(waitCtx); err != nil {
		return err
	}
	client.EnsurePremiumFirewallPorts(waitCtx)
	return nil
}

func ensureSSHPassword(
	ctx context.Context,
	twClient *timeweb.Client,
	serverRepo repository.PremiumServerRepository,
	server *domain.PremiumServer,
) error {
	if server == nil {
		return errors.New("server nil")
	}
	if strings.TrimSpace(server.SSHPassword) != "" {
		return nil
	}
	if server.TimewebID <= 0 {
		return fmt.Errorf("server id=%d: ssh_password empty and timeweb_id=0", server.ID)
	}
	srv, err := twClient.GetServer(ctx, server.TimewebID)
	if err != nil {
		return fmt.Errorf("get server root_pass: %w", err)
	}
	if pass := strings.TrimSpace(srv.RootPass); pass != "" {
		server.SSHPassword = pass
		return serverRepo.UpdateSSHPassword(server.ID, pass)
	}
	if err := twClient.PerformServerAction(ctx, server.TimewebID, "reset_password"); err != nil {
		return fmt.Errorf("timeweb reset_password: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait root_pass after reset_password: %w", waitCtx.Err())
		case <-time.After(5 * time.Second):
		}
		srv, err = twClient.GetServer(waitCtx, server.TimewebID)
		if err != nil {
			continue
		}
		if pass := strings.TrimSpace(srv.RootPass); pass != "" {
			server.SSHPassword = pass
			return serverRepo.UpdateSSHPassword(server.ID, pass)
		}
	}
}
