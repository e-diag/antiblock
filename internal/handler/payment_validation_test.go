package handler

import "testing"

func TestIsValidPreCheckout(t *testing.T) {
	h := &BotHandler{}

	if !h.isValidPreCheckout("premium", 30, "XTR", 100) {
		t.Fatalf("expected valid premium stars precheckout")
	}
	if h.isValidPreCheckout("premium", 30, "XTR", 101) {
		t.Fatalf("expected invalid amount for stars")
	}
	if !h.isValidPreCheckout("pro", 30, "RUB", 29900) {
		t.Fatalf("expected valid pro rub precheckout")
	}
	if h.isValidPreCheckout("pro", 30, "EUR", 29900) {
		t.Fatalf("expected invalid currency")
	}
}

func TestParsePaymentPayload(t *testing.T) {
	kind, days, userID, ok := parsePaymentPayload("premium_30_12345")
	if !ok || kind != "premium" || days != 30 || userID != 12345 {
		t.Fatalf("unexpected parse result: ok=%v kind=%s days=%d uid=%d", ok, kind, days, userID)
	}
	if _, _, _, ok := parsePaymentPayload("bad_payload"); ok {
		t.Fatalf("expected invalid payload")
	}
}
