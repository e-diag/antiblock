package botbuttons

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/yourusername/antiblock/internal/telegramx"
)

// Catalog — кнопки по key из assets/bot_buttons.json.
type Catalog struct {
	byKey map[string]ButtonDef
}

// ButtonDef одна запись из JSON.
type ButtonDef struct {
	Key               string `json:"key"`
	Text              string `json:"text"`
	CallbackData      string `json:"callback_data"`
	URL               string `json:"url"`
	Screen            string `json:"screen"`
	PaidEmojiID       string `json:"paid_emoji_id"`
	IconCustomEmojiID string `json:"icon_custom_emoji_id"`
}

// IconID — Telegram custom emoji id для поля icon_custom_emoji_id (алиас paid_emoji_id).
func (b *ButtonDef) IconID() string {
	if s := strings.TrimSpace(b.IconCustomEmojiID); s != "" {
		return s
	}
	return strings.TrimSpace(b.PaidEmojiID)
}

type fileRoot struct {
	Buttons []ButtonDef `json:"buttons"`
}

// Load читает JSON с диска.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse разбирает JSON (для тестов).
func Parse(data []byte) (*Catalog, error) {
	var root fileRoot
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	c := &Catalog{byKey: make(map[string]ButtonDef, len(root.Buttons))}
	for _, b := range root.Buttons {
		if b.Key != "" {
			c.byKey[b.Key] = b
		}
	}
	return c, nil
}

// Text возвращает поле text из каталога или пустую строку.
func (c *Catalog) Text(key string) string {
	if c == nil {
		return ""
	}
	if def, ok := c.byKey[key]; ok {
		return def.Text
	}
	return ""
}

// Def возвращает полное описание кнопки по ключу.
func (c *Catalog) Def(key string) (ButtonDef, bool) {
	if c == nil {
		return ButtonDef{}, false
	}
	def, ok := c.byKey[key]
	return def, ok
}

// Format форматирует text кнопки как fmt.Sprintf, если в каталоге есть шаблон.
func (c *Catalog) Format(key string, args ...any) string {
	t := c.Text(key)
	if t == "" {
		return ""
	}
	return fmt.Sprintf(t, args...)
}

// InlineCallback — callback-кнопка; text задаётся в коде (динамика). Иконка из каталога по key, если задана.
func (c *Catalog) InlineCallback(key, text, callbackData string) telegramx.InlineKeyboardButton {
	b := telegramx.InlineKeyboardButton{Text: text, CallbackData: callbackData}
	if c != nil {
		if def, ok := c.byKey[key]; ok {
			if id := def.IconID(); id != "" {
				b.IconCustomEmojiID = id
			}
		}
	}
	return b
}

// InlineURL — кнопка-ссылка с опциональной иконкой из каталога.
func (c *Catalog) InlineURL(key, text, url string) telegramx.InlineKeyboardButton {
	b := telegramx.InlineKeyboardButton{Text: text, URL: url}
	if c != nil {
		if def, ok := c.byKey[key]; ok {
			if id := def.IconID(); id != "" {
				b.IconCustomEmojiID = id
			}
		}
	}
	return b
}

// Callback — безопасно при nil-каталоге (для вызова не через метод на nil-указателе).
func Callback(c *Catalog, key, text, callbackData string) telegramx.InlineKeyboardButton {
	if c == nil {
		return telegramx.InlineKeyboardButton{Text: text, CallbackData: callbackData}
	}
	return c.InlineCallback(key, text, callbackData)
}

// URLButton — безопасно при nil-каталоге.
func URLButton(c *Catalog, key, text, url string) telegramx.InlineKeyboardButton {
	if c == nil {
		return telegramx.InlineKeyboardButton{Text: text, URL: url}
	}
	return c.InlineURL(key, text, url)
}
