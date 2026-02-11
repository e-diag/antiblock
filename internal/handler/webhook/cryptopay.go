package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/yourusername/antiblock/internal/usecase"
)

// CryptoPayWebhook обрабатывает callback от CryptoPay при оплате счёта.
// getPremiumDays возвращает текущее число дней премиума из настроек (по умолчанию 30).
func CryptoPayWebhook(userUC usecase.UserUseCase, paymentUC usecase.PaymentUseCase, secret string, getPremiumDays func() int) http.HandlerFunc {
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

		if secret == "" {
			log.Printf("[webhook] cryptopay WARNING: webhook secret is empty, signature verification disabled")
		} else {
			sig := r.Header.Get("crypto-pay-api-signature")
			if sig == "" {
				sig = r.Header.Get("X-Api-Signature")
			}
			if !verifyCryptoPaySignature(body, sig, secret) {
				log.Printf("[webhook] cryptopay invalid signature")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		// Crypto Pay присылает { "update_type": "invoice_paid", "payload": { "invoice_id", "status", ... } }
		var update struct {
			UpdateType string `json:"update_type"`
			Payload    struct {
				InvoiceID int64  `json:"invoice_id"`
				Status    string `json:"status"`
			} `json:"payload"`
			// fallback: если тело сразу invoice_id/status (старый формат)
			InvoiceID int64  `json:"invoice_id"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(body, &update); err != nil {
			log.Printf("[webhook] cryptopay decode error: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Различаем формат по наличию update_type (новый формат: update_type + payload)
		var invoiceID int64
		var status string
		if update.UpdateType == "invoice_paid" && update.Payload.InvoiceID != 0 {
			invoiceID = update.Payload.InvoiceID
			status = update.Payload.Status
		} else {
			invoiceID = update.InvoiceID
			status = update.Status
		}

		if status != "paid" {
			w.WriteHeader(http.StatusOK)
			return
		}

		userID, ok := paymentUC.GetUserIDByInvoiceID(invoiceID)
		if !ok {
			log.Printf("[webhook] cryptopay unknown invoice_id: %d", invoiceID)
			w.WriteHeader(http.StatusOK)
			return
		}

		premiumDays := getPremiumDays()
		if premiumDays < 1 {
			premiumDays = 30
		}
		if err := userUC.ActivatePremium(userID, premiumDays); err != nil {
			log.Printf("[webhook] cryptopay ActivatePremium error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := paymentUC.MarkInvoicePaid(invoiceID); err != nil {
			log.Printf("[webhook] cryptopay MarkInvoicePaid error: %v", err)
		}

		log.Printf("[webhook] cryptopay premium activated for user %d (invoice %d)", userID, invoiceID)
		w.WriteHeader(http.StatusOK)
	}
}

// verifyCryptoPaySignature проверяет подпись тела запроса CryptoPay.
// CryptoPay присылает X-Api-Signature как hex(HMAC-SHA256(body, secret)).
func verifyCryptoPaySignature(body []byte, signatureHeader, secret string) bool {
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
