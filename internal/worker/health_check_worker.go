package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// HealthCheckWorker проверяет все free- и premium-прокси и уведомляет менеджеров при сбоях/восстановлении.
type HealthCheckWorker struct {
	proxyUC  usecase.ProxyUseCase
	bot      *bot.Bot
	adminIDs []int64
	config   config.WorkerConfig
	stop     chan struct{}

	// prevMu защищает prevStatus от конкурентного доступа (checkProxies может вызываться по таймеру и при рефакторинге — из других мест).
	prevMu     sync.Mutex
	prevStatus map[uint]bool // proxyID -> true если был недоступен
}

func NewHealthCheckWorker(proxyUC usecase.ProxyUseCase, b *bot.Bot, adminIDs []int64, cfg config.WorkerConfig) *HealthCheckWorker {
	return &HealthCheckWorker{
		proxyUC:    proxyUC,
		bot:        b,
		adminIDs:   adminIDs,
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
	close(w.stop)
}

type proxyCheckResult struct {
	proxy     *domain.ProxyNode
	reachable bool
	rttMs     int // используется только для free-прокси; для premium — 0
}

func (w *HealthCheckWorker) checkProxies() {
	proxies, err := w.proxyUC.GetAllFreeProxies()
	if err != nil {
		log.Printf("HealthCheck: GetAllFreeProxies error: %v", err)
		return
	}

	activePremium, _ := w.proxyUC.GetActivePremiumProxies()
	unreachablePremium, _ := w.proxyUC.GetUnreachablePremiumProxies()

	// Шаг 1: все сетевые проверки — без мьютекса.
	freeResults := make([]proxyCheckResult, 0, len(proxies))
	for _, proxy := range proxies {
		reachable, rttMs := w.proxyUC.CheckFreeProxy(proxy)
		freeResults = append(freeResults, proxyCheckResult{proxy, reachable, rttMs})
	}

	premiumAll := append(activePremium, unreachablePremium...)
	premiumResults := make([]proxyCheckResult, 0, len(premiumAll))
	for _, proxy := range premiumAll {
		reachable, _ := w.proxyUC.CheckPremiumProxy(proxy)
		premiumResults = append(premiumResults, proxyCheckResult{proxy: proxy, reachable: reachable})
	}

	// Шаг 2: обновляем map и собираем уведомления — только под мьютексом, без сетевых вызовов.
	var notifications []string

	w.prevMu.Lock()
	for _, r := range freeResults {
		wasUnreachable := w.prevStatus[r.proxy.ID]
		if !r.reachable {
			if !wasUnreachable {
				w.prevStatus[r.proxy.ID] = true
				notifications = append(notifications, fmt.Sprintf(
					"⚠️ <b>Free-прокси недоступен</b>\n\nID: %d\n🌐 %s:%d\n\nПроверьте сервер.",
					r.proxy.ID, r.proxy.IP, r.proxy.Port,
				))
				log.Printf("HealthCheck: free proxy ID=%d %s:%d is DOWN", r.proxy.ID, r.proxy.IP, r.proxy.Port)
			}
		} else {
			if wasUnreachable {
				delete(w.prevStatus, r.proxy.ID)
				notifications = append(notifications, fmt.Sprintf(
					"✅ <b>Free-прокси восстановлен</b>\n\nID: %d\n🌐 %s:%d\nRTT: %d мс",
					r.proxy.ID, r.proxy.IP, r.proxy.Port, r.rttMs,
				))
				log.Printf("HealthCheck: free proxy ID=%d %s:%d is UP (RTT=%dms)", r.proxy.ID, r.proxy.IP, r.proxy.Port, r.rttMs)
			}
		}
	}
	for _, r := range premiumResults {
		if msg := w.buildPremiumNotification(r); msg != "" {
			notifications = append(notifications, msg)
		}
	}
	w.prevMu.Unlock()

	// Шаг 3: отправляем уведомления — без мьютекса.
	for _, msg := range notifications {
		w.notifyAdmins(msg)
	}
}

// buildPremiumNotification обновляет prevStatus и возвращает текст уведомления при смене состояния.
// Вызывается только под w.prevMu — без сетевых вызовов.
func (w *HealthCheckWorker) buildPremiumNotification(r proxyCheckResult) string {
	wasUnreachable := w.prevStatus[r.proxy.ID]

	if !r.reachable {
		if !wasUnreachable {
			w.prevStatus[r.proxy.ID] = true
			log.Printf("HealthCheck: premium proxy ID=%d %s:%d is DOWN", r.proxy.ID, r.proxy.IP, r.proxy.Port)
			return fmt.Sprintf(
				"⚠️ <b>Premium-прокси недоступен</b>\n\nID: %d\n🌐 %s:%d\n\nПроверьте сервер.",
				r.proxy.ID, r.proxy.IP, r.proxy.Port,
			)
		}
	} else {
		if wasUnreachable {
			delete(w.prevStatus, r.proxy.ID)
			rttStr := "—"
			if r.proxy.LastRTTMs != nil {
				rttStr = fmt.Sprintf("%d мс", *r.proxy.LastRTTMs)
			}
			log.Printf("HealthCheck: premium proxy ID=%d %s:%d is UP", r.proxy.ID, r.proxy.IP, r.proxy.Port)
			return fmt.Sprintf(
				"✅ <b>Premium-прокси восстановлен</b>\n\nID: %d\n🌐 %s:%d\nRTT: %s",
				r.proxy.ID, r.proxy.IP, r.proxy.Port, rttStr,
			)
		}
	}
	return ""
}

// notifyAdmins отправляет уведомление каждому администратору.
// Для каждого вызова создаётся отдельный контекст с коротким таймаутом,
// не зависящий от продолжительности цикла проверки прокси.
func (w *HealthCheckWorker) notifyAdmins(text string) {
	for _, adminID := range w.adminIDs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := w.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    adminID,
			Text:      text,
			ParseMode: "HTML",
		})
		cancel()
		if err != nil {
			log.Printf("HealthCheck: notifyAdmins failed for admin %d: %v", adminID, err)
		}
	}
}
