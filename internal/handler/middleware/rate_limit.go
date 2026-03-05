package middleware

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// RateLimiter реализует rate limiting для бота
type RateLimiter struct {
	requests map[int64][]time.Time
	mu       sync.RWMutex
	limit    int
	window   time.Duration
}

// NewRateLimiter создает новый rate limiter
func NewRateLimiter(requestsPerSecond int, burstSize int) *RateLimiter {
	return &RateLimiter{
		requests: make(map[int64][]time.Time),
		limit:    burstSize,
		window:   time.Second / time.Duration(requestsPerSecond),
	}
}

func (rl *RateLimiter) userID(update *models.Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil {
		return update.CallbackQuery.From.ID
	}
	return 0
}

// chatIDForReply возвращает ID чата для ответа (в личке совпадает с userID).
func (rl *RateLimiter) chatIDForReply(update *models.Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil {
		return update.CallbackQuery.From.ID
	}
	return 0
}

// Middleware возвращает middleware для rate limiting
func (rl *RateLimiter) Middleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		userID := rl.userID(update)
		if userID == 0 {
			next(ctx, b, update)
			return
		}

		rl.mu.Lock()
		now := time.Now()

		userRequests := rl.requests[userID]
		validRequests := make([]time.Time, 0)
		for _, reqTime := range userRequests {
			if now.Sub(reqTime) < rl.window {
				validRequests = append(validRequests, reqTime)
			}
		}

		if len(validRequests) >= rl.limit {
			rl.mu.Unlock()
			log.Printf("[rate_limit] limit exceeded for user/chat %d", userID)
			if cid := rl.chatIDForReply(update); cid != 0 {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: cid, Text: "⏳ Слишком много запросов. Подождите секунду и попробуйте снова."})
			}
			// Обязательно отвечаем на callback, иначе у пользователя бесконечно крутится загрузка на кнопке.
			if update.CallbackQuery != nil {
				_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: update.CallbackQuery.ID,
					Text:            "Подождите секунду",
				})
			}
			return
		}

		validRequests = append(validRequests, now)
		rl.requests[userID] = validRequests
		rl.mu.Unlock()

		next(ctx, b, update)
	}
}
