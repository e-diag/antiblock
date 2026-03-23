package middleware

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// AdminMiddleware проверяет, является ли пользователь администратором
func AdminMiddleware(adminIDs []int64) func(bot.HandlerFunc) bot.HandlerFunc {
	adminMap := make(map[int64]bool)
	for _, id := range adminIDs {
		adminMap[id] = true
	}

	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			var userID int64
			if update.Message != nil {
				userID = update.Message.Chat.ID
			} else if update.CallbackQuery != nil {
				userID = update.CallbackQuery.From.ID
			}
			if userID == 0 || !adminMap[userID] {
				b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID(update),
					Text:   "❌ У вас нет прав администратора",
				})
				return
			}
			next(ctx, b, update)
		}
	}
}

func chatID(update *models.Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil {
		return update.CallbackQuery.From.ID
	}
	return 0
}
