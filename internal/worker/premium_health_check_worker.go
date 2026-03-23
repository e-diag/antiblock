package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// PremiumHealthCheckWorker каждые 15 мин проверяет активные премиум-прокси; при сбое уведомляет админов
// и помечает UnreachableSince; каждые 5 мин перепроверяет недоступные до восстановления.
type PremiumHealthCheckWorker struct {
	bot      *bot.Bot
	proxyUC  usecase.ProxyUseCase
	adminIDs []int64
	cfg      config.PremiumHealthCheckConfig
	stop     chan struct{}
	stopOnce sync.Once
}

func NewPremiumHealthCheckWorker(b *bot.Bot, proxyUC usecase.ProxyUseCase, adminIDs []int64, cfg config.PremiumHealthCheckConfig) *PremiumHealthCheckWorker {
	return &PremiumHealthCheckWorker{
		bot:      b,
		proxyUC:  proxyUC,
		adminIDs: adminIDs,
		cfg:      cfg,
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
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, p := range proxies {
		reachable, _ := w.proxyUC.CheckPremiumProxy(p)
		if !reachable {
			msg := fmt.Sprintf("⚠️ Премиум-прокси ID %d (%s:%d) не отвечает.", p.ID, p.IP, p.Port)
			w.notifyAdmins(ctx, msg)
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
			msg := fmt.Sprintf("✅ Премиум-прокси ID %d (%s:%d) снова доступен.", p.ID, p.IP, p.Port)
			w.notifyAdmins(ctx, msg)
		}
	}
}

func (w *PremiumHealthCheckWorker) notifyAdmins(ctx context.Context, text string) {
	for _, adminID := range w.adminIDs {
		_, _ = w.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    adminID,
			Text:      text,
			ParseMode: "HTML",
		})
	}
}

func (w *PremiumHealthCheckWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}
