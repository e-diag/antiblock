package worker

import (
	"log"
	"time"

	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// SubscriptionWorker проверяет истечение премиум подписок
type SubscriptionWorker struct {
	userUC usecase.UserUseCase
	config config.WorkerConfig
	stop   chan struct{}
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
	close(w.stop)
}

func (w *SubscriptionWorker) checkSubscriptions() {
	log.Println("Checking expired premium subscriptions...")
	if err := w.userUC.CheckExpiredPremiums(); err != nil {
		log.Printf("Error during subscription check: %v", err)
	} else {
		log.Println("Subscription check completed successfully")
	}

	// Дополнительно очищаем старые персональные премиум-прокси (старше 60 дней после окончания подписки)
	if err := w.userUC.CleanupExpiredProxies(60); err != nil {
		log.Printf("Error during proxy cleanup: %v", err)
	}
}
