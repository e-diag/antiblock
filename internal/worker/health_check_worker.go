package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/alert"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// HealthCheckWorker проверяет только free-прокси и шлёт алерты в служебный чат при сбоях/восстановлении.
type HealthCheckWorker struct {
	proxyUC usecase.ProxyUseCase
	alerts  *alert.TelegramAlerter
	config  config.WorkerConfig
	stop     chan struct{}
	stopOnce sync.Once

	// prevMu защищает prevStatus от конкурентного доступа (checkProxies может вызываться по таймеру и при рефакторинге — из других мест).
	prevMu     sync.Mutex
	prevStatus map[uint]bool // proxyID -> true если был недоступен
}

func NewHealthCheckWorker(proxyUC usecase.ProxyUseCase, alerts *alert.TelegramAlerter, cfg config.WorkerConfig) *HealthCheckWorker {
	return &HealthCheckWorker{
		proxyUC:    proxyUC,
		alerts:     alerts,
		config:     cfg,
		stop:       make(chan struct{}),
		prevStatus: make(map[uint]bool),
	}
}

func (w *HealthCheckWorker) Start() {
	if !w.config.Enabled {
		log.Println("Health check worker is disabled")
		return
	}
	log.Printf("Starting health check worker (interval: %v)", w.config.Interval())
	ticker := time.NewTicker(w.config.Interval())
	defer ticker.Stop()
	w.checkProxies()
	for {
		select {
		case <-ticker.C:
			w.checkProxies()
		case <-w.stop:
			log.Println("Health check worker stopped")
			return
		}
	}
}

func (w *HealthCheckWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

type proxyCheckResult struct {
	proxy     *domain.ProxyNode
	reachable bool
	rttMs     int
}

func (w *HealthCheckWorker) checkProxies() {
	// Проверяем только Free прокси; Premium проверяет отдельный PremiumHealthCheckWorker.
	proxies, err := w.proxyUC.GetAllFreeProxies()
	if err != nil {
		log.Printf("HealthCheck: GetAllFreeProxies error: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		w.alerts.Send(ctx, alert.Report{
			Type:    "health_check_list_free",
			Source:  "worker/health_check",
			Tariff:  "free",
			ErrText: err.Error(),
		})
		cancel()
		return
	}

	// Шаг 1: все сетевые проверки — без мьютекса.
	freeResults := make([]proxyCheckResult, 0, len(proxies))
	for _, proxy := range proxies {
		reachable, rttMs := w.proxyUC.CheckFreeProxy(proxy)
		freeResults = append(freeResults, proxyCheckResult{proxy, reachable, rttMs})
	}

	// Шаг 2: обновляем map и собираем отчёты — только под мьютексом, без сетевых вызовов.
	var reports []alert.Report

	w.prevMu.Lock()
	for _, r := range freeResults {
		wasUnreachable := w.prevStatus[r.proxy.ID]
		if !r.reachable {
			if !wasUnreachable {
				w.prevStatus[r.proxy.ID] = true
				reports = append(reports, alert.Report{
					Type:    "free_proxy_unreachable",
					Source:  "worker/health_check",
					Tariff:  "free",
					ProxyID: r.proxy.ID,
					IP:      r.proxy.IP,
					Port:    r.proxy.Port,
					ErrText: "прокси недоступен (health check)",
				})
				log.Printf("HealthCheck: free proxy ID=%d %s:%d is DOWN", r.proxy.ID, r.proxy.IP, r.proxy.Port)
			}
		} else {
			if wasUnreachable {
				delete(w.prevStatus, r.proxy.ID)
				reports = append(reports, alert.Report{
					Type:    "free_proxy_recovered",
					Source:  "worker/health_check",
					Tariff:  "free",
					ProxyID: r.proxy.ID,
					IP:      r.proxy.IP,
					Port:    r.proxy.Port,
					Extra:   fmt.Sprintf("RTT=%d ms", r.rttMs),
					ErrText: "прокси снова доступен",
				})
				log.Printf("HealthCheck: free proxy ID=%d %s:%d is UP (RTT=%dms)", r.proxy.ID, r.proxy.IP, r.proxy.Port, r.rttMs)
			}
		}
	}
	w.prevMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, rep := range reports {
		w.alerts.Send(ctx, rep)
	}
}
