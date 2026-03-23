package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

// InvoiceCleanupWorker удаляет pending-инвойсы (xRocket) и чистит сообщения с оплатой.
// Логика:
// - далее по interval: удаляет pending старше 1 часа.
type InvoiceCleanupWorker struct {
	bot        *bot.Bot
	invoiceRepo repository.InvoiceRepository
	paymentUC  usecase.PaymentUseCase
	config     config.WorkerConfig
	stop       chan struct{}
	stopOnce   sync.Once
}

func NewInvoiceCleanupWorker(b *bot.Bot, invoiceRepo repository.InvoiceRepository, paymentUC usecase.PaymentUseCase, cfg config.WorkerConfig) *InvoiceCleanupWorker {
	return &InvoiceCleanupWorker{
		bot:         b,
		invoiceRepo: invoiceRepo,
		paymentUC:   paymentUC,
		config:      cfg,
		stop:        make(chan struct{}),
	}
}

func (w *InvoiceCleanupWorker) Start() {
	if !w.config.Enabled {
		log.Println("Invoice cleanup worker is disabled")
		return
	}
	interval := w.config.Interval()
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	log.Printf("Starting invoice cleanup worker (interval: %v)", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.cleanup()
	for {
		select {
		case <-ticker.C:
			w.cleanup()
		case <-w.stop:
			log.Println("Invoice cleanup worker stopped")
			return
		}
	}
}

func (w *InvoiceCleanupWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

const invoicePendingMaxAge = 1 * time.Hour

func (w *InvoiceCleanupWorker) cleanup() {
	if w.invoiceRepo == nil || w.paymentUC == nil {
		return
	}
	cutoff := time.Now().Add(-invoicePendingMaxAge)
	invs, err := w.invoiceRepo.ListPendingOlderThan(cutoff)
	if err != nil || len(invs) == 0 {
		return
	}

	w.cleanupInvoices(invs)
}

func (w *InvoiceCleanupWorker) cleanupInvoices(invs []*domain.Invoice) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, inv := range invs {
		if inv == nil {
			continue
		}
		// 1) Отменяем в платёжке (если поддерживается).
		if err := w.paymentUC.CancelInvoice(inv.InvoiceID); err != nil {
			log.Printf("invoice_cleanup: cancel invoice %d error: %v", inv.InvoiceID, err)
		}

		// 2) Удаляем сообщение с инвойсом у пользователя (если сохраняли).
		if w.bot != nil && inv.ChatID != 0 && inv.MessageID != 0 {
			_, _ = w.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{
				ChatID:    inv.ChatID,
				MessageID: int(inv.MessageID),
			})
		}
		// 3) Удаляем pending-инвойс из БД (оставляем только оплаченные).
		if err := w.invoiceRepo.DeleteByInvoiceID(inv.InvoiceID); err != nil {
			log.Printf("invoice_cleanup: delete invoice %d error: %v", inv.InvoiceID, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

