package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/botmessage"
	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/usecase"
)

// YooKassaWebhook обрабатывает payment.succeeded для Smart Payment.
func YooKassaWebhook(
	activatePremium func(tgID int64, days int) error,
	activatePro func(tgID int64, days int) (*domain.ProGroup, bool, error),
	paymentUC usecase.PaymentUseCase,
	telegramBot *bot.Bot,
	webhookToken string,
	proServerIP string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		const maxWebhookBodyBytes = 1 << 20 // 1 MiB
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
		if strings.TrimSpace(webhookToken) != "" {
			if strings.TrimSpace(r.Header.Get("X-AntiBlock-Webhook-Token")) != strings.TrimSpace(webhookToken) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		var payload struct {
			Event  string `json:"event"`
			Object struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Paid   bool   `json:"paid"`
				Amount struct {
					Value    string `json:"value"`
					Currency string `json:"currency"`
				} `json:"amount"`
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if payload.Event != "payment.succeeded" || payload.Object.Status != "succeeded" || !payload.Object.Paid {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.ToUpper(payload.Object.Amount.Currency) != "RUB" {
			w.WriteHeader(http.StatusOK)
			return
		}

		paymentID := payload.Object.ID
		expected, err := paymentUC.GetYooKassaInvoice(paymentID)
		if err != nil {
			log.Printf("[webhook] YooKassa load invoice error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if expected == nil {
			log.Printf("[webhook] YooKassa unknown payment_id=%q", paymentID)
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.TrimSpace(expected.Status) == "paid" || strings.TrimSpace(expected.Status) == "cancelled" {
			w.WriteHeader(http.StatusOK)
			return
		}

		md := payload.Object.Metadata
		tgID, errTG := strconv.ParseInt(md["tg_id"], 10, 64)
		days, errDays := strconv.Atoi(md["days_granted"])
		tariff := strings.ToLower(strings.TrimSpace(md["tariff_type"]))
		if errTG != nil || errDays != nil || days < 1 || (tariff != "pro" && tariff != "premium") {
			log.Printf("[webhook] YooKassa invalid metadata: %+v", md)
			w.WriteHeader(http.StatusOK)
			return
		}

		amountRub := parseRubToInt(payload.Object.Amount.Value)
		if expected.TGID != tgID || expected.DaysGranted != days || strings.ToLower(expected.TariffType) != tariff || expected.AmountRub != amountRub {
			log.Printf("[webhook] YooKassa mismatch invoice/payment payment_id=%s", paymentID)
			w.WriteHeader(http.StatusOK)
			return
		}
		var proGroup *domain.ProGroup
		var extendedOnly bool
		orchestrator := usecase.NewPaymentOrchestrator(
			paymentUC,
			activatePremium,
			func(tgID int64, days int) error {
				group, ext, err := activatePro(tgID, days)
				if err != nil {
					return err
				}
				proGroup = group
				extendedOnly = ext
				return nil
			},
			func(in usecase.PaymentEventInput) error {
				return paymentUC.RecordYooKassaPayment(in.TGID, in.Tariff, in.AmountRub, in.Days, "", in.ExternalID)
			},
			func(in usecase.PaymentEventInput) error {
				return paymentUC.MarkYooKassaInvoicePaid(in.ExternalID)
			},
		)
		res, err := orchestrator.ProcessPaidEvent(usecase.PaymentEventInput{
			Provider:   "yookassa",
			ExternalID: paymentID,
			TGID:       tgID,
			Tariff:     tariff,
			Days:       days,
			AmountRub:  amountRub,
			Currency:   "RUB",
		})
		if err != nil {
			log.Printf("[webhook] YooKassa orchestration error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if res == nil || res.Status == usecase.PaymentAlreadyProcessed || res.Status == usecase.PaymentValidationFailed {
			w.WriteHeader(http.StatusOK)
			return
		}

		if tariff == "pro" {
			if telegramBot != nil {
				// Поведение как у xRocket webhook: при продлении (extendedOnly) — уведомление,
				// при новой группе — два ee-прокси (nineseconds).
				if extendedOnly {
					_, _ = telegramBot.SendMessage(r.Context(), &bot.SendMessageParams{
						ChatID:    tgID,
						ParseMode: models.ParseModeHTML,
						Text:      fmt.Sprintf("✅ <b>Pro продлён</b> на %d дн.", days),
					})
				} else if proGroup != nil {
					tgCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
					botmessage.SendProGroupTwoEEWithServerIP(tgCtx, telegramBot, tgID, proGroup, botmessage.ProGroupStylePayment, strings.TrimSpace(proServerIP))
					cancel()
				}
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		if telegramBot != nil {
			_, _ = telegramBot.SendMessage(r.Context(), &bot.SendMessageParams{
				ChatID:    tgID,
				ParseMode: models.ParseModeHTML,
				Text:      fmt.Sprintf("✅ Оплата получена! Премиум на %d дн. активирован.", days),
			})
		}
		w.WriteHeader(http.StatusOK)
	}
}

func parseRubToInt(v string) int {
	v = strings.TrimSpace(strings.ReplaceAll(v, ",", "."))
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return int(f + 0.000001)
}
