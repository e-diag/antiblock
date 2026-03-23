package middleware

import (
	"context"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const MaintenanceUserMessage = "Ведутся технические работы, мы сообщим, как бот будет доступен."

// MaintenanceSettings минимальный интерфейс настроек для режима техработ.
type MaintenanceSettings interface {
	Get(key string) (string, error)
}

// MaintenanceWaitQueue добавление tg_id в очередь уведомлений после техработ.
type MaintenanceWaitQueue interface {
	AddTGID(tgID int64) error
}

// MaintenanceMiddleware блокирует всех, кроме админов, пока maintenance_mode включён.
func MaintenanceMiddleware(settings MaintenanceSettings, wait MaintenanceWaitQueue, adminIDs []int64) func(bot.HandlerFunc) bot.HandlerFunc {
	adm := make(map[int64]bool)
	for _, id := range adminIDs {
		adm[id] = true
	}
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			on, err := settings.Get("maintenance_mode")
			if err != nil || (on != "1" && !strings.EqualFold(on, "true")) {
				next(ctx, b, update)
				return
			}

			fromID, targetChatID, kind := maintenanceActor(update)
			if fromID == 0 {
				next(ctx, b, update)
				return
			}
			if adm[fromID] {
				next(ctx, b, update)
				return
			}

			_ = wait.AddTGID(fromID)

			switch kind {
			case "precheckout":
				if update.PreCheckoutQuery != nil {
					_, _ = b.AnswerPreCheckoutQuery(ctx, &bot.AnswerPreCheckoutQueryParams{
						PreCheckoutQueryID: update.PreCheckoutQuery.ID,
						OK:                 false,
						ErrorMessage:       "Технические работы. Попробуйте позже.",
					})
				}
				return
			case "shipping":
				if update.ShippingQuery != nil {
					_, _ = b.AnswerShippingQuery(ctx, &bot.AnswerShippingQueryParams{
						ShippingQueryID: update.ShippingQuery.ID,
						OK:              false,
						ErrorMessage:    "Технические работы.",
					})
				}
				return
			case "inline":
				if update.InlineQuery != nil {
					_, _ = b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
						InlineQueryID: update.InlineQuery.ID,
						Results:       []models.InlineQueryResult{},
						CacheTime:     1,
						Button: &models.InlineQueryResultsButton{
							Text:           "Технические работы",
							StartParameter: "maintenance",
						},
					})
				}
				return
			case "callback":
				if update.CallbackQuery != nil {
					_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
						CallbackQueryID: update.CallbackQuery.ID,
					})
				}
				fallthrough
			default:
				if targetChatID != 0 {
					_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
						ChatID: targetChatID,
						Text:   MaintenanceUserMessage,
					})
				}
			}
		}
	}
}

// maintenanceActor: fromID — кто нажал; targetChatID — куда ответить (личка / группа).
func maintenanceActor(u *models.Update) (fromID int64, targetChatID int64, kind string) {
	switch {
	case u.Message != nil:
		if u.Message.From != nil {
			fromID = u.Message.From.ID
		}
		targetChatID = u.Message.Chat.ID
		return fromID, targetChatID, "message"
	case u.EditedMessage != nil:
		if u.EditedMessage.From != nil {
			fromID = u.EditedMessage.From.ID
		}
		targetChatID = u.EditedMessage.Chat.ID
		return fromID, targetChatID, "message"
	case u.CallbackQuery != nil:
		fromID = u.CallbackQuery.From.ID
		if u.CallbackQuery.Message.Type == models.MaybeInaccessibleMessageTypeMessage && u.CallbackQuery.Message.Message != nil {
			targetChatID = u.CallbackQuery.Message.Message.Chat.ID
		} else {
			targetChatID = fromID
		}
		return fromID, targetChatID, "callback"
	case u.PreCheckoutQuery != nil:
		fromID = u.PreCheckoutQuery.From.ID
		return fromID, 0, "precheckout"
	case u.InlineQuery != nil:
		fromID = u.InlineQuery.From.ID
		return fromID, 0, "inline"
	case u.ShippingQuery != nil:
		fromID = u.ShippingQuery.From.ID
		return fromID, 0, "shipping"
	case u.ChosenInlineResult != nil:
		fromID = u.ChosenInlineResult.From.ID
		targetChatID = fromID
		return fromID, targetChatID, "message"
	}
	return 0, 0, ""
}
