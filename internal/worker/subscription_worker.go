package worker

import (
	"log"
	"sync"
	"time"

	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// SubscriptionWorker проверяет истечение премиум подписок
type SubscriptionWorker struct {
	userUC usecase.UserUseCase
	config config.WorkerConfig
	stop   chan struct{}
	stopOnce sync.Once
}

// NewSubscriptionWorker создает новый worker для проверки подписок
func NewSubscriptionWorker(userUC usecase.UserUseCase, cfg config.WorkerConfig) *SubscriptionWorker {
	return &SubscriptionWorker{
		userUC: userUC,
		config: cfg,
		stop:   make(chan struct{}),
	}
}

// Start запускает worker
func (w *SubscriptionWorker) Start() {
	if !w.config.Enabled {
		log.Println("Subscription checker worker is disabled")
		return
	}

	log.Printf("Starting subscription checker worker (interval: %v)", w.config.Interval())

	ticker := time.NewTicker(w.config.Interval())
	defer ticker.Stop()

	// Выполняем первую проверку сразу
	w.checkSubscriptions()

	for {
		select {
		case <-ticker.C:
			w.checkSubscriptions()
		case <-w.stop:
			log.Println("Subscription checker worker stopped")
			return
		}
	}
}

// Stop останавливает worker
func (w *SubscriptionWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

const workerRetryDelay = 5 * time.Second

func (w *SubscriptionWorker) checkSubscriptions() {
	for attempt := 0; attempt < 2; attempt++ {
		if err := w.userUC.CheckExpiredPremiums(); err != nil {
			if attempt == 0 {
				time.Sleep(workerRetryDelay)
				continue
			}
			log.Printf("Error during subscription check: %v", err)
		}
		break
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := w.userUC.CleanupExpiredProxies(60); err != nil {
			if attempt == 0 {
				time.Sleep(workerRetryDelay)
				continue
			}
			log.Printf("Error during proxy cleanup: %v", err)
		}
		break
	}
}
