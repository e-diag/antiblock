package botbuttons

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yourusername/antiblock/internal/telegramx"
)

func TestParseAndIcon(t *testing.T) {
	t.Parallel()
	const raw = `{"buttons":[{"key":"a","text":"Hi","callback_data":"x","paid_emoji_id":"12345"}]}`
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	b := Callback(c, "a", "Hi", "cb")
	if b.IconCustomEmojiID != "12345" {
		t.Fatalf("icon: %+v", b)
	}
}

func TestCallbackNilCatalog(t *testing.T) {
	t.Parallel()
	b := Callback(nil, "k", "T", "c")
	if b.Text != "T" || b.CallbackData != "c" || b.IconCustomEmojiID != "" {
		t.Fatalf("%+v", b)
	}
}

func TestInlineKeyboardJSONHasIconField(t *testing.T) {
	t.Parallel()
	m := &telegramx.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegramx.InlineKeyboardButton{
			{{Text: "L", IconCustomEmojiID: "999", CallbackData: "c"}},
		},
	}
	// smoke: must marshal icon_custom_emoji_id for Telegram API
	s := mustJSON(t, m)
	if !strings.Contains(s, "icon_custom_emoji_id") {
		t.Fatal(s)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
