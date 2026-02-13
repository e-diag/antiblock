package worker

import (
	"log"
	"time"

	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// HealthCheckWorker выполняет проверку здоровья прокси-серверов
type HealthCheckWorker struct {
	proxyUC usecase.ProxyUseCase
	config  config.WorkerConfig
	stop    chan struct{}
}

// NewHealthCheckWorker создает новый worker для проверки здоровья
func NewHealthCheckWorker(proxyUC usecase.ProxyUseCase, cfg config.WorkerConfig) *HealthCheckWorker {
	return &HealthCheckWorker{
		proxyUC: proxyUC,
		config:  cfg,
		stop:    make(chan struct{}),
	}
}

// Start запускает worker
func (w *HealthCheckWorker) Start() {
	if !w.config.Enabled {
		log.Println("Health check worker is disabled")
		return
	}

	log.Printf("Starting health check worker (interval: %v)", w.config.Interval())

	ticker := time.NewTicker(w.config.Interval())
	defer ticker.Stop()

	// Выполняем первую проверку сразу
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

// Stop останавливает worker
func (w *HealthCheckWorker) Stop() {
	close(w.stop)
}

const healthCheckRetryDelay = 5 * time.Second

func (w *HealthCheckWorker) checkProxies() {
	for attempt := 0; attempt < 2; attempt++ {
		if err := w.proxyUC.CheckAllProxies(); err != nil {
			if attempt == 0 {
				time.Sleep(healthCheckRetryDelay)
				continue
			}
			log.Printf("Error during health check: %v", err)
		}
		break
	}
}
