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
func (p *PremiumProvisioner) newSSHClient(server *domain.PremiumServer) *timeweb.SSHClient {
	client := timeweb.NewSSHClient(server.IP, 22, p.sshUser, p.sshKeyPath)
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

// ProvisionForUser создаёт floating IP и запускает контейнеры для нового Premium-юзера.
// В случае ErrFloatingIPDailyLimit возвращает (placeholderProxy, ErrFloatingIPDailyLimit),
// чтобы dd/ee секреты были сгенерированы один раз на момент первой покупки.
func (p *PremiumProvisioner) ProvisionForUser(ctx context.Context, user *domain.User, secretDD string) (*domain.ProxyNode, error) {
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

	ownerID := user.ID
	sshClient := p.newSSHClient(server)

	// Генерируем ee-секрет сразу (чтобы он не менялся при последующей выдаче/провижининге).
	log.Printf("[Premium] ProvisionForUser tg_id=%d: step=GenerateEESecret via SSH %s", tgID, server.IP)
	secretEE, err := sshClient.GenerateEESecret(ctx)
	if err != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: FAILED at GenerateEESecret: %v", tgID, err)
		return nil, fmt.Errorf("generate ee secret: %w", err)
	}
	log.Printf("[Premium] ProvisionForUser tg_id=%d: ee secret generated prefix=%.8s…", tgID, secretEE)

	placeholder := &domain.ProxyNode{
		IP:                  server.IP, // временно; после успешного floating IP обновим.
		Port:                domain.PremiumPortDD,
		Secret:              secretDD,
		SecretEE:            secretEE,
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

	log.Printf("[Premium] ProvisionForUser tg_id=%d: step=StartPremiumContainers SSH=%s bind=%s portDD=%d portEE=%d dd_secret=%.8s…",
		tgID, server.IP, floatingIP.IP, domain.PremiumPortDD, domain.PremiumPortEE, secretDD)
	if err := sshClient.StartPremiumContainers(ctx, user.TGID, floatingIP.IP, secretDD, secretEE); err != nil {
		log.Printf("[Premium] ProvisionForUser tg_id=%d: StartPremiumContainers non-fatal: %v", tgID, err)
	}

	placeholder.FloatingIP = floatingIP.IP
	placeholder.TimewebFloatingIPID = floatingIP.ID
	placeholder.IP = floatingIP.IP
	placeholder.Status = domain.ProxyStatusActive
	log.Printf("[Premium] ProvisionForUser tg_id=%d: DONE floating_ip=%s fip_id=%s portDD=%d portEE=%d",
		tgID, floatingIP.IP, floatingIP.ID, domain.PremiumPortDD, domain.PremiumPortEE)
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

	sshClient := p.newSSHClient(server)

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
	if err := sshClient.StartPremiumContainers(ctx, user.TGID, floatingIP.IP, proxy.Secret, proxy.SecretEE); err != nil {
		log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: StartPremiumContainers non-fatal: %v", tgID, err)
	}

	proxy.FloatingIP = floatingIP.IP
	proxy.TimewebFloatingIPID = floatingIP.ID
	proxy.IP = floatingIP.IP
	proxy.Status = domain.ProxyStatusActive
	log.Printf("[Premium] ProvisionExistingProxyForUser tg_id=%d: DONE ip=%s fip_id=%s", tgID, proxy.IP, proxy.TimewebFloatingIPID)
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
	if proxy.FloatingIP == "" && proxy.IP != "" {
		// На всякий случай: используем proxy.IP если FloatingIP не заполнено.
		proxy.FloatingIP = proxy.IP
	}
	if proxy.FloatingIP == "" {
		return errors.New("proxy floating ip is empty")
	}

	server, err := p.serverRepo.GetByID(*proxy.PremiumServerID)
	if err != nil || server == nil {
		log.Printf("[Premium] RestartContainersForUser tg_id=%d: FAILED server lookup: %v", tgID, err)
		return fmt.Errorf("premium server not found")
	}

	sshClient := p.newSSHClient(server)
	log.Printf("[Premium] RestartContainersForUser tg_id=%d: SSH=%s floating_ip=%s", tgID, server.IP, proxy.FloatingIP)
	if err := sshClient.StartPremiumContainers(ctx, user.TGID, proxy.FloatingIP, proxy.Secret, proxy.SecretEE); err != nil {
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

	sshClient := p.newSSHClient(server)

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
	if err := sshClient.StartPremiumContainers(ctx, user.TGID, newFloating.IP, proxy.Secret, proxy.SecretEE); err != nil {
		log.Printf("[Premium ReplaceFloatingIP] StartPremiumContainers non-fatal: %v", err)
	}
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
			sshClient := p.newSSHClient(server)
			sshClient.StopPremiumContainers(ctx, user.TGID)
		} else {
			log.Printf("[Premium] DeprovisionForUser tg_id=%d: skip SSH stop (server lookup err=%v)", tgID, err)
		}
	}
	if proxy.TimewebFloatingIPID != "" {
		log.Printf("[Premium] DeprovisionForUser tg_id=%d: unbind+delete floating IP id=%s ip=%s", tgID, proxy.TimewebFloatingIPID, proxy.FloatingIP)
		_ = p.twClient.UnbindFloatingIP(ctx, proxy.TimewebFloatingIPID)
		if err := p.twClient.DeleteFloatingIP(ctx, proxy.TimewebFloatingIPID); err != nil {
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

	sshClient := p.newSSHClient(premiumServer)
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	if err := sshClient.WaitSSHReady(waitCtx); err != nil {
		return nil, fmt.Errorf("wait ssh: %w", err)
	}
	if err := sshClient.SetupDocker(waitCtx); err != nil {
		return nil, fmt.Errorf("setup docker: %w", err)
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
