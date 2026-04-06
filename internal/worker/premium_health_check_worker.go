package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/yourusername/antiblock/internal/infrastructure/alert"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// PremiumHealthCheckWorker каждые 15 мин проверяет активные премиум-прокси; при сбое шлёт в служебный чат
// и помечает UnreachableSince; каждые 5 мин перепроверяет недоступные до восстановления.
type PremiumHealthCheckWorker struct {
	proxyUC usecase.ProxyUseCase
	alerts  *alert.TelegramAlerter
	cfg     config.PremiumHealthCheckConfig
	stop     chan struct{}
	stopOnce sync.Once
}

func NewPremiumHealthCheckWorker(proxyUC usecase.ProxyUseCase, alerts *alert.TelegramAlerter, cfg config.PremiumHealthCheckConfig) *PremiumHealthCheckWorker {
	return &PremiumHealthCheckWorker{
		proxyUC: proxyUC,
		alerts:  alerts,
		cfg:     cfg,
		stop:     make(chan struct{}),
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
	for _, p := range proxies {
		reachable, _ := w.proxyUC.CheckPremiumProxy(p)
		if !reachable {
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

func (w *PremiumHealthCheckWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}
