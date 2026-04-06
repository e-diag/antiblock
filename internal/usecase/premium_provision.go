package usecase

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
)

var (
	ErrNoActivePremiumServer = errors.New("no active premium server available")
	// ErrFloatingIPDailyLimit — либо все активные серверы в пуле исчерпали локальный лимит 10 FIP/сутки (UTC),
	// либо TimeWeb API вернул суточный лимит создания FIP.
	ErrFloatingIPDailyLimit = timeweb.ErrFloatingIPDailyLimit
	// ErrFloatingIPNoBalanceForMonth — у TimeWeb недостаточно средств для создания floating IP.
	ErrFloatingIPNoBalanceForMonth = timeweb.ErrFloatingIPNoBalanceForMonth
)

const maxFIPPerServerPerDay = 10

func effectiveFIPToday(s *domain.PremiumServer) int {
	if s == nil {
		return 0
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if s.FIPCountDate == nil || s.FIPCountDate.Before(today) {
		return 0
	}
	return s.FIPCountToday
}

// PremiumProvisioner управляет Premium provisioning через TimeWeb (floating IP + docker через SSH).
type PremiumProvisioner struct {
	twClient         *timeweb.Client
	serverRepo       repository.PremiumServerRepository
	provisionReqRepo repository.VPSProvisionRequestRepository

	sshUser    string
	sshKeyPath string
	sshKeyID   int
	zone       string
}

func NewPremiumProvisioner(
	twClient *timeweb.Client,
	serverRepo repository.PremiumServerRepository,
	provisionReqRepo repository.VPSProvisionRequestRepository,
	sshUser, sshKeyPath string,
	sshKeyID int,
	zone string,
) *PremiumProvisioner {
	return &PremiumProvisioner{
		twClient:         twClient,
		serverRepo:       serverRepo,
		provisionReqRepo: provisionReqRepo,
		sshUser:          sshUser,
		sshKeyPath:       sshKeyPath,
		sshKeyID:         sshKeyID,
		zone:             zone,
	}
}

// IsConfigured true, если задан TimeWeb API client с токеном.
func (p *PremiumProvisioner) IsConfigured() bool {
	return p != nil && p.twClient != nil && p.twClient.IsConfigured()
}

// newSSHClient создаёт SSH клиент с верификацией host key.
// Для нового сервера host key будет сохранён при первом успешном подключении.
// При непустом пароле файл SSH-ключа не используется (не требуется mount premium-keys в Docker).
func (p *PremiumProvisioner) newSSHClient(server *domain.PremiumServer) *timeweb.SSHClient {
	keyPath := p.sshKeyPath
	if strings.TrimSpace(server.SSHPassword) != "" {
		keyPath = ""
	}
	client := timeweb.NewSSHClient(server.IP, 22, p.sshUser, keyPath)
	if strings.TrimSpace(server.SSHPassword) != "" {
		client = client.WithPassword(server.SSHPassword)
	}
	if strings.TrimSpace(server.SSHHostKey) != "" {
		return client.WithKnownHostKey(server.SSHHostKey, nil)
	}
	serverID := server.ID
	serverIP := server.IP
	return client.WithKnownHostKey("", func(hostKey string) {
		if err := p.serverRepo.UpdateSSHHostKey(serverID, hostKey); err != nil {
			log.Printf("[SSH] failed to save host key for server %d: %v", serverID, err)
		} else {
			log.Printf("[SSH] host key saved for server %s (id=%d)", serverIP, serverID)
		}
	})
}

// ensureSSHRootPassword подгружает root_pass из Timeweb GET /servers/{id}, при пустом — reset_password и опрос API.
// Обновляет server.SSHPassword в памяти и колонку ssh_password в БД.
func (p *PremiumProvisioner) ensureSSHRootPassword(ctx context.Context, server *domain.PremiumServer) error {
	if server == nil {
		return errors.New("ensureSSHRootPassword: server is nil")
	}
	if strings.TrimSpace(server.SSHPassword) != "" {
		return nil
	}
	if server.TimewebID <= 0 {
		return fmt.Errorf("premium server id=%d: пустой ssh_password и timeweb_id=0 — задайте пароль в БД или привяжите VPS к Timeweb", server.ID)
	}
	if p.twClient == nil || !p.twClient.IsConfigured() {
		return fmt.Errorf("premium server id=%d: пустой ssh_password, Timeweb API не настроен", server.ID)
	}

	savePass := func(pass string) error {
		pass = strings.TrimSpace(pass)
		if pass == "" {
			return nil
		}
		server.SSHPassword = pass
		if err := p.serverRepo.UpdateSSHPassword(server.ID, pass); err != nil {
			return fmt.Errorf("save ssh_password: %w", err)
		}
		log.Printf("[Premium] server id=%d timeweb_id=%d: root password получен из API и сохранён в БД", server.ID, server.TimewebID)
		return nil
	}

	srv, err := p.twClient.GetServer(ctx, server.TimewebID)
	if err != nil {
		return fmt.Errorf("get server (root_pass): %w", err)
	}
	if err := savePass(srv.RootPass); err != nil {
		return err
	}
	if strings.TrimSpace(server.SSHPassword) != "" {
		return nil
	}

	log.Printf("[Premium] server id=%d timeweb_id=%d: root_pass пуст в GET — вызываем reset_password", server.ID, server.TimewebID)
	if err := p.twClient.PerformServerAction(ctx, server.TimewebID, "reset_password"); err != nil {
		return fmt.Errorf("timeweb reset_password: %w", err)
	}

	pollCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-pollCtx.Done():
				return fmt.Errorf("таймаут ожидания root_pass после reset_password (timeweb_id=%d): %w", server.TimewebID, pollCtx.Err())
			case <-time.After(5 * time.Second):
			}
		}
		srv, err := p.twClient.GetServer(pollCtx, server.TimewebID)
		if err != nil {
			if errors.Is(err, timeweb.ErrServerNotFound) {
				return err
			}
			continue
		}
		if err := savePass(srv.RootPass); err != nil {
			return err
		}
		if strings.TrimSpace(server.SSHPassword) != "" {
			return nil
		}
	}
}

func (p *PremiumProvisioner) logPremiumPool(ctxTag string) {
	servers, err := p.serverRepo.GetAllActive()
	if err != nil {
		log.Printf("[Premium] pool (%s): GetAllActive err=%v", ctxTag, err)
		return
	}
	for _, s := range servers {
		if s == nil {
			continue
		}
		fip := effectiveFIPToday(s)
		log.Printf("[Premium] pool: server_id=%d ip=%s fip=%d/%d active=%v (%s)",
			s.ID, s.IP, fip, maxFIPPerServerPerDay, s.IsActive, ctxTag)
	}
}

// getAvailableServer возвращает первый активный сервер с fip_count за сегодня (UTC) < maxFIPPerServerPerDay.
// Нет активных — ErrNoActivePremiumServer; все исчерпали лимит — ErrFloatingIPDailyLimit.
func (p *PremiumProvisioner) getAvailableServer() (*domain.PremiumServer, error) {
	servers, err := p.serverRepo.GetAllActive()
	if err != nil {
		return nil, fmt.Errorf("GetAllActive: %w", err)
	}
	if len(servers) == 0 {
		all, errAll := p.serverRepo.GetAll()
		if errAll != nil {
			log.Printf("[Premium] getAvailableServer: pool empty (0 active), GetAll diagnostic err=%v", errAll)
		} else {
			log.Printf("[Premium] getAvailableServer: pool empty — активных серверов 0, всего строк premium_servers: %d", len(all))
			for _, s := range all {
				if s == nil {
					continue
				}
				log.Printf("[Premium]   └ id=%d ip=%s timeweb_id=%d is_active=%v", s.ID, s.IP, s.TimewebID, s.IsActive)
			}
			if len(all) == 0 {
				log.Printf("[Premium]   └ добавьте VPS: команда бота /premium_pool_add или мастер «Создать Premium VPS»")
			} else {
				log.Printf("[Premium]   └ если сервер есть, но is_active=false — включите в БД или добавьте новый через /premium_pool_add")
			}
		}
		return nil, ErrNoActivePremiumServer
	}
	for _, s := range servers {
		if s == nil {
			continue
		}
		fip := effectiveFIPToday(s)
		log.Printf("[Premium] getAvailableServer: server_id=%d ip=%s fip_today=%d/%d",
			s.ID, s.IP, fip, maxFIPPerServerPerDay)
		if fip < maxFIPPerServerPerDay {
			return s, nil
		}
	}
	log.Printf("[Premium] getAvailableServer: all %d active servers exhausted FIP limit today (UTC)", len(servers))
	return nil, ErrFloatingIPDailyLimit
}

// sshPreparePremiumHost — SSH готов, Docker установлен, порты 443/8443 в ufw (если включён).
func (p *PremiumProvisioner) sshPreparePremiumHost(ctx context.Context, sshClient *timeweb.SSHClient, logTag string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := sshClient.WaitSSHReady(waitCtx); err != nil {
		return fmt.Errorf("wait ssh (%s): %w", logTag, err)
	}
	if err := sshClient.EnsureDockerInstalled(waitCtx); err != nil {
		return fmt.Errorf("ensure docker (%s): %w", logTag, err)
	}
	if err := sshClient.EnsurePremiumHostTuning(waitCtx); err != nil {
		return fmt.Errorf("ensure premium host tuning (%s): %w", logTag, err)
	}
	if err := sshClient.PullPremiumMtgImages(waitCtx); err != nil {
		return fmt.Errorf("pull premium mtg images (%s): %w", logTag, err)
	}
	sshClient.EnsurePremiumFirewallPorts(ctx)
	return nil
}

// sshStartPremiumContainers назначает FIP на NIC (ip + netplan), затем docker bind на этот адрес.
// secretEE1/secretEE2 — оба ee (nineseconds); порты 8443 и 443.
func (p *PremiumProvisioner) sshStartPremiumContainers(ctx context.Context, sshClient *timeweb.SSHClient, tgID int64, bindIP, secretEE1, secretEE2 string) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := sshClient.EnsureHostLocalFloatingIP(ctx, bindIP); err != nil {
			lastErr = fmt.Errorf("floating ip on host: %w", err)
		} else if err := sshClient.StartPremiumContainers(ctx, tgID, bindIP, secretEE1, secretEE2); err != nil {
			lastErr = err
		} else {
			return nil
		}

		if !isTransientSSHErr(lastErr) || attempt == maxAttempts {
			return lastErr
		}
		log.Printf("[Premium] sshStartPremiumContainers tg_id=%d bind_ip=%s: transient SSH error (attempt %d/%d): %v",
			tgID, bindIP, attempt, maxAttempts, lastErr)
		select {
		case <-ctx.Done():
			return fmt.Errorf("sshStartPremiumContainers canceled: %w", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	return lastErr
}

func isTransientSSHErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "ssh dial") ||
		strings.Contains(s, "handshake failed") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "i/o timeout")
}

// ProvisionForUser создаёт floating IP и запускает два ee-контейнера для нового Premium-юзера.
// secretEE1/secretEE2: если непустые — из Pro Docker; иначе генерируются на VPS.
// В случае ErrFloatingIPDailyLimit возвращает (placeholderProxy, ErrFloatingIPDailyLimit).
func (p *PremiumProvisioner) ProvisionForUser(ctx context.Context, user *domain.User, secretEE1, secretEE2 string) (*domain.ProxyNode, error) {
	if user == nil {
		return nil, errors.New("user is nil")
	}
	tgID := user.TGID
	log.Printf("[Premium] ProvisionForUser: start tg_id=%d user_id=%d zone=%s", tgID, user.ID, p.zone)
	p.logPremiumPool("ProvisionForUser")

	server, err := p.getAvailableServer()
	if err != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: no available server: %v", tgID, err)
		return nil, err
	}
	log.Printf("[Premium] ProvisionForUser tg_id=%d: selected server_id=%d ip=%s timeweb_id=%d", tgID, server.ID, server.IP, server.TimewebID)

	if err := p.ensureSSHRootPassword(ctx, server); err != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: ensureSSHRootPassword FAILED: %v", tgID, err)
		return nil, fmt.Errorf("ensure ssh root password: %w", err)
	}

	ownerID := user.ID
	sshClient := p.newSSHClient(server)

	if err := p.sshPreparePremiumHost(ctx, sshClient, "ProvisionForUser"); err != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: sshPreparePremiumHost FAILED: %v", tgID, err)
		return nil, err
	}

	secretEE1 = strings.TrimSpace(secretEE1)
	secretEE2 = strings.TrimSpace(secretEE2)
	if secretEE1 == "" {
		genEE, genErr := sshClient.GenerateEESecret(ctx)
		if genErr != nil {
			return nil, fmt.Errorf("generate ee secret 1 on VPS: %w", genErr)
		}
		secretEE1 = genEE
	}
	if secretEE2 == "" {
		genEE, genErr := sshClient.GenerateEESecret(ctx)
		if genErr != nil {
			return nil, fmt.Errorf("generate ee secret 2 on VPS: %w", genErr)
		}
		secretEE2 = genEE
	}
	log.Printf("[Premium] ProvisionForUser tg_id=%d: ee1 prefix=%.8s… ee2 prefix=%.8s…", tgID, secretEE1, secretEE2)

	// IP для клиента — только персональный floating IP; основной IP VPS в прокси не подставляем.
	placeholder := &domain.ProxyNode{
		IP:                  "",
		Port:                domain.PremiumPortEE1,
		Secret:              secretEE1,
		SecretEE:            secretEE2,
		Type:                domain.ProxyTypePremium,
		Status:              domain.ProxyStatusInactive,
		Load:                0,
		OwnerID:             &ownerID,
		PremiumServerID:     &server.ID,
		FloatingIP:          "",
		TimewebFloatingIPID: "",
	}

	log.Printf("[Premium] ProvisionForUser tg_id=%d: step=CreateFloatingIP zone=%s", tgID, p.zone)
	floatingIP, err := p.twClient.CreateFloatingIP(ctx, p.zone)
	if err != nil {
		if errors.Is(err, ErrFloatingIPDailyLimit) {
			log.Printf("[Premium] ProvisionForUser tg_id=%d: daily floating IP limit — returning placeholder", tgID)
			return placeholder, ErrFloatingIPDailyLimit
		}
		log.Printf("[Premium] ProvisionForUser tg_id=%d: FAILED at CreateFloatingIP: %v", tgID, err)
		return nil, err
	}
	log.Printf("[Premium] ProvisionForUser tg_id=%d: floating IP created id=%s ip=%s", tgID, floatingIP.ID, floatingIP.IP)

	log.Printf("[Premium] ProvisionForUser tg_id=%d: step=BindFloatingIP fip_id=%s → server timeweb_id=%d", tgID, floatingIP.ID, server.TimewebID)
	if err := p.twClient.BindFloatingIP(ctx, floatingIP.ID, server.TimewebID); err != nil {
		_ = p.twClient.DeleteFloatingIP(ctx, floatingIP.ID)
		log.Printf("[Premium] ProvisionForUser tg_id=%d: FAILED at BindFloatingIP: %v", tgID, err)
		return nil, fmt.Errorf("bind floating ip: %w", err)
	}
	if err := p.serverRepo.IncrementFIPCount(server.ID); err != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: IncrementFIPCount server_id=%d failed: %v", tgID, server.ID, err)
	}

	log.Printf("[Premium] ProvisionForUser tg_id=%d: step=StartPremiumContainers SSH=%s bind=%s port1=%d port2=%d",
		tgID, server.IP, floatingIP.IP, domain.PremiumPortEE1, domain.PremiumPortEE2)
	startErr := p.sshStartPremiumContainers(ctx, sshClient, user.TGID, floatingIP.IP, secretEE1, secretEE2)
	if startErr != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: StartPremiumContainers FAILED: %v", tgID, startErr)
		// Оставляем Status=inactive, чтобы пользователь не получил недоступные ключи.
	} else {
		placeholder.Status = domain.ProxyStatusActive
	}

	placeholder.FloatingIP = floatingIP.IP
	placeholder.TimewebFloatingIPID = floatingIP.ID
	placeholder.IP = floatingIP.IP // единственный адрес для tg://proxy (персональный FIP)
	log.Printf("[Premium] ProvisionForUser tg_id=%d: DONE floating_ip=%s fip_id=%s port1=%d port2=%d status=%s",
		tgID, floatingIP.IP, floatingIP.ID, domain.PremiumPortEE1, domain.PremiumPortEE2, placeholder.Status)
	return placeholder, nil
}

// ProvisionExistingProxyForUser финализирует provisioning для уже созданного proxy (placeholder),
// используя уже сохраненные dd/ee секреты (не генерирует ee повторно).
func (p *PremiumProvisioner) ProvisionExistingProxyForUser(ctx context.Context, user *domain.User, proxy *domain.ProxyNode) (*domain.ProxyNode, error) {
	tgID := user.TGID
	log.Printf("[Premium] ProvisionExistingProxyForUser: start tg_id=%d user_id=%d zone=%s", tgID, user.ID, p.zone)
	if proxy == nil {
		return nil, errors.New("proxy is nil")
	}
	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d proxy_id=%d", tgID, proxy.ID)
	if proxy.Secret == "" || proxy.SecretEE == "" {
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: FAILED empty secrets", tgID)
		return nil, errors.New("proxy secrets are empty")
	}
	p.logPremiumPool("ProvisionExistingProxyForUser")
	server, err := p.getAvailableServer()
	if err != nil {
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: no available server: %v", tgID, err)
		return nil, err
	}
	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: selected server_id=%d ip=%s", tgID, server.ID, server.IP)

	ownerID := user.ID
	if proxy.OwnerID == nil {
		proxy.OwnerID = &ownerID
	}
	proxy.PremiumServerID = &server.ID

	if err := p.ensureSSHRootPassword(ctx, server); err != nil {
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: ensureSSHRootPassword FAILED: %v", tgID, err)
		return nil, fmt.Errorf("ensure ssh root password: %w", err)
	}

	sshClient := p.newSSHClient(server)
	if err := p.sshPreparePremiumHost(ctx, sshClient, "ProvisionExistingProxy"); err != nil {
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: sshPreparePremiumHost FAILED: %v", tgID, err)
		return nil, err
	}

	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: CreateFloatingIP zone=%s", tgID, p.zone)
	floatingIP, err := p.twClient.CreateFloatingIP(ctx, p.zone)
	if err != nil {
		if errors.Is(err, ErrFloatingIPDailyLimit) {
			log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: daily FIP limit (placeholder unchanged)", tgID)
			return proxy, ErrFloatingIPDailyLimit
		}
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: FAILED CreateFloatingIP: %v", tgID, err)
		return nil, err
	}
	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: floating IP id=%s ip=%s", tgID, floatingIP.ID, floatingIP.IP)

	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: BindFloatingIP fip=%s server=%d", tgID, floatingIP.ID, server.TimewebID)
	if err := p.twClient.BindFloatingIP(ctx, floatingIP.ID, server.TimewebID); err != nil {
		_ = p.twClient.DeleteFloatingIP(ctx, floatingIP.ID)
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: FAILED BindFloatingIP: %v", tgID, err)
		return nil, fmt.Errorf("bind floating ip: %w", err)
	}

	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: StartPremiumContainers ssh=%s bind=%s", tgID, server.IP, floatingIP.IP)
	startErr := p.sshStartPremiumContainers(ctx, sshClient, user.TGID, floatingIP.IP, proxy.Secret, proxy.SecretEE)
	if startErr != nil {
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: StartPremiumContainers FAILED: %v", tgID, startErr)
		proxy.Status = domain.ProxyStatusInactive
	} else {
		proxy.Status = domain.ProxyStatusActive
	}

	proxy.FloatingIP = floatingIP.IP
	proxy.TimewebFloatingIPID = floatingIP.ID
	proxy.IP = floatingIP.IP
	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: DONE ip=%s fip_id=%s status=%s",
		tgID, proxy.IP, proxy.TimewebFloatingIPID, proxy.Status)
	return proxy, nil
}

// RestartContainersForUser поднимает контейнеры с теми же секретами (при продлении подписки).
func (p *PremiumProvisioner) RestartContainersForUser(ctx context.Context, user *domain.User, proxy *domain.ProxyNode) error {
	tgID := user.TGID
	if proxy == nil {
		return errors.New("proxy is nil")
	}
	log.Printf("[Premium] RestartContainersForUser: start tg_id=%d user_id=%d proxy_id=%d", tgID, user.ID, proxy.ID)
	if proxy.PremiumServerID == nil || *proxy.PremiumServerID == 0 {
		return errors.New("proxy.PremiumServerID is empty")
	}
	// Биндим только персональный FIP (в новых строках он в FloatingIP и дублируется в IP для клиента).
	bindIP := strings.TrimSpace(proxy.FloatingIP)
	if bindIP == "" && strings.TrimSpace(proxy.TimewebFloatingIPID) != "" {
		bindIP = strings.TrimSpace(proxy.IP) // legacy: FIP мог быть только в IP
	}
	if bindIP == "" {
		return errors.New("premium proxy: пустой floating IP — нужен персональный FIP пользователя")
	}

	server, err := p.serverRepo.GetByID(*proxy.PremiumServerID)
	if err != nil || server == nil {
		log.Printf("[Premium] RestartContainersForUser tg_id=%d: FAILED server lookup: %v", tgID, err)
		return fmt.Errorf("premium server not found")
	}

	if err := p.ensureSSHRootPassword(ctx, server); err != nil {
		log.Printf("[Premium] RestartContainersForUser tg_id=%d: ensureSSHRootPassword FAILED: %v", tgID, err)
		return fmt.Errorf("ensure ssh root password: %w", err)
	}

	sshClient := p.newSSHClient(server)
	if err := p.sshPreparePremiumHost(ctx, sshClient, "RestartContainers"); err != nil {
		log.Printf("[Premium] RestartContainersForUser tg_id=%d: sshPreparePremiumHost FAILED: %v", tgID, err)
		return err
	}
	log.Printf("[Premium] RestartContainersForUser tg_id=%d: SSH=%s bind_floating_ip=%s", tgID, server.IP, bindIP)
	if err := p.sshStartPremiumContainers(ctx, sshClient, user.TGID, bindIP, proxy.Secret, proxy.SecretEE); err != nil {
		log.Printf("[Premium] RestartContainersForUser tg_id=%d: FAILED: %v", tgID, err)
		return err
	}
	log.Printf("[Premium] RestartContainersForUser tg_id=%d: DONE", tgID)
	return nil
}

// ReplaceFloatingIP меняет floating IP для Premium юзера.
// Контейнеры перезапускаются с теми же секретами.
func (p *PremiumProvisioner) ReplaceFloatingIP(ctx context.Context, user *domain.User, proxy *domain.ProxyNode) (newIP string, newFloatingIPID string, err error) {
	if proxy == nil || proxy.PremiumServerID == nil || *proxy.PremiumServerID == 0 {
		return "", "", errors.New("invalid proxy/premium server id")
	}

	server, err := p.serverRepo.GetByID(*proxy.PremiumServerID)
	if err != nil || server == nil {
		return "", "", fmt.Errorf("premium server not found")
	}
	if effectiveFIPToday(server) >= maxFIPPerServerPerDay {
		log.Printf("[Premium ReplaceFloatingIP] server_id=%d ip=%s: local FIP daily limit %d reached", server.ID, server.IP, maxFIPPerServerPerDay)
		return "", "", ErrFloatingIPDailyLimit
	}

	if err := p.ensureSSHRootPassword(ctx, server); err != nil {
		return "", "", fmt.Errorf("ensure ssh root password: %w", err)
	}

	sshClient := p.newSSHClient(server)
	if err := p.sshPreparePremiumHost(ctx, sshClient, "ReplaceFloatingIP"); err != nil {
		return "", "", err
	}

	// Создаем новый floating IP сначала.
	newFloating, err := p.twClient.CreateFloatingIP(ctx, p.zone)
	if err != nil {
		return "", "", fmt.Errorf("create new floating ip: %w", err)
	}

	// Привязываем новый IP.
	if err := p.twClient.BindFloatingIP(ctx, newFloating.ID, server.TimewebID); err != nil {
		_ = p.twClient.DeleteFloatingIP(ctx, newFloating.ID)
		return "", "", fmt.Errorf("bind new floating ip: %w", err)
	}

	// Перезапускаем контейнеры на новом IP.
	_ = p.sshStartPremiumContainers(ctx, sshClient, user.TGID, newFloating.IP, proxy.Secret, proxy.SecretEE)
	if err := p.serverRepo.IncrementFIPCount(server.ID); err != nil {
		log.Printf("[Premium ReplaceFloatingIP] IncrementFIPCount server_id=%d failed: %v", server.ID, err)
	}

	// Удаляем старый floating IP (best-effort).
	if proxy.TimewebFloatingIPID != "" {
		_ = p.twClient.UnbindFloatingIP(ctx, proxy.TimewebFloatingIPID)
		_ = p.twClient.DeleteFloatingIP(ctx, proxy.TimewebFloatingIPID)
	}

	return newFloating.IP, newFloating.ID, nil
}

// DeprovisionForUser останавливает контейнеры и удаляет floating IP при истечении подписки.
func (p *PremiumProvisioner) DeprovisionForUser(ctx context.Context, user *domain.User, proxy *domain.ProxyNode) error {
	if proxy == nil {
		return nil
	}
	tgID := user.TGID
	log.Printf("[Premium] DeprovisionForUser: start tg_id=%d user_id=%d proxy_id=%d fip_id=%q", tgID, user.ID, proxy.ID, proxy.TimewebFloatingIPID)
	if proxy.PremiumServerID != nil && *proxy.PremiumServerID != 0 {
		server, err := p.serverRepo.GetByID(*proxy.PremiumServerID)
		if err == nil && server != nil {
			log.Printf("[Premium] DeprovisionForUser tg_id=%d: stopping containers on server %s", tgID, server.IP)
			if err := p.ensureSSHRootPassword(ctx, server); err != nil {
				log.Printf("[Premium] DeprovisionForUser tg_id=%d: ensureSSHRootPassword failed, skip SSH stop: %v", tgID, err)
			} else {
				sshClient := p.newSSHClient(server)
				sshClient.StopPremiumContainers(ctx, user.TGID)
			}
		} else {
			log.Printf("[Premium] DeprovisionForUser tg_id=%d: skip SSH stop (server lookup err=%v)", tgID, err)
		}
	}
	if proxy.TimewebFloatingIPID != "" {
		log.Printf("[Premium] DeprovisionForUser tg_id=%d: unbind+delete floating IP id=%s ip=%s", tgID, proxy.TimewebFloatingIPID, proxy.FloatingIP)
		if err := p.twClient.UnbindFloatingIP(ctx, proxy.TimewebFloatingIPID); err != nil && !errors.Is(err, timeweb.ErrFloatingIPNotFound) {
			log.Printf("[Premium] DeprovisionForUser tg_id=%d: FAILED unbind floating IP: %v", tgID, err)
		}
		if err := p.twClient.DeleteFloatingIP(ctx, proxy.TimewebFloatingIPID); err != nil && !errors.Is(err, timeweb.ErrFloatingIPNotFound) {
			log.Printf("[Premium] DeprovisionForUser tg_id=%d: FAILED delete floating IP: %v", tgID, err)
			return fmt.Errorf("delete floating ip: %w", err)
		}
	}
	log.Printf("[Premium] DeprovisionForUser tg_id=%d: DONE", tgID)
	return nil
}

// CreateVPSFromRequest создаёт новый Premium VPS по подтверждённой заявке менеджера.
// Сервер попадает в пул premium_servers сразу после появления основного IPv4 (до долгого SSH/Docker),
// чтобы пользователи не получали «нет активного сервера», пока идёт установка Docker.
// При ошибке после перевода заявки в creating статус заявки откатывается в pending (можно подтвердить снова).
// Если POST /servers уже прошёл, TimewebServerID на заявке сохраняется — повторный запуск продолжит с того же VPS.
func (p *PremiumProvisioner) CreateVPSFromRequest(ctx context.Context, req *domain.VPSProvisionRequest) (*domain.PremiumServer, error) {
	if req == nil {
		return nil, errors.New("req is nil")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.RegionID) == "" || strings.TrimSpace(req.OSImageID) == "" || req.ConfigID <= 0 {
		return nil, errors.New("invalid request params")
	}
	if req.Status == "done" {
		return nil, fmt.Errorf("request already processed (status=%s)", req.Status)
	}

	var enteredCreating bool
	var success bool
	defer func() {
		if !success && enteredCreating && req.ID != 0 {
			req.Status = "pending"
			if err := p.provisionReqRepo.Update(req); err != nil {
				log.Printf("[Premium] CreateVPSFromRequest: rollback request %d to pending failed: %v", req.ID, err)
			} else {
				log.Printf("[Premium] CreateVPSFromRequest: request %d → pending после ошибки (повторите подтверждение; Timeweb id=%d сохранён)", req.ID, req.TimewebServerID)
			}
		}
	}()

	if req.Status != "creating" {
		if req.Status == "pending" {
			req.Status = "confirmed"
			_ = p.provisionReqRepo.Update(req)
		}
		req.Status = "creating"
		_ = p.provisionReqRepo.Update(req)
	}
	enteredCreating = true

	createReq := timeweb.CreateServerRequest{
		Name:             req.Name,
		PresetID:         req.ConfigID,
		AvailabilityZone: req.RegionID,
		IsDDOSGuard:      false,
	}
	if osID, err := strconv.Atoi(strings.TrimSpace(req.OSImageID)); err == nil && osID > 0 {
		createReq.OsID = osID
	} else if id := strings.TrimSpace(req.OSImageID); id != "" {
		createReq.ImageID = id
	}
	// Переходим на password auth: не передаём SSHKeysIDs в create-server.

	var twID int
	if req.TimewebServerID > 0 {
		storedTW := req.TimewebServerID
		_, errProbe := p.twClient.GetServer(ctx, storedTW)
		if errProbe != nil && errors.Is(errProbe, timeweb.ErrServerNotFound) {
			log.Printf("[Premium] CreateVPSFromRequest: Timeweb server_id=%d не найден (удалён в облаке) — сброс timeweb_server_id и создание нового VPS", storedTW)
			req.TimewebServerID = 0
			if err := p.provisionReqRepo.Update(req); err != nil {
				return nil, fmt.Errorf("clear stale timeweb_server_id: %w", err)
			}
			if ps, _ := p.serverRepo.GetByTimewebID(storedTW); ps != nil {
				if delErr := p.serverRepo.Delete(ps.ID); delErr != nil {
					log.Printf("[Premium] CreateVPSFromRequest: не удалось удалить устаревший premium_servers id=%d: %v", ps.ID, delErr)
				} else {
					log.Printf("[Premium] CreateVPSFromRequest: удалена устаревшая запись premium_servers id=%d (timeweb_id=%d)", ps.ID, storedTW)
				}
			}
		} else if errProbe != nil {
			return nil, fmt.Errorf("get server (проверка resume): %w", errProbe)
		} else {
			twID = storedTW
			log.Printf("[Premium] CreateVPSFromRequest: resume Timeweb server_id=%d (request %d)", twID, req.ID)
		}
	}
	if twID == 0 {
		srvOut, err := p.twClient.CreateServer(ctx, createReq)
		if err != nil {
			return nil, fmt.Errorf("create server: %w", err)
		}
		twID = srvOut.ID
		req.TimewebServerID = twID
		if err := p.provisionReqRepo.Update(req); err != nil {
			return nil, fmt.Errorf("persist timeweb_server_id on request: %w", err)
		}
		log.Printf("[Premium] CreateVPSFromRequest: Timeweb POST /servers ok id=%d, saved on request %d", twID, req.ID)
	}

	if err := p.twClient.WaitServerReady(ctx, twID); err != nil {
		return nil, fmt.Errorf("wait server ready: %w", err)
	}

	if err := p.ensurePublicIPv4ForServer(ctx, twID); err != nil {
		return nil, err
	}

	srv, err := p.twClient.GetServer(ctx, twID)
	if err != nil {
		return nil, fmt.Errorf("get server after ready: %w", err)
	}
	mainIP := timeweb.ExtractMainIPv4(srv)
	if mainIP == "" {
		return nil, errors.New("нет публичного IPv4 у сервера (после AddServerIP ipv4)")
	}

	premiumServer, err := p.serverRepo.GetByTimewebID(twID)
	if err != nil {
		return nil, fmt.Errorf("lookup premium server by timeweb id: %w", err)
	}
	if premiumServer == nil {
		premiumServer = &domain.PremiumServer{
			Name:        req.Name,
			IP:          mainIP,
			TimewebID:   twID,
			IsActive:    true,
			SSHPassword: strings.TrimSpace(srv.RootPass),
		}
		if err := p.serverRepo.Create(premiumServer); err != nil {
			return nil, fmt.Errorf("save premium server: %w", err)
		}
		log.Printf("[Premium] CreateVPSFromRequest: premium_servers id=%d ip=%s — в пуле до SSH/Docker", premiumServer.ID, mainIP)
	} else {
		if mainIP != "" && premiumServer.IP != mainIP {
			premiumServer.IP = mainIP
		}
		if pass := strings.TrimSpace(srv.RootPass); pass != "" && premiumServer.SSHPassword == "" {
			premiumServer.SSHPassword = pass
		}
		_ = p.serverRepo.Update(premiumServer)
		log.Printf("[Premium] CreateVPSFromRequest: reuse premium_servers id=%d ip=%s", premiumServer.ID, premiumServer.IP)
	}

	if active, errAct := p.serverRepo.GetAllActive(); errAct != nil {
		log.Printf("[Premium] CreateVPSFromRequest: pool size log err=%v", errAct)
	} else {
		log.Printf("[Premium] CreateVPSFromRequest: active premium_servers in pool=%d", len(active))
	}

	if err := p.ensureSSHRootPassword(ctx, premiumServer); err != nil {
		return nil, fmt.Errorf("ensure ssh root password: %w", err)
	}

	sshClient := p.newSSHClient(premiumServer)
	if err := p.sshPreparePremiumHost(ctx, sshClient, "CreateVPSFromRequest"); err != nil {
		return nil, err
	}

	req.Status = "done"
	_ = p.provisionReqRepo.Update(req)
	success = true
	log.Printf("[Premium] CreateVPSFromRequest: request %d done, premium_server id=%d", req.ID, premiumServer.ID)
	return premiumServer, nil
}

// ensurePublicIPv4ForServer гарантирует публичный IPv4 для SSH/Docker: при только IPv6 вызывает POST .../servers/{id}/ips.
func (p *PremiumProvisioner) ensurePublicIPv4ForServer(ctx context.Context, twID int) error {
	srv, err := p.twClient.GetServer(ctx, twID)
	if err != nil {
		return fmt.Errorf("get server: %w", err)
	}
	if timeweb.ExtractMainIPv4(srv) != "" {
		return nil
	}
	log.Printf("[Premium] server timeweb_id=%d: нет публичного IPv4 в API — AddServerIP(type=ipv4)", twID)
	assigned, err := p.twClient.AddServerIP(ctx, twID, "ipv4")
	if err != nil {
		return fmt.Errorf("timeweb AddServerIP ipv4: %w", err)
	}
	log.Printf("[Premium] server timeweb_id=%d: AddServerIP ответ ip=%q", twID, assigned)

	waitCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		srv, err := p.twClient.GetServer(waitCtx, twID)
		if err != nil {
			return err
		}
		if v := timeweb.ExtractMainIPv4(srv); v != "" {
			log.Printf("[Premium] server timeweb_id=%d: публичный IPv4 в сети: %s", twID, v)
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("таймаут ожидания публичного IPv4 на сервере %d после AddServerIP: %w", twID, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// parsePendingUserIDs — полезно для handler/worker.
