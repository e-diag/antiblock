package worker

import (
	"context"
	"log"
	"time"

	"github.com/go-telegram/bot"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/usecase"
)

// YooKassaCleanupWorker отменяет и удаляет pending платежи ЮКассы старше часа.
type YooKassaCleanupWorker struct {
	bot       *bot.Bot
	paymentUC usecase.PaymentUseCase
	config    config.WorkerConfig
	stop      chan struct{}
}

func NewYooKassaCleanupWorker(b *bot.Bot, paymentUC usecase.PaymentUseCase, cfg config.WorkerConfig) *YooKassaCleanupWorker {
	return &YooKassaCleanupWorker{
		bot:       b,
		paymentUC: paymentUC,
		config:    cfg,
		stop:      make(chan struct{}),
	}
}

func (w *YooKassaCleanupWorker) Start() {
	if !w.config.Enabled {
		return
	}
	interval := w.config.Interval()
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.cleanup()
	for {
		select {
		case <-ticker.C:
			w.cleanup()
		case <-w.stop:
			return
		}
	}
}

func (w *YooKassaCleanupWorker) Stop() {
	select {
	case <-w.stop:
		return
	default:
		close(w.stop)
	}
}

const yooKassaPendingMaxAge = 1 * time.Hour

func (w *YooKassaCleanupWorker) cleanup() {
	if w.paymentUC == nil {
		return
	}
	cutoff := time.Now().Add(-yooKassaPendingMaxAge)
	invs, err := w.paymentUC.ListPendingYooKassaInvoicesOlderThan(cutoff)
	if err != nil || len(invs) == 0 {
		return
	}
	w.cleanupInvoices(invs)
}

func (w *YooKassaCleanupWorker) cleanupInvoices(invs []*domain.YooKassaInvoice) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, inv := range invs {
		if inv == nil || inv.PaymentID == "" {
			continue
		}

		// 1) Отменяем платёж в ЮКассе (если он ещё pending).
		if err := w.paymentUC.CancelYooKassaPayment(inv.PaymentID); err != nil {
			log.Printf("yookassa_cleanup: cancel payment %s error: %v", inv.PaymentID, err)
		}

		// 2) Удаляем сообщение с оплатой (если знаем).
		if w.bot != nil && inv.ChatID != 0 && inv.MessageID != 0 {
			_, _ = w.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{
				ChatID:    inv.ChatID,
				MessageID: int(inv.MessageID),
			})
		}

		// 3) Удаляем запись pending (оставляем только оплаченные в yookassa_payments).
		if err := w.paymentUC.DeleteYooKassaInvoice(inv.PaymentID); err != nil {
			log.Printf("yookassa_cleanup: delete invoice %s error: %v", inv.PaymentID, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

