package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/alert"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

const defaultPremiumUnreachableAlertCooldown = 4 * time.Hour

// PremiumHealthCheckWorker каждые 15 мин проверяет активные премиум-прокси; при сбое шлёт в служебный чат
// и помечает UnreachableSince; каждые 5 мин перепроверяет недоступные до восстановления.
// Алерт «недоступен»: сразу при первом обнаружении, далее не чаще cooldown для того же proxy_id.
type PremiumHealthCheckWorker struct {
	proxyUC usecase.ProxyUseCase
	alerts  *alert.TelegramAlerter
	cfg     config.PremiumHealthCheckConfig
	stop     chan struct{}
	stopOnce sync.Once

	unreachableAlertMu     sync.Mutex
	lastUnreachableAlert map[uint]time.Time // proxy_nodes.id
}

func NewPremiumHealthCheckWorker(proxyUC usecase.ProxyUseCase, alerts *alert.TelegramAlerter, cfg config.PremiumHealthCheckConfig) *PremiumHealthCheckWorker {
	return &PremiumHealthCheckWorker{
		proxyUC:              proxyUC,
		alerts:               alerts,
		cfg:                  cfg,
		stop:                 make(chan struct{}),
		lastUnreachableAlert: make(map[uint]time.Time),
	}
}

func (w *PremiumHealthCheckWorker) Start() {
	if !w.cfg.Enabled {
		log.Println("Premium health check worker is disabled")
		return
	}
	intervalFull := time.Duration(w.cfg.IntervalSeconds) * time.Second
	intervalRecheck := time.Duration(w.cfg.UnreachableRecheckSeconds) * time.Second
	if intervalFull <= 0 {
		intervalFull = 15 * time.Minute
	}
	if intervalRecheck <= 0 {
		intervalRecheck = 5 * time.Minute
	}
	log.Printf("Starting premium health check worker (full: %v, unreachable recheck: %v)", intervalFull, intervalRecheck)

	go w.runFullCheck(intervalFull)
	go w.runUnreachableRecheck(intervalRecheck)
}

func (w *PremiumHealthCheckWorker) runFullCheck(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	w.checkActivePremium()
	for {
		select {
		case <-ticker.C:
			w.checkActivePremium()
		case <-w.stop:
			return
		}
	}
}

func (w *PremiumHealthCheckWorker) runUnreachableRecheck(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.checkUnreachablePremium()
		case <-w.stop:
			return
		}
	}
}

func (w *PremiumHealthCheckWorker) checkActivePremium() {
	cooldown := time.Duration(w.cfg.UnreachableAlertCooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = defaultPremiumUnreachableAlertCooldown
	}

	proxies, err := w.proxyUC.GetActivePremiumProxies()
	if err != nil {
		log.Printf("Premium health check: GetActivePremiumProxies: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		w.alerts.Send(ctx, alert.Report{
			Type:    "premium_health_list",
			Source:  "worker/premium_health_check",
			Tariff:  "premium",
			ErrText: err.Error(),
		})
		cancel()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	now := time.Now()
	for _, p := range proxies {
		reachable, _ := w.proxyUC.CheckPremiumProxy(p)
		if reachable {
			w.clearUnreachableAlertCooldown(p.ID)
			continue
		}
		// Реконсиляция legacy (ручное удаление контейнеров): запись стала inactive — не шлём алерт.
		if p.Type == domain.ProxyTypePremium && p.Status != domain.ProxyStatusActive {
			w.clearUnreachableAlertCooldown(p.ID)
			continue
		}
		if !w.shouldSendUnreachableAlert(p.ID, now, cooldown) {
			continue
		}
		w.markUnreachableAlert(p.ID, now)
		rep := alert.Report{
			Type:    "premium_proxy_unreachable",
			Source:  "worker/premium_health_check",
			Tariff:  "premium",
			ProxyID: p.ID,
			IP:      p.IP,
			Port:    p.Port,
			ErrText: "не отвечает (health check)",
		}
		if p.OwnerID != nil {
			rep.UserDBID = *p.OwnerID
		}
		if p.TimewebFloatingIPID != "" {
			rep.Extra = "fip_id=" + p.TimewebFloatingIPID
		}
		w.alerts.Send(ctx, rep)
	}
}

func (w *PremiumHealthCheckWorker) checkUnreachablePremium() {
	proxies, err := w.proxyUC.GetUnreachablePremiumProxies()
	if err != nil || len(proxies) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, p := range proxies {
		reachable, _ := w.proxyUC.CheckPremiumProxy(p)
		if reachable {
			w.clearUnreachableAlertCooldown(p.ID)
			rep := alert.Report{
				Type:    "premium_proxy_recovered",
				Source:  "worker/premium_health_check",
				Tariff:  "premium",
				ProxyID: p.ID,
				IP:      p.IP,
				Port:    p.Port,
				ErrText: "снова доступен",
			}
			if p.OwnerID != nil {
				rep.UserDBID = *p.OwnerID
			}
			w.alerts.Send(ctx, rep)
		}
	}
}

func (w *PremiumHealthCheckWorker) shouldSendUnreachableAlert(proxyID uint, now time.Time, cooldown time.Duration) bool {
	w.unreachableAlertMu.Lock()
	defer w.unreachableAlertMu.Unlock()
	last, ok := w.lastUnreachableAlert[proxyID]
	if !ok {
		return true
	}
	return now.Sub(last) >= cooldown
}

func (w *PremiumHealthCheckWorker) markUnreachableAlert(proxyID uint, t time.Time) {
	w.unreachableAlertMu.Lock()
	defer w.unreachableAlertMu.Unlock()
	w.lastUnreachableAlert[proxyID] = t
}

func (w *PremiumHealthCheckWorker) clearUnreachableAlertCooldown(proxyID uint) {
	w.unreachableAlertMu.Lock()
	defer w.unreachableAlertMu.Unlock()
	delete(w.lastUnreachableAlert, proxyID)
}

func (w *PremiumHealthCheckWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}
