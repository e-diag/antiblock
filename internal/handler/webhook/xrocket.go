package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/yourusername/antiblock/internal/usecase"
)

// XRocketWebhook обрабатывает webhook от xRocket Pay при успешной оплате счёта.
// apiToken — API-ключ приложения (Rocket-Pay-Key). Подпись верифицируется по SHA256(apiToken) согласно документации xRocket.
// getPremiumDays возвращает текущее число дней премиума из настроек (по умолчанию 30).
func XRocketWebhook(userUC usecase.UserUseCase, paymentUC usecase.PaymentUseCase, apiToken string, getPremiumDays func() int) http.HandlerFunc {
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

		// Проверка подписи: xRocket подписывает body как hex(HMAC-SHA256(body, SHA256(apiToken))).
		if apiToken != "" {
			sig := r.Header.Get("Rocket-Pay-Signature")
			if !verifyXRocketSignature(body, sig, apiToken) {
				log.Printf("[webhook] xRocket invalid signature")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		} else {
			log.Printf("[webhook] xRocket WARNING: API token empty, signature verification disabled")
		}

		// xRocket присылает type=invoicePay с data.id и data.status. Альтернатива: invoice.id.
		var payload struct {
			Type string `json:"type"`
			Invoice struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"invoice"`
			Data struct {
				ID     interface{} `json:"id"`     // может быть string или number
				Status string      `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[webhook] xRocket decode error: %v, body=%s", err, string(body))
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
			log.Printf("[webhook] xRocket missing invoice id, type=%q, body=%s", payload.Type, string(body))
			w.WriteHeader(http.StatusOK)
			return
		}
		if statusStr != "paid" {
			w.WriteHeader(http.StatusOK)
			return
		}

		invoiceID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			log.Printf("[webhook] xRocket invalid invoice id %q: %v, body=%s", idStr, err, string(body))
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

