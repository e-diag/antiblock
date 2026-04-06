package alert

import (
	"context"
	"strings"
	"testing"
)

func TestReport_FormatHTML(t *testing.T) {
	r := Report{
		Type:     "test_type",
		Source:   "unit/test",
		UserTGID: 42,
		Username: "user",
		Tariff:   "premium",
		ProxyID:  7,
		IP:       "1.2.3.4",
		Port:     443,
		Extra:    "fip=abc",
		ErrText:  "boom & <tag>",
	}
	out := r.FormatHTML()
	if !strings.Contains(out, "test_type") || !strings.Contains(out, "unit/test") {
		t.Fatalf("missing type/source: %q", out)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "&amp;") {
		t.Fatalf("error not escaped: %q", out)
	}
}

func TestTruncateRunes(t *testing.T) {
	s := strings.Repeat("а", 10) // 10 runes
	got := truncateRunes(s, 5)
	if len([]rune(got)) != 6 { // 5 + «…»
		t.Fatalf("got %q runes=%d", got, len([]rune(got)))
	}
}

func TestNilTelegramAlerterSend(t *testing.T) {
	var a *TelegramAlerter
	// не паникует
	a.Send(context.TODO(), Report{Type: "x", ErrText: "e"})
}
