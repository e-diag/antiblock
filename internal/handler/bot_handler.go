package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

const broadcastDelayMs = 50 // ~20 сообщений в секунду (лимит Telegram ~30/сек)

// BotHandler обрабатывает команды бота
type BotHandler struct {
	userUC         usecase.UserUseCase
	proxyUC        usecase.ProxyUseCase
	paymentUC      usecase.PaymentUseCase
	userRepo       repository.UserRepository
	adRepo         repository.AdRepository
	dockerMgr      *docker.Manager
	adminIDs       []int64
	broadcastState *BroadcastState
}

// NewBotHandler создает новый обработчик бота
func NewBotHandler(
	userUC usecase.UserUseCase,
	proxyUC usecase.ProxyUseCase,
	paymentUC usecase.PaymentUseCase,
	userRepo repository.UserRepository,
	adRepo repository.AdRepository,
	dockerMgr *docker.Manager,
	broadcastState *BroadcastState,
	adminIDs []int64,
) *BotHandler {
	return &BotHandler{
		userUC:         userUC,
		proxyUC:        proxyUC,
		paymentUC:      paymentUC,
		userRepo:       userRepo,
		adRepo:         adRepo,
		dockerMgr:      dockerMgr,
		broadcastState: broadcastState,
		adminIDs:       adminIDs,
	}
}

func (h *BotHandler) isAdmin(userID int64) bool {
	for _, id := range h.adminIDs {
		if id == userID {
			return true
		}
	}
	return false
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
	description := fmt.Sprintf("Premium 30 days (ID: %d)", userID)

	payURL, _, err := h.paymentUC.CreateInvoice(amount, currency, description, userID)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось создать счёт. Попробуйте позже.")
		return
	}

	msg := "💎 Премиум подписка на 30 дней\n\n" +
		"💰 Оплата: CryptoPay или Telegram Stars\n\n" +
		"Выберите способ оплаты:"

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "💳 CryptoPay", URL: payURL}},
			{{Text: "⭐ Telegram Stars", CallbackData: "buy_stars"}},
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

// HandleManager открывает панель менеджера с inline-кнопками (только админ)
func (h *BotHandler) HandleManager(ctx context.Context, b *bot.Bot, update *models.Update) {
	msg := "🛠 <b>Панель менеджера</b>\n\nВыберите действие:"
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "📊 Статистика", CallbackData: "mgr_stats"},
				{Text: "📋 Прокси", CallbackData: "mgr_proxies"},
			},
			{
				{Text: "➕ Добавить прокси", CallbackData: "mgr_addproxy"},
				{Text: "🗑 Удалить прокси", CallbackData: "mgr_delproxy"},
			},
			{
				{Text: "📢 Рассылка", CallbackData: "mgr_broadcast"},
				{Text: "📣 Объявления", CallbackData: "mgr_sendad"},
			},
			{
				{Text: "💎 Подписки", CallbackData: "mgr_subs"},
				{Text: "✅ Выдать премиум", CallbackData: "mgr_grant"},
				{Text: "❌ Отозвать премиум", CallbackData: "mgr_revoke"},
			},
		},
	}
	h.send(ctx, b, update, msg, kb)
}

// HandleManagerCallback обрабатывает нажатия кнопок панели менеджера
func (h *BotHandler) HandleManagerCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}
	cqID := update.CallbackQuery.ID
	chatID := update.CallbackQuery.From.ID
	data := update.CallbackQuery.Data

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})

	send := func(text string) {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML,
		})
	}

	switch data {
	case "mgr_stats":
		stats, err := h.proxyUC.GetStats()
		if err != nil {
			send("❌ Ошибка статистики")
			return
		}
		userCount, _ := h.userRepo.Count()
		send(fmt.Sprintf("📊 <b>Статистика</b>\n\n👥 Пользователей: %d\n🌐 Прокси: %d\n✅ Активных: %d\n🆓 Free: %d\n💎 Premium: %d",
			userCount, stats.TotalProxies, stats.ActiveProxies, stats.FreeProxies, stats.PremiumProxies))

	case "mgr_proxies":
		proxies, err := h.proxyUC.GetAll()
		if err != nil {
			send("❌ Ошибка списка прокси")
			return
		}
		if len(proxies) == 0 {
			send("Нет прокси. Добавьте через кнопку «Добавить прокси» и отправьте:\n<code>/addproxy ip port secret Free</code>")
			return
		}
		var sb strings.Builder
		sb.WriteString("📋 <b>Прокси</b> (удалить: /delproxy id)\n\n")
		for _, p := range proxies {
			sb.WriteString(fmt.Sprintf("• ID %d: %s:%d [%s] %s\n", p.ID, p.IP, p.Port, p.Type, p.Status))
		}
		send(sb.String())

	case "mgr_addproxy":
		send("➕ <b>Добавить прокси</b>\n\nОтправьте команду:\n<code>/addproxy &lt;ip&gt; &lt;port&gt; &lt;secret&gt; Free</code>\nили <code>Premium</code>")

	case "mgr_delproxy":
		send("🗑 <b>Удалить прокси</b>\n\nСначала откройте «📋 Прокси», затем отправьте:\n<code>/delproxy &lt;id&gt;</code>")

	case "mgr_broadcast":
		h.broadcastState.SetAwaiting(chatID)
		send("📢 <b>Рассылка</b>\n\nВведите текст сообщения (не команду). Отмена: /cancel")

	case "mgr_sendad":
		ads, err := h.adRepo.GetActive()
		if err != nil || len(ads) == 0 {
			send("❌ Нет активных объявлений.")
			return
		}
		users, err := h.userRepo.GetAll()
		if err != nil {
			send("❌ Ошибка списка пользователей")
			return
		}
		sent := 0
		for _, ad := range ads {
			text := ad.Text
			var kb *models.InlineKeyboardMarkup
			if ad.ButtonURL != nil && *ad.ButtonURL != "" {
				btnText := "Перейти"
				if ad.ButtonText != nil {
					btnText = *ad.ButtonText
				}
				kb = &models.InlineKeyboardMarkup{
					InlineKeyboard: [][]models.InlineKeyboardButton{
						{{Text: btnText, URL: *ad.ButtonURL}},
					},
				}
			}
			for _, u := range users {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: u.TGID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
				})
				sent++
				time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
			}
		}
		send(fmt.Sprintf("✅ Разослано: %d получателям", sent))

	case "mgr_subs":
		users, err := h.userRepo.GetPremiumUsers()
		if err != nil {
			send("❌ Ошибка списка подписок")
			return
		}
		if len(users) == 0 {
			send("💎 Нет активных премиум-подписок.")
			return
		}
		var sb strings.Builder
		sb.WriteString("💎 <b>Премиум-подписки</b>\n\n")
		for _, u := range users {
			until := "—"
			if u.PremiumUntil != nil {
				until = u.PremiumUntil.Format("02.01.2006")
			}
			sb.WriteString(fmt.Sprintf("• TG %d — до %s\n", u.TGID, until))
		}
		sb.WriteString("\nВыдать: <code>/grantpremium tg_id дней</code>\nОтозвать: <code>/revokepremium tg_id</code>")
		send(sb.String())

	case "mgr_grant":
		send("✅ <b>Выдать премиум</b>\n\nОтправьте:\n<code>/grantpremium &lt;tg_id&gt; &lt;дней&gt;</code>")

	case "mgr_revoke":
		send("❌ <b>Отозвать премиум</b>\n\nОтправьте:\n<code>/revokepremium &lt;tg_id&gt;</code>")

	default:
		send("Неизвестное действие.")
	}
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

// HandleAdminStats выводит статистику по премиум‑контейнерам
func (h *BotHandler) HandleAdminStats(ctx context.Context, b *bot.Bot, update *models.Update) {
	proxies, err := h.proxyUC.GetAll()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка получения списка прокси")
		return
	}
	var (
		activePremium int
		ports         []int
	)
	for _, p := range proxies {
		if p.Type == domain.ProxyTypePremium {
			ports = append(ports, p.Port)
			if p.Status == domain.ProxyStatusActive {
				activePremium++
			}
		}
	}

	// Условный лимит ресурсов сервера: 2GB под премиум‑контейнеры по 100MB
	const totalRAMMB = 2048
	const perContainerMB = 100
	maxSlots := totalRAMMB / perContainerMB
	freeSlots := maxSlots - activePremium
	if freeSlots < 0 {
		freeSlots = 0
	}

	msg := fmt.Sprintf(
		"📊 <b>Premium Docker статистика</b>\n\n"+
			"🟢 Активных премиум‑контейнеров: <b>%d</b>\n"+
			"🔌 Занятые порты: %v\n"+
			"💾 Свободные слоты (по 100MB): <b>%d</b>\n",
		activePremium, ports, freeSlots,
	)
	h.sendText(ctx, b, update, msg)
}

// HandleAdminInfo показывает статус прокси и контейнера конкретного пользователя
func (h *BotHandler) HandleAdminInfo(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "Использование: /admin_info <tg_id>")
		return
	}
	tgID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный tg_id")
		return
	}

	user, err := h.userRepo.GetByTGID(tgID)
	if err != nil || user == nil {
		h.sendText(ctx, b, update, "❌ Пользователь не найден")
		return
	}

	proxy, err := h.proxyUC.GetByOwnerID(user.ID)
	if err != nil || proxy == nil {
		h.sendText(ctx, b, update, "❌ Персональный премиум‑прокси не найден")
		return
	}

	name := fmt.Sprintf(docker.UserContainerName, tgID)
	containerStatus := "неизвестен"
	if h.dockerMgr != nil {
		running, err := h.dockerMgr.IsContainerRunning(ctx, name)
		if err == nil {
			if running {
				containerStatus = "🟢 запущен"
			} else {
				containerStatus = "🔴 остановлен"
			}
		}
	}

	until := "—"
	if user.PremiumUntil != nil {
		until = user.PremiumUntil.Format("02.01.2006 15:04")
	}

	msg := fmt.Sprintf(
		"👤 <b>Пользователь %d</b>\n\n"+
			"💎 Премиум до: %s\n"+
			"🌐 Порт: <code>%d</code>\n"+
			"🔑 Secret: <code>%s</code>\n"+
			"📦 Контейнер: <code>%s</code>\n"+
			"⚙️ Статус контейнера: %s\n",
		tgID, until, proxy.Port, proxy.Secret, name, containerStatus,
	)
	h.sendText(ctx, b, update, msg)
}

// HandleAdminRebuild принудительно пересоздает Docker‑контейнер пользователя
func (h *BotHandler) HandleAdminRebuild(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "Использование: /admin_rebuild <tg_id>")
		return
	}
	tgID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный tg_id")
		return
	}

	if h.dockerMgr == nil {
		h.sendText(ctx, b, update, "❌ Docker менеджер недоступен")
		return
	}

	user, err := h.userRepo.GetByTGID(tgID)
	if err != nil || user == nil {
		h.sendText(ctx, b, update, "❌ Пользователь не найден")
		return
	}

	proxy, err := h.proxyUC.GetByOwnerID(user.ID)
	if err != nil || proxy == nil {
		h.sendText(ctx, b, update, "❌ Персональный премиум‑прокси не найден")
		return
	}

	name := fmt.Sprintf(docker.UserContainerName, tgID)
	if err := h.dockerMgr.RemoveUserContainer(ctx, name); err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка удаления контейнера")
		return
	}
	if err := h.dockerMgr.CreateUserContainer(ctx, tgID, proxy); err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка создания контейнера")
		return
	}

	h.sendText(ctx, b, update, "✅ Контейнер пересоздан")
}

// HandleBroadcast обрабатывает команду /broadcast (только для админов)
func (h *BotHandler) HandleBroadcast(ctx context.Context, b *bot.Bot, update *models.Update) {
	adminID := chatID(update)
	h.broadcastState.SetAwaiting(adminID)
	h.sendText(ctx, b, update, "📢 Введите сообщение для рассылки. Отправьте текст (не команду). Для отмены отправьте /cancel")
}

// HandleBroadcastMessage выполняет рассылку по списку пользователей с rate limit
func (h *BotHandler) HandleBroadcastMessage(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}
	adminID := chatID(update)
	if !h.broadcastState.IsAwaiting(adminID) {
		return
	}

	users, err := h.userRepo.GetAll()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка получения списка пользователей")
		h.broadcastState.Clear(adminID)
		return
	}

	text := update.Message.Text
	sent, failed := 0, 0
	for _, u := range users {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: u.TGID,
			Text:   text,
			ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			failed++
		} else {
			sent++
		}
		time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
	}

	h.broadcastState.Clear(adminID)
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Рассылка завершена. Доставлено: %d, ошибок: %d", sent, failed))
}

// DefaultHandler вызывается, если ни один обработчик не сработал (для broadcast и т.д.)
func (h *BotHandler) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message != nil && !strings.HasPrefix(update.Message.Text, "/") {
		cid := chatID(update)
		if h.isAdmin(cid) && h.broadcastState.IsAwaiting(cid) {
			h.HandleBroadcastMessage(ctx, b, update)
			return
		}
	}
}

// HandleDelProxy удаляет прокси по ID (только админ)
func (h *BotHandler) HandleDelProxy(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 2 {
		h.sendText(ctx, b, update, "❌ Использование: /delproxy <id>\nСписок прокси: /proxies")
		return
	}
	id, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный ID прокси")
		return
	}
	if err := h.proxyUC.DeleteProxy(uint(id)); err != nil {
		h.sendText(ctx, b, update, "❌ "+err.Error())
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Прокси #%d удалён", id))
}

// HandleProxies выводит список прокси с ID для удаления
func (h *BotHandler) HandleProxies(ctx context.Context, b *bot.Bot, update *models.Update) {
	proxies, err := h.proxyUC.GetAll()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка получения списка прокси")
		return
	}
	if len(proxies) == 0 {
		h.sendText(ctx, b, update, "Нет прокси. Добавьте: /addproxy <ip> <port> <secret> <type>")
		return
	}
	var sb strings.Builder
	sb.WriteString("📋 Прокси (удаление: /delproxy <id>):\n\n")
	for _, p := range proxies {
		sb.WriteString(fmt.Sprintf("ID %d: %s:%d [%s] %s\n", p.ID, p.IP, p.Port, p.Type, p.Status))
	}
	h.sendText(ctx, b, update, sb.String())
}

// HandleSubs список премиум-подписок (админ)
func (h *BotHandler) HandleSubs(ctx context.Context, b *bot.Bot, update *models.Update) {
	users, err := h.userRepo.GetPremiumUsers()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка получения списка")
		return
	}
	if len(users) == 0 {
		h.sendText(ctx, b, update, "Нет активных премиум-подписок.")
		return
	}
	var sb strings.Builder
	sb.WriteString("💎 Премиум-подписки:\n\n")
	for _, u := range users {
		until := "—"
		if u.PremiumUntil != nil {
			until = u.PremiumUntil.Format("02.01.2006")
		}
		sb.WriteString(fmt.Sprintf("TG %d — до %s\n", u.TGID, until))
	}
	sb.WriteString("\n/grantpremium <tg_id> <дней>\n/revokepremium <tg_id>")
	h.sendText(ctx, b, update, sb.String())
}

// HandleGrantPremium выдать премиум вручную (админ)
func (h *BotHandler) HandleGrantPremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 3 {
		h.sendText(ctx, b, update, "❌ Использование: /grantpremium <tg_id> <дней>")
		return
	}
	tgID, err1 := strconv.ParseInt(args[1], 10, 64)
	days, err2 := strconv.Atoi(args[2])
	if err1 != nil || err2 != nil || days < 1 {
		h.sendText(ctx, b, update, "❌ Неверные аргументы")
		return
	}
	if err := h.userUC.ActivatePremium(tgID, days); err != nil {
		h.sendText(ctx, b, update, "❌ "+err.Error())
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Премиум выдан пользователю %d на %d дн.", tgID, days))
}

// HandleRevokePremium отозвать премиум (админ)
func (h *BotHandler) HandleRevokePremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 2 {
		h.sendText(ctx, b, update, "❌ Использование: /revokepremium <tg_id>")
		return
	}
	tgID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный tg_id")
		return
	}
	if err := h.userUC.RevokePremium(tgID); err != nil {
		h.sendText(ctx, b, update, "❌ "+err.Error())
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Премиум отозван у %d", tgID))
}

// HandleSendAd рассылает активные объявления всем (админ)
func (h *BotHandler) HandleSendAd(ctx context.Context, b *bot.Bot, update *models.Update) {
	ads, err := h.adRepo.GetActive()
	if err != nil || len(ads) == 0 {
		h.sendText(ctx, b, update, "❌ Нет активных объявлений.")
		return
	}
	users, err := h.userRepo.GetAll()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка списка пользователей")
		return
	}
	sent := 0
	for _, ad := range ads {
		text := ad.Text
		var kb *models.InlineKeyboardMarkup
		if ad.ButtonURL != nil && *ad.ButtonURL != "" {
			btnText := "Перейти"
			if ad.ButtonText != nil {
				btnText = *ad.ButtonText
			}
			kb = &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: btnText, URL: *ad.ButtonURL}},
				},
			}
		}
		for _, u := range users {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: u.TGID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
			})
			sent++
			time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
		}
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Разослано: %d получателям", sent))
}

// HandleCancel отмена ввода рассылки
func (h *BotHandler) HandleCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	adminID := chatID(update)
	if h.broadcastState.IsAwaiting(adminID) {
		h.broadcastState.Clear(adminID)
		h.sendText(ctx, b, update, "❌ Ввод отменён.")
	}
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
	case "buy_stars":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleBuyStars(ctx, b, update)
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

// HandleBuyStars отправляет счёт на оплату Telegram Stars (XTR)
func (h *BotHandler) HandleBuyStars(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)
	user, err := h.userUC.GetOrCreateUser(userID)
	if err != nil || user.IsPremiumActive() {
		if user != nil && user.IsPremiumActive() {
			h.sendText(ctx, b, update, "✅ У вас уже есть премиум.")
		} else {
			h.sendText(ctx, b, update, "❌ Ошибка. Попробуйте позже.")
		}
		return
	}
	payload := fmt.Sprintf("premium_30_%d", userID)
	link, err := b.CreateInvoiceLink(ctx, &bot.CreateInvoiceLinkParams{
		Title:       "Premium 30 дней",
		Description: "Премиум подписка на 30 дней — доступ к быстрым прокси",
		Payload:     payload,
		Currency:    "XTR",
		Prices:      []models.LabeledPrice{{Label: "Premium 30 дней", Amount: 100}},
	})
	if err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось создать счёт Stars. Попробуйте позже.")
		return
	}
	msg := "💎 Оплата Telegram Stars (⭐)\n\nНажмите кнопку для оплаты:"
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "⭐ Оплатить Stars", URL: link}},
		},
	}
	h.send(ctx, b, update, msg, kb)
}

// HandlePreCheckout подтверждает предоплату (Stars)
func (h *BotHandler) HandlePreCheckout(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.PreCheckoutQuery == nil {
		return
	}
	_, _ = b.AnswerPreCheckoutQuery(ctx, &bot.AnswerPreCheckoutQueryParams{
		PreCheckoutQueryID: update.PreCheckoutQuery.ID,
		OK:                 true,
	})
}

// HandleSuccessfulPayment выдача премиума после оплаты Stars
func (h *BotHandler) HandleSuccessfulPayment(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.SuccessfulPayment == nil {
		return
	}
	sp := update.Message.SuccessfulPayment
	payload := sp.InvoicePayload
	const prefix = "premium_30_"
	if !strings.HasPrefix(payload, prefix) {
		return
	}
	userIDStr := strings.TrimPrefix(payload, prefix)
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		return
	}
	const days = 30
	if err := h.userUC.ActivatePremium(userID, days); err != nil {
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "✅ Оплата получена! Премиум на 30 дней активирован.",
	})
}
