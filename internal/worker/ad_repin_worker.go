package worker

import (
	"context"
	"log"
	"time"

	"github.com/go-telegram/bot"

	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/repository"
)

const adRePinDelayMs = 50

// AdRePinWorker раз в час повторно закрепляет активное объявление у всех пользователей из ad_pins
// (если пользователь открепил — сообщение снова будет закреплено до истечения срока объявления).
type AdRePinWorker struct {
	bot      *bot.Bot
	adRepo   repository.AdRepository
	adPinRepo repository.AdPinRepository
	config   config.WorkerConfig
	stop     chan struct{}
}

// NewAdRePinWorker создаёт воркер повторного закрепления объявлений.
func NewAdRePinWorker(
	b *bot.Bot,
	adRepo repository.AdRepository,
	adPinRepo repository.AdPinRepository,
	cfg config.WorkerConfig,
) *AdRePinWorker {
	return &AdRePinWorker{
		bot:       b,
		adRepo:    adRepo,
		adPinRepo: adPinRepo,
		config:    cfg,
		stop:      make(chan struct{}),
	}
}

// Start запускает воркер.
func (w *AdRePinWorker) Start() {
	if !w.config.Enabled {
		log.Println("Ad repin worker is disabled")
		return
	}
	log.Printf("Starting ad repin worker (interval: %v)", w.config.Interval())
	ticker := time.NewTicker(w.config.Interval())
	defer ticker.Stop()
	w.repin()
	for {
		select {
		case <-ticker.C:
			w.repin()
		case <-w.stop:
			log.Println("Ad repin worker stopped")
			return
		}
	}
}

// Stop останавливает воркер.
func (w *AdRePinWorker) Stop() {
	close(w.stop)
}

func (w *AdRePinWorker) repin() {
	ad, err := w.adRepo.GetActiveOne()
	if err != nil || ad == nil {
		return
	}
	if ad.ExpiresAt != nil && ad.ExpiresAt.Before(time.Now()) {
		return
	}
	pins, err := w.adPinRepo.ListByAdID(ad.ID)
	if err != nil || len(pins) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, pin := range pins {
		_, _ = w.bot.PinChatMessage(ctx, &bot.PinChatMessageParams{
			ChatID:    pin.ChatID,
			MessageID: pin.MessageID,
		})
		time.Sleep(time.Duration(adRePinDelayMs) * time.Millisecond)
	}
}
