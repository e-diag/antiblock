package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
)

var (
	ErrNoActivePremiumServer = errors.New("no active premium server available")
	ErrFloatingIPDailyLimit  = timeweb.ErrFloatingIPDailyLimit
)

// PremiumProvisioner управляет Premium provisioning через TimeWeb (floating IP + docker через SSH).
type PremiumProvisioner struct {
	twClient         *timeweb.Client
	serverRepo       repository.PremiumServerRepository
	provisionReqRepo repository.VPSProvisionRequestRepository

	sshUser     string
	sshPassword string
	zone        string
}

func NewPremiumProvisioner(
	twClient *timeweb.Client,
	serverRepo repository.PremiumServerRepository,
	provisionReqRepo repository.VPSProvisionRequestRepository,
	sshUser, sshPassword, zone string,
) *PremiumProvisioner {
	return &PremiumProvisioner{
		twClient:         twClient,
		serverRepo:       serverRepo,
		provisionReqRepo: provisionReqRepo,
		sshUser:          sshUser,
		sshPassword:      sshPassword,
		zone:             zone,
	}
}

// newSSHClient создаёт SSH клиент с верификацией host key.
// Для нового сервера host key будет сохранён при первом успешном подключении.
func (p *PremiumProvisioner) newSSHClient(server *domain.PremiumServer) *timeweb.SSHClient {
	client := timeweb.NewSSHClient(server.IP, 22, p.sshUser, p.sshPassword)
	if strings.TrimSpace(server.SSHHostKey) != "" {
		return client.WithKnownHostKey(server.SSHHostKey, nil)
	}
	return client.WithKnownHostKey("", func(hostKey string) {
		server.SSHHostKey = hostKey
		if err := p.serverRepo.Update(server); err != nil {
			log.Printf("[SSH] failed to save host key for server %d: %v", server.ID, err)
		} else {
			log.Printf("[SSH] host key saved for server %s", server.IP)
		}
	})
}

// ProvisionForUser создаёт floating IP и запускает контейнеры для нового Premium-юзера.
// В случае ErrFloatingIPDailyLimit возвращает (placeholderProxy, ErrFloatingIPDailyLimit),
// чтобы dd/ee секреты были сгенерированы один раз на момент первой покупки.
func (p *PremiumProvisioner) ProvisionForUser(ctx context.Context, user *domain.User, secretDD string) (*domain.ProxyNode, error) {
	server, err := p.serverRepo.GetActive()
	if err != nil || server == nil {
		return nil, ErrNoActivePremiumServer
	}

	ownerID := user.ID
	sshClient := p.newSSHClient(server)

	// Генерируем ee-секрет сразу (чтобы он не менялся при последующей выдаче/провижининге).
	secretEE, err := sshClient.GenerateEESecret(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate ee secret: %w", err)
	}

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

	floatingIP, err := p.twClient.CreateFloatingIP(ctx, p.zone)
	if err != nil {
		if errors.Is(err, ErrFloatingIPDailyLimit) {
			return placeholder, ErrFloatingIPDailyLimit
		}
		return nil, err
	}

	// Привязка floating IP к серверу.
	if err := p.twClient.BindFloatingIP(ctx, floatingIP.ID, server.TimewebID); err != nil {
		_ = p.twClient.DeleteFloatingIP(ctx, floatingIP.ID)
		return nil, fmt.Errorf("bind floating ip: %w", err)
	}

	// Запуск контейнеров с bind на floating IP.
	if err := sshClient.StartPremiumContainers(ctx, user.TGID, floatingIP.IP, secretDD, secretEE); err != nil {
		log.Printf("[Premium Provision] StartPremiumContainers non-fatal: %v", err)
	}

	placeholder.FloatingIP = floatingIP.IP
	placeholder.TimewebFloatingIPID = floatingIP.ID
	placeholder.IP = floatingIP.IP
	placeholder.Status = domain.ProxyStatusActive
	return placeholder, nil
}

// ProvisionExistingProxyForUser финализирует provisioning для уже созданного proxy (placeholder),
// используя уже сохраненные dd/ee секреты (не генерирует ee повторно).
func (p *PremiumProvisioner) ProvisionExistingProxyForUser(ctx context.Context, user *domain.User, proxy *domain.ProxyNode) (*domain.ProxyNode, error) {
	if proxy == nil {
		return nil, errors.New("proxy is nil")
	}
	if proxy.Secret == "" || proxy.SecretEE == "" {
		return nil, errors.New("proxy secrets are empty")
	}
	server, err := p.serverRepo.GetActive()
	if err != nil || server == nil {
		return nil, ErrNoActivePremiumServer
	}

	ownerID := user.ID
	if proxy.OwnerID == nil {
		proxy.OwnerID = &ownerID
	}
	proxy.PremiumServerID = &server.ID

	sshClient := p.newSSHClient(server)

	floatingIP, err := p.twClient.CreateFloatingIP(ctx, p.zone)
	if err != nil {
		if errors.Is(err, ErrFloatingIPDailyLimit) {
			// Не меняем поля, остаёмся в placeholder-состоянии.
			return proxy, ErrFloatingIPDailyLimit
		}
		return nil, err
	}

	if err := p.twClient.BindFloatingIP(ctx, floatingIP.ID, server.TimewebID); err != nil {
		_ = p.twClient.DeleteFloatingIP(ctx, floatingIP.ID)
		return nil, fmt.Errorf("bind floating ip: %w", err)
	}

	if err := sshClient.StartPremiumContainers(ctx, user.TGID, floatingIP.IP, proxy.Secret, proxy.SecretEE); err != nil {
		log.Printf("[Premium ProvisionExisting] StartPremiumContainers non-fatal: %v", err)
	}

	proxy.FloatingIP = floatingIP.IP
	proxy.TimewebFloatingIPID = floatingIP.ID
	proxy.IP = floatingIP.IP
	proxy.Status = domain.ProxyStatusActive
	return proxy, nil
}

// RestartContainersForUser поднимает контейнеры с теми же секретами (при продлении подписки).
func (p *PremiumProvisioner) RestartContainersForUser(ctx context.Context, user *domain.User, proxy *domain.ProxyNode) error {
	if proxy == nil {
		return errors.New("proxy is nil")
	}
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
		return fmt.Errorf("premium server not found")
	}

	sshClient := p.newSSHClient(server)
	return sshClient.StartPremiumContainers(ctx, user.TGID, proxy.FloatingIP, proxy.Secret, proxy.SecretEE)
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
	if proxy.PremiumServerID != nil && *proxy.PremiumServerID != 0 {
		server, err := p.serverRepo.GetByID(*proxy.PremiumServerID)
		if err == nil && server != nil {
			sshClient := p.newSSHClient(server)
			sshClient.StopPremiumContainers(ctx, user.TGID)
		}
	}
	if proxy.TimewebFloatingIPID != "" {
		_ = p.twClient.UnbindFloatingIP(ctx, proxy.TimewebFloatingIPID)
		if err := p.twClient.DeleteFloatingIP(ctx, proxy.TimewebFloatingIPID); err != nil {
			return fmt.Errorf("delete floating ip: %w", err)
		}
	}
	return nil
}

// CreateVPSFromRequest создаёт новый Premium VPS по подтверждённой заявке менеджера.
// После создания устанавливает Docker и регистрирует сервер в БД.
func (p *PremiumProvisioner) CreateVPSFromRequest(ctx context.Context, req *domain.VPSProvisionRequest) (*domain.PremiumServer, error) {
	if req == nil {
		return nil, errors.New("req is nil")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.RegionID) == "" || strings.TrimSpace(req.OSImageID) == "" || req.ConfigID <= 0 {
		return nil, errors.New("invalid request params")
	}
	if req.Status == "creating" || req.Status == "done" {
		return nil, fmt.Errorf("request already processed (status=%s)", req.Status)
	}

	// Меняем статус (best-effort).
	if req.Status == "pending" {
		req.Status = "confirmed"
		_ = p.provisionReqRepo.Update(req)
	}
	req.Status = "creating"
	_ = p.provisionReqRepo.Update(req)

	createReq := timeweb.CreateServerRequest{
		Name:             req.Name,
		PresetID:         req.ConfigID,
		ImageID:          req.OSImageID,
		AvailabilityZone: req.RegionID,
		IsDDOSGuard:      false,
	}

	serverInfo, err := p.twClient.CreateServer(ctx, createReq)
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}
	if err := p.twClient.WaitServerReady(ctx, serverInfo.ID); err != nil {
		return nil, fmt.Errorf("wait server ready: %w", err)
	}

	// Берём финальную инфу, чтобы достать основной IPv4.
	srv, err := p.twClient.GetServer(ctx, serverInfo.ID)
	if err != nil {
		return nil, fmt.Errorf("get server after ready: %w", err)
	}

	mainIP := extractMainIPv4(srv)
	if mainIP == "" {
		return nil, errors.New("cannot extract server main IPv4")
	}

	hostKeySeen := ""
	sshClient := timeweb.NewSSHClient(mainIP, 22, p.sshUser, p.sshPassword).WithKnownHostKey("", func(hostKey string) {
		hostKeySeen = hostKey
	})
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	if err := sshClient.WaitSSHReady(waitCtx); err != nil {
		return nil, fmt.Errorf("wait ssh: %w", err)
	}
	if err := sshClient.SetupDocker(waitCtx); err != nil {
		return nil, fmt.Errorf("setup docker: %w", err)
	}

	// Делаем новый сервер активным: выключаем остальные.
	if all, errAll := p.serverRepo.GetAll(); errAll == nil {
		for _, s := range all {
			if s == nil {
				continue
			}
			s.IsActive = false
			_ = p.serverRepo.Update(s)
		}
	}

	premiumServer := &domain.PremiumServer{
		Name:      req.Name,
		IP:        mainIP,
		TimewebID: serverInfo.ID,
		IsActive:  true,
		SSHHostKey: hostKeySeen,
	}
	if err := p.serverRepo.Create(premiumServer); err != nil {
		return nil, fmt.Errorf("save premium server: %w", err)
	}

	req.Status = "done"
	_ = p.provisionReqRepo.Update(req)

	return premiumServer, nil
}

func extractMainIPv4(srv *timeweb.Server) string {
	if srv == nil {
		return ""
	}
	for _, n := range srv.Networks {
		if strings.EqualFold(n.Type, "public") {
			for _, ip := range n.Ips {
				if ip.IsMain && strings.EqualFold(ip.Type, "ipv4") && ip.IP != "" {
					return ip.IP
				}
			}
			// fallback: главный ip без строгого типа ipv4
			for _, ip := range n.Ips {
				if ip.IsMain && ip.IP != "" {
					return ip.IP
				}
			}
		}
	}
	return ""
}

// parsePendingUserIDs — полезно для handler/worker.
func parsePendingUserIDs(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

