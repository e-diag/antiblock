package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

// BotHandler обрабатывает команды бота
type BotHandler struct {
	userUC    usecase.UserUseCase
	proxyUC   usecase.ProxyUseCase
	paymentUC usecase.PaymentUseCase
	userRepo  repository.UserRepository
	adminIDs  []int64
}

// NewBotHandler создает новый обработчик бота
func NewBotHandler(
	userUC usecase.UserUseCase,
	proxyUC usecase.ProxyUseCase,
	paymentUC usecase.PaymentUseCase,
	userRepo repository.UserRepository,
	adminIDs []int64,
) *BotHandler {
	return &BotHandler{
		userUC:    userUC,
		proxyUC:   proxyUC,
		paymentUC: paymentUC,
		userRepo:  userRepo,
		adminIDs:  adminIDs,
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

func (h *BotHandler) send(ctx context.Context, b *bot.Bot, update *models.Update, text string, replyMarkup models.ReplyMarkup) {
	params := &bot.SendMessageParams{
		ChatID:    chatID(update),
		Text:      text,
		ParseMode:  models.ParseModeHTML,
		ReplyMarkup: replyMarkup,
	}
	b.SendMessage(ctx, params)
}

func (h *BotHandler) sendText(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	h.send(ctx, b, update, text, nil)
}

// HandleStart обрабатывает команду /start
func (h *BotHandler) HandleStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)

	user, err := h.userUC.GetOrCreateUser(userID)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}

	welcomeMsg := "👋 Добро пожаловать в AntiBlock MTProto Proxy Bot!\n\n"
	welcomeMsg += "🔐 Получите безопасный доступ к Telegram через наш прокси-сервер.\n\n"

	if user.IsPremiumActive() {
		premiumUntil := "неограниченно"
		if user.PremiumUntil != nil {
			premiumUntil = user.PremiumUntil.Format("02.01.2006 15:04")
		}
		welcomeMsg += fmt.Sprintf("✨ Ваш премиум статус активен до: %s\n\n", premiumUntil)
	} else {
		welcomeMsg += "💎 Премиум подписка дает доступ к более быстрым и стабильным прокси-серверам.\n\n"
	}

	welcomeMsg += "Выберите действие:"

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔗 Получить прокси", CallbackData: "get_proxy"}},
			{{Text: "💎 Купить премиум", CallbackData: "buy_premium"}},
		},
	}
	h.send(ctx, b, update, welcomeMsg, kb)
}

// HandleGetProxy обрабатывает запрос на получение прокси
func (h *BotHandler) HandleGetProxy(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)

	user, err := h.userUC.GetOrCreateUser(userID)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}

	proxy, err := h.proxyUC.GetProxyForUser(user)
	if err != nil {
		h.sendText(ctx, b, update, "❌ В данный момент нет доступных прокси-серверов. Попробуйте позже.")
		return
	}

	proxyURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s",
		proxy.IP, proxy.Port, proxy.Secret)

	msg := fmt.Sprintf("✅ Ваш прокси-сервер:\n\n"+
		"🌐 IP: <code>%s</code>\n"+
		"🔌 Порт: <code>%d</code>\n"+
		"🔑 Секрет: <code>%s</code>\n\n"+
		"Нажмите на кнопку ниже для автоматической настройки:",
		proxy.IP, proxy.Port, proxy.Secret)

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔗 Подключиться", URL: proxyURL}},
		},
	}
	h.send(ctx, b, update, msg, kb)
}

// HandleBuyPremium обрабатывает запрос на покупку премиума
func (h *BotHandler) HandleBuyPremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)

	user, err := h.userUC.GetOrCreateUser(userID)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}

	if user.IsPremiumActive() {
		premiumUntil := "неограниченно"
		if user.PremiumUntil != nil {
			premiumUntil = user.PremiumUntil.Format("02.01.2006 15:04")
		}
		h.sendText(ctx, b, update, fmt.Sprintf("✅ У вас уже активна премиум подписка до %s", premiumUntil))
		return
	}

	amount := 10.0
	currency := "USD"
	description := fmt.Sprintf("Premium subscription for 30 days (User ID: %d)", userID)

	payURL, err := h.paymentUC.CreateInvoice(amount, currency, description, userID)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось создать счет на оплату. Попробуйте позже.")
		return
	}

	msg := fmt.Sprintf("💎 Премиум подписка на 30 дней\n\n"+
		"💰 Стоимость: %.2f %s\n\n"+
		"Нажмите на кнопку ниже для оплаты:",
		amount, currency)

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "💳 Оплатить", URL: payURL}},
			{{Text: "❌ Отмена", CallbackData: "cancel_payment"}},
		},
	}
	h.send(ctx, b, update, msg, kb)
}

// HandleAddProxy обрабатывает команду /addproxy (только для админов)
func (h *BotHandler) HandleAddProxy(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 5 {
		h.sendText(ctx, b, update, "❌ Использование: /addproxy <ip> <port> <secret> <type>\nТипы: Free или Premium")
		return
	}

	ip := args[1]
	port, err := strconv.Atoi(args[2])
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный формат порта")
		return
	}

	secret := args[3]
	proxyTypeStr := args[4]

	var proxyType domain.ProxyType
	if proxyTypeStr == "Free" {
		proxyType = domain.ProxyTypeFree
	} else if proxyTypeStr == "Premium" {
		proxyType = domain.ProxyTypePremium
	} else {
		h.sendText(ctx, b, update, "❌ Тип должен быть Free или Premium")
		return
	}

	if err := h.proxyUC.AddProxy(ip, port, secret, proxyType); err != nil {
		h.sendText(ctx, b, update, fmt.Sprintf("❌ Ошибка при добавлении прокси: %v", err))
		return
	}

	h.sendText(ctx, b, update, fmt.Sprintf("✅ Прокси-сервер добавлен:\nIP: %s\nПорт: %d\nТип: %s", ip, port, proxyType))
}

// HandleStats обрабатывает команду /stats (только для админов)
func (h *BotHandler) HandleStats(ctx context.Context, b *bot.Bot, update *models.Update) {
	stats, err := h.proxyUC.GetStats()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка при получении статистики")
		return
	}

	userCount, err := h.userRepo.Count()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка при получении статистики пользователей")
		return
	}

	msg := fmt.Sprintf("📊 Статистика:\n\n"+
		"👥 Всего пользователей: %d\n\n"+
		"🌐 Всего прокси: %d\n"+
		"✅ Активных прокси: %d\n"+
		"🆓 Бесплатных: %d\n"+
		"💎 Премиум: %d",
		userCount, stats.TotalProxies, stats.ActiveProxies, stats.FreeProxies, stats.PremiumProxies)

	h.sendText(ctx, b, update, msg)
}

// HandleBroadcast обрабатывает команду /broadcast (только для админов)
func (h *BotHandler) HandleBroadcast(ctx context.Context, b *bot.Bot, update *models.Update) {
	h.sendText(ctx, b, update, "📢 Введите сообщение для рассылки всем пользователям:")
}

// HandleCallback обрабатывает callback-запросы от inline кнопок
func (h *BotHandler) HandleCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}

	data := update.CallbackQuery.Data
	chatID := update.CallbackQuery.From.ID
	cqID := update.CallbackQuery.ID

	switch data {
	case "buy_premium":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleBuyPremium(ctx, b, update)
	case "get_proxy":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleGetProxy(ctx, b, update)
	case "cancel_payment":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Оплата отменена",
		})
		b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Оплата отменена"})
	default:
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Неизвестная команда",
		})
	}
}
