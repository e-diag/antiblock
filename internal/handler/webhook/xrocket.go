package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/yourusername/antiblock/internal/usecase"
)

// XRocketWebhook обрабатывает webhook от xRocket Pay при успешной оплате счёта.
// Подписка выдаётся на getPremiumDays() дней (настройка premium_days, по умолчанию 30).
// При повторной оплате (любым способом — xRocket или Stars) ActivatePremium добавляет +N дней к текущей дате окончания.
// getPremiumDays возвращает текущее число дней премиума из настроек (по умолчанию 30).
func XRocketWebhook(userUC usecase.UserUseCase, paymentUC usecase.PaymentUseCase, secret string, getPremiumDays func() int) http.HandlerFunc {
	if getPremiumDays == nil {
		getPremiumDays = func() int { return 30 }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		// Проверка подписи webhook xRocket (Rocket-Pay-Signature: hex(HMAC-SHA256(body, sha256(appToken)))).
		if secret != "" {
			sig := r.Header.Get("Rocket-Pay-Signature")
			if !verifyXRocketSignature(body, sig, secret) {
				log.Printf("[webhook] xRocket invalid signature")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		} else {
			log.Printf("[webhook] xRocket WARNING: webhook secret is empty, signature verification disabled")
		}

		// Модель webhook описана в xRocket Pay API как WebhookDto / Invoice.
		// Используем минимальный набор полей: id счёта (строка) и статус.
		var update struct {
			Invoice struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"invoice"`
		}
		if err := json.Unmarshal(body, &update); err != nil {
			log.Printf("[webhook] xRocket decode error: %v, body=%s", err, string(body))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if update.Invoice.ID == "" {
			log.Printf("[webhook] xRocket missing invoice id, body=%s", string(body))
			w.WriteHeader(http.StatusOK)
			return
		}
		if update.Invoice.Status != "paid" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// invoice.id приходит строкой — приводим к int64, как в таблице invoices.
		invoiceID, err := strconv.ParseInt(update.Invoice.ID, 10, 64)
		if err != nil {
			log.Printf("[webhook] xRocket invalid invoice id %q: %v, body=%s", update.Invoice.ID, err, string(body))
			w.WriteHeader(http.StatusOK)
			return
		}

		userID, ok := paymentUC.GetUserIDByInvoiceID(invoiceID)
		if !ok {
			log.Printf("[webhook] xRocket unknown invoice_id: %d", invoiceID)
			w.WriteHeader(http.StatusOK)
			return
		}

		premiumDays := getPremiumDays()
		if premiumDays < 1 {
			premiumDays = 30
		}
		// ActivatePremium продлевает подписку на premiumDays; если уже есть активный премиум — добавляет +premiumDays к дате окончания.
		if err := userUC.ActivatePremium(userID, premiumDays); err != nil {
			if err == usecase.ErrPremiumProxyCreationFailed {
				_ = paymentUC.MarkInvoicePaid(invoiceID)
				log.Printf("[webhook] xRocket premium activated for user %d (invoice %d), but proxy creation failed — notify manager manually", userID, invoiceID)
				w.WriteHeader(http.StatusOK)
				return
			}
			log.Printf("[webhook] xRocket ActivatePremium error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := paymentUC.MarkInvoicePaid(invoiceID); err != nil {
			log.Printf("[webhook] xRocket MarkInvoicePaid error: %v", err)
		}

		log.Printf("[webhook] xRocket premium activated for user %d (invoice %d)", userID, invoiceID)
		w.WriteHeader(http.StatusOK)
	}
}

// verifyXRocketSignature проверяет подпись тела запроса xRocket.
// xRocket присылает Rocket-Pay-Signature как hex(HMAC-SHA256(body, secret)).
func verifyXRocketSignature(body []byte, signatureHeader, secret string) bool {
	if signatureHeader == "" || secret == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(signatureHeader)
	if err != nil {
		return false
	}

	return hmac.Equal(expected, got)
}

