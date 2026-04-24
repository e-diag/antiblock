package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/usecase"
)

type fakePaymentUC struct {
	started map[string]string
	inv     map[string]*domain.YooKassaInvoice
	getUser func(invoiceID int64) (int64, bool)
	getInv  func(invoiceID int64) (*domain.Invoice, error)
}

func (f *fakePaymentUC) CreateInvoice(float64, string, string, int64) (string, int64, error) { return "", 0, nil }
func (f *fakePaymentUC) GetUserIDByInvoiceID(invoiceID int64) (int64, bool) {
	if f.getUser != nil {
		return f.getUser(invoiceID)
	}
	return 0, false
}
func (f *fakePaymentUC) SetInvoiceMeta(int64, string, int) error                               { return nil }
func (f *fakePaymentUC) GetInvoice(invoiceID int64) (*domain.Invoice, error) {
	if f.getInv != nil {
		return f.getInv(invoiceID)
	}
	return nil, nil
}
func (f *fakePaymentUC) SetInvoiceMessage(int64, int64, int64) error                           { return nil }
func (f *fakePaymentUC) GetInvoiceMessageInfo(int64) (int64, int64, bool)                      { return 0, 0, false }
func (f *fakePaymentUC) MarkInvoicePaid(int64) error                                            { return nil }
func (f *fakePaymentUC) CancelInvoice(int64) error                                              { return nil }
func (f *fakePaymentUC) RecordStarPayment(int64, int64, string, int, string) error             { return nil }
func (f *fakePaymentUC) RecordYooKassaPayment(int64, string, int, int, string, string) error   { return nil }
func (f *fakePaymentUC) HasYooKassaPayment(string) (bool, error)                                { return false, nil }
func (f *fakePaymentUC) CreateYooKassaInvoice(*domain.YooKassaInvoice) error                    { return nil }
func (f *fakePaymentUC) GetYooKassaInvoice(paymentID string) (*domain.YooKassaInvoice, error) {
	return f.inv[paymentID], nil
}
func (f *fakePaymentUC) MarkYooKassaInvoicePaid(string) error                           { return nil }
func (f *fakePaymentUC) ListPendingYooKassaInvoicesOlderThan(time.Time) ([]*domain.YooKassaInvoice, error) {
	return nil, nil
}
func (f *fakePaymentUC) DeleteYooKassaInvoice(string) error              { return nil }
func (f *fakePaymentUC) CancelYooKassaPayment(string) error              { return nil }
func (f *fakePaymentUC) MarkPaymentEventSucceeded(provider, externalID string) error {
	f.started[provider+":"+externalID] = "succeeded"
	return nil
}
func (f *fakePaymentUC) MarkPaymentEventFailed(provider, externalID string) error {
	f.started[provider+":"+externalID] = "failed"
	return nil
}
func (f *fakePaymentUC) TryStartPaymentEvent(provider, externalID string) (bool, error) {
	key := provider + ":" + externalID
	if st, ok := f.started[key]; ok && st != "failed" {
		return false, nil
	}
	f.started[key] = "processing"
	return true, nil
}

func TestYooKassaWebhook_DuplicateSamePaymentSingleActivation(t *testing.T) {
	f := &fakePaymentUC{
		started: map[string]string{},
		inv: map[string]*domain.YooKassaInvoice{
			"p1": {PaymentID: "p1", TGID: 100, TariffType: "premium", DaysGranted: 30, AmountRub: 499, Status: "pending"},
		},
	}
	activated := 0
	h := YooKassaWebhook(func(tgID int64, days int) error { activated++; return nil }, nil, f, nil, "", "")
	body := `{"event":"payment.succeeded","object":{"id":"p1","status":"succeeded","paid":true,"amount":{"value":"499.00","currency":"RUB"},"metadata":{"tg_id":"100","days_granted":"30","tariff_type":"premium"}}}`

	req1 := httptest.NewRequest(http.MethodPost, "/webhook/yookassa", strings.NewReader(body))
	rr1 := httptest.NewRecorder()
	h(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rr1.Code)
	}
	if st := f.started["yookassa:p1"]; st != "succeeded" {
		t.Fatalf("expected succeeded state after first webhook, got %q", st)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/yookassa", strings.NewReader(body))
	rr2 := httptest.NewRecorder()
	h(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rr2.Code)
	}
	if activated != 1 {
		t.Fatalf("expected single activation, got %d", activated)
	}
}

func TestYooKassaWebhook_AmountMismatch_NoActivation(t *testing.T) {
	f := &fakePaymentUC{
		started: map[string]string{},
		inv: map[string]*domain.YooKassaInvoice{
			"p2": {PaymentID: "p2", TGID: 200, TariffType: "premium", DaysGranted: 30, AmountRub: 499, Status: "pending"},
		},
	}
	activated := 0
	h := YooKassaWebhook(func(tgID int64, days int) error { activated++; return nil }, nil, f, nil, "", "")
	body := `{"event":"payment.succeeded","object":{"id":"p2","status":"succeeded","paid":true,"amount":{"value":"1.00","currency":"RUB"},"metadata":{"tg_id":"200","days_granted":"30","tariff_type":"premium"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/yookassa", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rr.Code)
	}
	if activated != 0 {
		t.Fatalf("expected no activation on amount mismatch")
	}
}

func TestYooKassaWebhook_InvalidPayload_NoActivation(t *testing.T) {
	f := &fakePaymentUC{
		started: map[string]string{},
		inv:     map[string]*domain.YooKassaInvoice{},
	}
	activated := 0
	h := YooKassaWebhook(func(tgID int64, days int) error { activated++; return nil }, nil, f, nil, "", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/yookassa", strings.NewReader(`{"event":"payment.waiting_for_capture"}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rr.Code)
	}
	if activated != 0 {
		t.Fatalf("expected no activation")
	}
}

var _ usecase.PaymentUseCase = (*fakePaymentUC)(nil)
