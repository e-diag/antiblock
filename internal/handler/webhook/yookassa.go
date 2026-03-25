package webhook

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/usecase"
)

// YooKassaWebhook обрабатывает payment.succeeded для Smart Payment.
func YooKassaWebhook(
	activatePremium func(tgID int64, days int) error,
	activatePro func(tgID int64, days int) (*domain.ProGroup, bool, error),
	paymentUC usecase.PaymentUseCase,
	telegramBot *bot.Bot,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
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

		if payload.Event != "payment.succeeded" && payload.Object.Status != "succeeded" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.ToUpper(payload.Object.Amount.Currency) != "RUB" {
			w.WriteHeader(http.StatusOK)
			return
		}

		paymentID := payload.Object.ID
		already, err := paymentUC.HasYooKassaPayment(paymentID)
		if err != nil {
			log.Printf("[webhook] YooKassa idempotency check error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if already {
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
		if err := paymentUC.RecordYooKassaPayment(tgID, tariff, amountRub, days, "", paymentID); err != nil {
			log.Printf("[webhook] YooKassa RecordYooKassaPayment error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_ = paymentUC.MarkYooKassaInvoicePaid(paymentID)

		if tariff == "pro" {
			group, extendedOnly, err := activatePro(tgID, days)
			if err != nil {
				log.Printf("[webhook] YooKassa ActivatePro error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if telegramBot != nil {
				// Поведение как у xRocket webhook: при продлении (extendedOnly) — уведомление,
				// при новой группе — отправляем dd+ee прокси.
				if extendedOnly {
					_, _ = telegramBot.SendMessage(r.Context(), &bot.SendMessageParams{
						ChatID:    tgID,
						ParseMode: models.ParseModeHTML,
						Text:      fmt.Sprintf("✅ <b>Pro продлён</b> на %d дн.", days),
					})
				} else if group != nil {
					ddURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", group.ServerIP, group.PortDD, group.SecretDD)
					msgDD := fmt.Sprintf("✅ <b>Ваш Pro proxy готов!</b>\n\n🔐 <b>Тип: стандартный (dd)</b>\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>\n\nНажмите для подключения:",
						group.ServerIP, group.PortDD, group.SecretDD)
					kbDD := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "🔗 Подключиться (dd)", URL: ddURL}}}}
					_, _ = telegramBot.SendMessage(r.Context(), &bot.SendMessageParams{
						ChatID: tgID, Text: msgDD, ParseMode: models.ParseModeHTML, ReplyMarkup: kbDD,
					})

					eeURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", group.ServerIP, group.PortEE, group.SecretEE)
					msgEE := fmt.Sprintf("🛡 <b>Дополнительный proxy с маскировкой (ee/fake-TLS)</b>\n\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>\n\n<i>Запасной вариант для случаев, когда dd ограничен</i>",
						group.ServerIP, group.PortEE, group.SecretEE)
					kbEE := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "🔗 Подключиться (ee)", URL: eeURL}}}}
					_, _ = telegramBot.SendMessage(r.Context(), &bot.SendMessageParams{
						ChatID: tgID, Text: msgEE, ParseMode: models.ParseModeHTML, ReplyMarkup: kbEE,
					})
				}
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		if err := activatePremium(tgID, days); err != nil {
			log.Printf("[webhook] YooKassa ActivatePremium error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
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
