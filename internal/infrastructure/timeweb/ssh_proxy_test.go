package timeweb

import "testing"

func TestValidateMtgUpstreamProxyURL(t *testing.T) {
	tests := []struct {
		raw string
		ok  bool
	}{
		{"socks5://127.0.0.1:1080", true},
		{"socks5h://proxy.example:1080", true},
		{"http://127.0.0.1:8080", true},
		{"https://user:pass@host:443", true},
		{"", false},
		{"ftp://127.0.0.1:1080", false},
		{"socks5://", false},
		{"socks5://\n127.0.0.1:1080", false},
	}
	for _, tc := range tests {
		err := ValidateMtgUpstreamProxyURL(tc.raw)
		if tc.ok && err != nil {
			t.Errorf("%q: want ok, got %v", tc.raw, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q: want error, got nil", tc.raw)
		}
	}
}

func TestPremiumMtgProxyShellFragment(t *testing.T) {
	s, err := premiumMtgProxyShellFragment("")
	if err != nil || s != " " {
		t.Fatalf("empty: got %q err=%v", s, err)
	}
	s, err = premiumMtgProxyShellFragment("  ")
	if err != nil || s != " " {
		t.Fatalf("whitespace: got %q err=%v", s, err)
	}
	s, err = premiumMtgProxyShellFragment("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if s != " --proxy="+shellQuote("socks5://127.0.0.1:1080")+" " {
		t.Fatalf("unexpected fragment: %q", s)
	}
}
