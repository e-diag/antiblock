package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// XRocketWebhook обрабатывает webhook от xRocket Pay при успешной оплате счёта.
// В зависимости от payment invoice.Kind выдаёт premium или pro.
func XRocketWebhook(
	activatePremium func(tgID int64, days int) error,
	activatePro func(tgID int64, days int) (*domain.ProGroup, bool, error),
	paymentUC usecase.PaymentUseCase,
	apiToken string,
	getPremiumDays func() int,
	getProDays func() int,
	telegramBot *bot.Bot,
) http.HandlerFunc {
	if getPremiumDays == nil {
		getPremiumDays = func() int { return 30 }
	}
	if getProDays == nil {
		getProDays = func() int { return 30 }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		const maxWebhookBodyBytes = 1 << 20 // 1 MiB safety limit against DoS
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		// Проверка подписи: xRocket подписывает body как hex(HMAC-SHA256(body, SHA256(apiToken))).
		if apiToken == "" {
			log.Printf("[webhook] xRocket FATAL: XROCKET_API_TOKEN not configured — all requests rejected")
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		sig := r.Header.Get("Rocket-Pay-Signature")
		if !verifyXRocketSignature(body, sig, apiToken) {
			log.Printf("[webhook] xRocket invalid signature")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// xRocket присылает type=invoicePay с data.id и data.status. Альтернатива: invoice.id.
		var payload struct {
			Type    string `json:"type"`
			Invoice struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"invoice"`
			Data struct {
				ID     interface{} `json:"id"` // может быть string или number
				Status string      `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[webhook] xRocket decode error: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var idStr, statusStr string
		if payload.Data.ID != nil {
			switch v := payload.Data.ID.(type) {
			case string:
				idStr = v
			case float64:
				idStr = strconv.FormatInt(int64(v), 10)
			default:
				idStr = fmt.Sprintf("%v", v)
			}
			statusStr = payload.Data.Status
		}
		if idStr == "" && payload.Invoice.ID != "" {
			idStr, statusStr = payload.Invoice.ID, payload.Invoice.Status
		}
		if idStr == "" {
			log.Printf("[webhook] xRocket missing invoice id, type=%q", payload.Type)
			w.WriteHeader(http.StatusOK)
			return
		}
		if statusStr != "paid" {
			w.WriteHeader(http.StatusOK)
			return
		}

		invoiceID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			log.Printf("[webhook] xRocket invalid invoice id %q: %v", idStr, err)
			w.WriteHeader(http.StatusOK)
			return
		}

		userID, ok := paymentUC.GetUserIDByInvoiceID(invoiceID)
		if !ok {
			log.Printf("[webhook] xRocket unknown invoice_id: %d", invoiceID)
			w.WriteHeader(http.StatusOK)
			return
		}

		inv, _ := paymentUC.GetInvoice(invoiceID)
		kind := "premium"
		days := 0
		if inv != nil {
			if inv.Kind != "" {
				kind = strings.ToLower(strings.TrimSpace(inv.Kind))
			}
			if inv.DaysGranted > 0 {
				days = inv.DaysGranted
			}
		}
		if days <= 0 {
			if kind == "pro" {
				days = getProDays()
			} else {
				days = getPremiumDays()
			}
			if days < 1 {
				days = 30
			}
		}

		if kind == "pro" {
			if activatePro == nil {
				log.Printf("[webhook] xRocket pro activator is nil")
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			group, extendedOnly, err := activatePro(userID, days)
			if err != nil {
				log.Printf("[webhook] xRocket ActivatePro error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			_ = paymentUC.MarkInvoicePaid(invoiceID)

			if telegramBot != nil && group != nil {
				if chatID, msgID, ok := paymentUC.GetInvoiceMessageInfo(invoiceID); ok && chatID != 0 && msgID != 0 {
					tgCtx, tgCancel := context.WithTimeout(r.Context(), 5*time.Second)
					_, _ = telegramBot.DeleteMessage(tgCtx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: int(msgID)})
					tgCancel()
				}
				if extendedOnly {
					cycle := getProDays()
					if cycle < 1 {
						cycle = 30
					}
					tgCtx, tgCancel := context.WithTimeout(r.Context(), 5*time.Second)
					_, _ = telegramBot.SendMessage(tgCtx, &bot.SendMessageParams{
						ChatID: userID, ParseMode: models.ParseModeHTML,
						Text: fmt.Sprintf("✅ <b>Pro продлён</b> на %d дн.\n\nТекущие прокси не меняются. Раз в <b>%d</b> дн. ключи обновляются — новые данные придут в этот чат.", days, cycle),
					})
					tgCancel()
				} else {
					tgCtx, tgCancel := context.WithTimeout(r.Context(), 15*time.Second)
					botmessage.SendProGroupTwoEE(tgCtx, telegramBot, userID, group, botmessage.ProGroupStylePayment)
					tgCancel()
				}
			}

			log.Printf("[webhook] xRocket pro activated for user %d (invoice %d)", userID, invoiceID)
			w.WriteHeader(http.StatusOK)
			return
		}

		// premium
		if activatePremium == nil {
			log.Printf("[webhook] xRocket premium activator is nil")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := activatePremium(userID, days); err != nil {
			log.Printf("[webhook] xRocket ActivatePremium error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := paymentUC.MarkInvoicePaid(invoiceID); err != nil {
			log.Printf("[webhook] xRocket MarkInvoicePaid error: %v", err)
		}

		// Уведомление пользователю и удаление сообщения с инвойсом
		if telegramBot != nil {
			confirmMsg := "✅ Оплата получена! Ваш Premium прокси будет готов в течение нескольких минут — мы уведомим вас."
			if chatID, msgID, ok := paymentUC.GetInvoiceMessageInfo(invoiceID); ok && chatID != 0 && msgID != 0 {
				tgCtx, tgCancel := context.WithTimeout(r.Context(), 5*time.Second)
				_, _ = telegramBot.DeleteMessage(tgCtx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: int(msgID)})
				tgCancel()
			}
			tgCtx, tgCancel := context.WithTimeout(r.Context(), 5*time.Second)
			_, _ = telegramBot.SendMessage(tgCtx, &bot.SendMessageParams{
				ChatID:    userID,
				Text:      confirmMsg,
				ParseMode: models.ParseModeHTML,
			})
			tgCancel()
		}

		log.Printf("[webhook] xRocket premium activated for user %d (invoice %d)", userID, invoiceID)
		w.WriteHeader(http.StatusOK)
	}
}

// verifyXRocketSignature проверяет подпись тела запроса xRocket.
// Документация: Rocket-Pay-Signature = hex(HMAC-SHA256(body, SHA256(apiToken))).
func verifyXRocketSignature(body []byte, signatureHeader, apiToken string) bool {
	if signatureHeader == "" || apiToken == "" {
		return false
	}

	// Ключ HMAC = SHA256(apiToken) (сырые байты)
	hash := sha256.Sum256([]byte(apiToken))
	mac := hmac.New(sha256.New, hash[:])
	mac.Write(body)
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(signatureHeader)
	if err != nil {
		return false
	}

	return hmac.Equal(expected, got)
}
