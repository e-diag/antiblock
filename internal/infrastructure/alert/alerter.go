// Package alert — единая доставка технических инцидентов в служебный Telegram-чат (не личку менеджера).
package alert

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const maxErrRunes = 3000
const sendAttempts = 3

// Report описывает технический инцидент для служебного чата.
type Report struct {
	Type     string // короткий код, напр. premium_proxy_unreachable
	Source   string // handler/worker/usecase
	UserTGID int64
	Username string // @username без @, может быть пустым
	Tariff   string // free / premium / pro / пусто
	UserDBID uint   // внутренний id users.id, если известен
	ProxyID  uint
	IP       string
	Port     int
	Extra    string // FIP id, container, server id и т.п.
	ErrText  string
}

// LogLine — строка для стандартного лога (без HTML).
func (r Report) LogLine() string {
	var b strings.Builder
	b.WriteString(r.Type)
	if r.Source != "" {
		b.WriteString(" [")
		b.WriteString(r.Source)
		b.WriteString("]")
	}
	if r.UserTGID != 0 {
		fmt.Fprintf(&b, " tg=%d", r.UserTGID)
	}
	if r.ProxyID != 0 {
		fmt.Fprintf(&b, " proxy=%d", r.ProxyID)
	}
	if r.IP != "" || r.Port != 0 {
		fmt.Fprintf(&b, " %s:%d", r.IP, r.Port)
	}
	if r.ErrText != "" {
		b.WriteString(" err=")
		b.WriteString(truncateRunes(r.ErrText, 500))
	}
	return b.String()
}

// FormatHTML форматирует отчёт для Telegram (HTML).
func (r Report) FormatHTML() string {
	esc := html.EscapeString
	ts := time.Now().UTC().Format("2006-01-02 15:04:05 MST")

	var b strings.Builder
	b.WriteString("🚨 <b>")
	b.WriteString(esc("AntiBlock"))
	b.WriteString("</b>\n")
	b.WriteString("📛 <b>Тип:</b> <code>")
	b.WriteString(esc(r.Type))
	b.WriteString("</code>\n")
	if r.Source != "" {
		b.WriteString("📍 <b>Источник:</b> <code>")
		b.WriteString(esc(r.Source))
		b.WriteString("</code>\n")
	}
	b.WriteString("⏰ <b>Время:</b> ")
	b.WriteString(esc(ts))
	b.WriteString("\n")

	if r.UserTGID != 0 || r.Username != "" || r.Tariff != "" || r.UserDBID != 0 {
		b.WriteString("\n👤 <b>Пользователь</b>\n")
		if r.UserTGID != 0 {
			fmt.Fprintf(&b, "• tg_id: <code>%d</code>\n", r.UserTGID)
		}
		if r.Username != "" {
			b.WriteString("• username: @")
			b.WriteString(esc(strings.TrimPrefix(r.Username, "@")))
			b.WriteString("\n")
		}
		if r.Tariff != "" {
			b.WriteString("• тариф: <code>")
			b.WriteString(esc(r.Tariff))
			b.WriteString("</code>\n")
		}
		if r.UserDBID != 0 {
			fmt.Fprintf(&b, "• user_db_id: <code>%d</code>\n", r.UserDBID)
		}
	}

	if r.ProxyID != 0 || r.IP != "" || r.Port != 0 || r.Extra != "" {
		b.WriteString("\n🌐 <b>Прокси / ресурс</b>\n")
		if r.ProxyID != 0 {
			fmt.Fprintf(&b, "• proxy_id: <code>%d</code>\n", r.ProxyID)
		}
		if r.IP != "" || r.Port != 0 {
			fmt.Fprintf(&b, "• endpoint: <code>%s:%d</code>\n", esc(r.IP), r.Port)
		}
		if r.Extra != "" {
			b.WriteString("• extra: ")
			b.WriteString(esc(truncateRunes(r.Extra, 800)))
			b.WriteString("\n")
		}
	}

	if r.ErrText != "" {
		b.WriteString("\n❌ <b>Ошибка</b>\n<code>")
		b.WriteString(esc(truncateRunes(r.ErrText, maxErrRunes)))
		b.WriteString("</code>\n")
	}

	return b.String()
}

// TelegramAlerter отправляет Report в заданный chat_id (группа/канал с ботом).
type TelegramAlerter struct {
	bot    *bot.Bot
	chatID int64
}

// NewTelegramAlerter создаёт отправитель. chatID == 0 — только логирование, без Telegram.
func NewTelegramAlerter(b *bot.Bot, chatID int64) *TelegramAlerter {
	return &TelegramAlerter{bot: b, chatID: chatID}
}

// Send логирует всегда; в Telegram — если chat_id задан и бот не nil.
func (a *TelegramAlerter) Send(ctx context.Context, r Report) {
	log.Printf("[alert] %s", r.LogLine())
	if a == nil || a.bot == nil || a.chatID == 0 {
		return
	}
	text := r.FormatHTML()
	if utf8.RuneCountInString(text) > 4000 {
		text = truncateRunes(text, 4000) + "\n<i>(обрезано)</i>"
	}

	var lastErr error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		sendCtx := ctx
		if sendCtx == nil {
			sendCtx = context.Background()
		}
		cctx, cancel := context.WithTimeout(sendCtx, 12*time.Second)
		_, err := a.bot.SendMessage(cctx, &bot.SendMessageParams{
			ChatID:    a.chatID,
			Text:      text,
			ParseMode: models.ParseModeHTML,
		})
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		log.Printf("[alert] SendMessage attempt %d/%d failed: %v", attempt, sendAttempts, err)
		time.Sleep(time.Duration(attempt*400) * time.Millisecond)
	}
	if lastErr != nil {
		log.Printf("[alert] giving up after %d attempts: %v", sendAttempts, lastErr)
	}
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}
