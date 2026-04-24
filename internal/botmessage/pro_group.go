// Package botmessage — единые шаблоны сообщений бота (без handler-состояния).
package botmessage

import (
	"context"
	"fmt"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
)

// ProGroupStyle задаёт префикс заголовка первого сообщения (бот vs вебхук оплаты).
type ProGroupStyle int

const (
	// ProGroupStyleBot — как в главном меню бота (⚡).
	ProGroupStyleBot ProGroupStyle = iota
	// ProGroupStylePayment — после успешной оплаты (✅).
	ProGroupStylePayment
)

// SendProGroupTwoEE отправляет два ee-прокси Pro-группы (nineseconds). Поля PortDD/SecretDD — первый ee-слот.
func SendProGroupTwoEE(ctx context.Context, b *bot.Bot, tgID int64, group *domain.ProGroup, style ProGroupStyle) {
	SendProGroupTwoEEWithServerIP(ctx, b, tgID, group, style, "")
}

// SendProGroupTwoEEWithServerIP отправляет два ee-прокси Pro-группы с optional override IP.
// Если overrideServerIP пустой — используется group.ServerIP.
func SendProGroupTwoEEWithServerIP(ctx context.Context, b *bot.Bot, tgID int64, group *domain.ProGroup, style ProGroupStyle, overrideServerIP string) {
	if b == nil || group == nil {
		return
	}
	prefix := "⚡"
	if style == ProGroupStylePayment {
		prefix = "✅"
	}
	clientIP := group.ServerIP
	if v := overrideServerIP; v != "" {
		clientIP = v
	}
	sendEE := func(title string, port int, secret string, withHint bool) {
		if port <= 0 || secret == "" {
			return
		}
		u := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", clientIP, port, secret)
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "🔗 Подключиться (ee)", URL: u}},
			},
		}
		hint := ""
		if withHint {
			hint = "\n\n<i>Второй вариант — в следующем сообщении.</i>"
		}
		msg := fmt.Sprintf(
			"%s <b>%s</b>\n\n🔐 <b>ee / fake-TLS</b>\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>%s",
			prefix, title, clientIP, port, secret, hint,
		)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: tgID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
		})
	}
	sendEE("Ваш Pro proxy (1/2)", group.PortDD, group.SecretDD, group.SecretEE != "")
	sendEE("Ваш Pro proxy (2/2)", group.PortEE, group.SecretEE, false)
}
