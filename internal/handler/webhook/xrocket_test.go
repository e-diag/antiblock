package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourusername/antiblock/internal/domain"
)

func xrSig(body, token string) string {
	hash := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, hash[:])
	_, _ = mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestXRocketWebhook_DuplicateRetry_NoDoubleActivation(t *testing.T) {
	f := &fakePaymentUC{
		started: map[string]string{},
		inv:     map[string]*domain.YooKassaInvoice{},
	}
	activateCount := 0
	token := "secret-token"
	h := XRocketWebhook(
		func(int64, int) error { return nil },
		func(tgID int64, days int) (*domain.ProGroup, bool, error) {
			activateCount++
			return &domain.ProGroup{ID: 1}, false, nil
		},
		f,
		token,
		func() int { return 30 },
		func() int { return 30 },
		nil,
		"",
	)
	body := `{"data":{"id":"123","status":"paid"}}`
	fakeInvoice := &domain.Invoice{InvoiceID: 123, UserID: 999, Kind: "pro", DaysGranted: 30, Status: "pending"}
	f.getInv = func(invoiceID int64) (*domain.Invoice, error) { return fakeInvoice, nil }
	f.getUser = func(invoiceID int64) (int64, bool) { return 999, true }

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhook/xrocket", strings.NewReader(body))
		req.Header.Set("Rocket-Pay-Signature", xrSig(body, token))
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", rr.Code)
		}
	}
	if activateCount != 1 {
		t.Fatalf("expected single activation, got %d", activateCount)
	}
}

func TestXRocketWebhook_PartialFailureThenRetry_SucceedsOnce(t *testing.T) {
	f := &fakePaymentUC{
		started: map[string]string{},
		inv:     map[string]*domain.YooKassaInvoice{},
	}
	token := "secret-token"
	attempts := 0
	h := XRocketWebhook(
		func(int64, int) error { return nil },
		func(tgID int64, days int) (*domain.ProGroup, bool, error) {
			attempts++
			if attempts == 1 {
				return nil, false, errors.New("temporary failure")
			}
			return &domain.ProGroup{ID: 2}, false, nil
		},
		f,
		token,
		func() int { return 30 },
		func() int { return 30 },
		nil,
		"",
	)
	body := `{"data":{"id":"124","status":"paid"}}`
	fakeInvoice := &domain.Invoice{InvoiceID: 124, UserID: 888, Kind: "pro", DaysGranted: 30, Status: "pending"}
	f.getInv = func(invoiceID int64) (*domain.Invoice, error) { return fakeInvoice, nil }
	f.getUser = func(invoiceID int64) (int64, bool) { return 888, true }

	req1 := httptest.NewRequest(http.MethodPost, "/webhook/xrocket", strings.NewReader(body))
	req1.Header.Set("Rocket-Pay-Signature", xrSig(body, token))
	rr1 := httptest.NewRecorder()
	h(rr1, req1)
	if rr1.Code != http.StatusInternalServerError {
		t.Fatalf("expected first attempt to fail, got %d", rr1.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/webhook/xrocket", strings.NewReader(body))
	req2.Header.Set("Rocket-Pay-Signature", xrSig(body, token))
	rr2 := httptest.NewRecorder()
	h(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected retry to succeed, got %d", rr2.Code)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}
