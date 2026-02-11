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
// Для защиты от подделки запросов проверяется подпись в заголовке X-Api-Signature.
func CryptoPayWebhook(userUC usecase.UserUseCase, paymentUC usecase.PaymentUseCase, secret string) http.HandlerFunc {
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
			sig := r.Header.Get("X-Api-Signature")
			if !verifyCryptoPaySignature(body, sig, secret) {
				log.Printf("[webhook] cryptopay invalid signature")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		var payload struct {
			InvoiceID int64  `json:"invoice_id"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[webhook] cryptopay decode error: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if payload.Status != "paid" {
			w.WriteHeader(http.StatusOK)
			return
		}

		userID, ok := paymentUC.GetUserIDByInvoiceID(payload.InvoiceID)
		if !ok {
			log.Printf("[webhook] cryptopay unknown invoice_id: %d", payload.InvoiceID)
			w.WriteHeader(http.StatusOK)
			return
		}

		const premiumDays = 30
		if err := userUC.ActivatePremium(userID, premiumDays); err != nil {
			log.Printf("[webhook] cryptopay ActivatePremium error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := paymentUC.MarkInvoicePaid(payload.InvoiceID); err != nil {
			log.Printf("[webhook] cryptopay MarkInvoicePaid error: %v", err)
		}

		log.Printf("[webhook] cryptopay premium activated for user %d (invoice %d)", userID, payload.InvoiceID)
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
