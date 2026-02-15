package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

const broadcastDelayMs = 50 // ~20 сообщений в секунду (лимит Telegram ~30/сек)

// formatProxyLine возвращает одну строку для списка прокси (админка): ID, ip:port, type, status, опционально Load, RTT и "!" для недоступного премиум.
func formatProxyLine(p *domain.ProxyNode, withBullet bool) string {
	line := fmt.Sprintf("ID %d: %s:%d [%s] %s", p.ID, p.IP, p.Port, p.Type, p.Status)
	if p.Type == domain.ProxyTypePremium && p.UnreachableSince != nil {
		line += " ! не отвечает"
	}
	if p.Type == domain.ProxyTypeFree {
		line += fmt.Sprintf(" Load:%d", p.Load)
	}
	if p.LastRTTMs != nil {
		line += fmt.Sprintf(" RTT:%d мс", *p.LastRTTMs)
	}
	if withBullet {
		return "• " + line
	}
	return line
}

// BotHandler обрабатывает команды бота
type BotHandler struct {
	userUC         usecase.UserUseCase
	proxyUC        usecase.ProxyUseCase
	paymentUC      usecase.PaymentUseCase
	userRepo       repository.UserRepository
	userProxyRepo  repository.UserProxyRepository
	adRepo         repository.AdRepository
	adPinRepo      repository.AdPinRepository
	settingsRepo   repository.SettingsRepository

	dockerMgr   *docker.Manager
	forcedSubCh string // из конфига (fallback, если в БД пусто)

	broadcastState       *BroadcastState
	broadcastMediaGroup  *BroadcastMediaGroupBuffer
	adComposeState      *AdComposeState
	opAwaiting          map[int64]bool // админ вводит новый канал ОП
	opMu                sync.Mutex
	adminIDs            []int64
	botRef              *bot.Bot // для асинхронной рассылки альбомов (устанавливается из main)
}

// NewBotHandler создает новый обработчик бота
func NewBotHandler(
	userUC usecase.UserUseCase,
	proxyUC usecase.ProxyUseCase,
	paymentUC usecase.PaymentUseCase,
	userRepo repository.UserRepository,
	userProxyRepo repository.UserProxyRepository,
	adRepo repository.AdRepository,
	adPinRepo repository.AdPinRepository,
	settingsRepo repository.SettingsRepository,
	dockerMgr *docker.Manager,
	forcedSubCh string,
	broadcastState      *BroadcastState,
	broadcastMediaGroup *BroadcastMediaGroupBuffer,
	adComposeState      *AdComposeState,
	adminIDs            []int64,
) *BotHandler {
	return &BotHandler{
		userUC:             userUC,
		proxyUC:            proxyUC,
		paymentUC:          paymentUC,
		userRepo:           userRepo,
		userProxyRepo:      userProxyRepo,
		adRepo:             adRepo,
		adPinRepo:          adPinRepo,
		settingsRepo:       settingsRepo,
		dockerMgr:          dockerMgr,
		forcedSubCh:        forcedSubCh,
		broadcastState:     broadcastState,
		broadcastMediaGroup: broadcastMediaGroup,
		adComposeState:     adComposeState,
		opAwaiting:         make(map[int64]bool),
		adminIDs:           adminIDs,
	}
}

// SetBot сохраняет ссылку на бота для асинхронной рассылки альбомов (вызывается из main после создания бота)
func (h *BotHandler) SetBot(b *bot.Bot) {
	h.botRef = b
}

func (h *BotHandler) isAdmin(userID int64) bool {
	for _, id := range h.adminIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func (h *BotHandler) setOPAwaiting(adminID int64, v bool) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if v {
		h.opAwaiting[adminID] = true
	} else {
		delete(h.opAwaiting, adminID)
	}
}

func (h *BotHandler) isOPAwaiting(adminID int64) bool {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	return h.opAwaiting[adminID]
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

// getUsername возвращает Telegram @username из update (Message.From, CallbackQuery.From и т.д.); пустая строка, если нет.
func (h *BotHandler) getUsername(update *models.Update) string {
	if update.Message != nil && update.Message.From != nil && update.Message.From.Username != "" {
		return update.Message.From.Username
	}
	if update.CallbackQuery != nil && update.CallbackQuery.From.Username != "" {
		return update.CallbackQuery.From.Username
	}
	if update.EditedMessage != nil && update.EditedMessage.From != nil && update.EditedMessage.From.Username != "" {
		return update.EditedMessage.From.Username
	}
	if update.PreCheckoutQuery != nil && update.PreCheckoutQuery.From != nil && update.PreCheckoutQuery.From.Username != "" {
		return update.PreCheckoutQuery.From.Username
	}
	return ""
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

// mainMenuContent возвращает текст и клавиатуру главного меню пользователя (для /start и кнопки «Назад»).
func (h *BotHandler) mainMenuContent(user *domain.User) (welcomeMsg string, kb *models.InlineKeyboardMarkup) {
	welcomeMsg = "👋 <b>Добро пожаловать в eDiag Proxy Bot!</b>\n\n"
	welcomeMsg += "🔐 Безопасный доступ к Telegram через MTProto-прокси.\n\n"
	welcomeMsg += "Нажми: <b>«Получить proxy»</b> → <b>«Подключиться»</b> → <b>«Включить»</b> — и Telegram работает без замедлений!\n\n"
	welcomeMsg += "💎 Купи Premium — получи персональный Proxy, без ограничений по скорости и количеству.\n\n"
	if user.IsPremiumActive() {
		premiumUntil := "неограниченно"
		if user.PremiumUntil != nil {
			premiumUntil = user.PremiumUntil.Format("02.01.2006 15:04")
		}
		welcomeMsg += fmt.Sprintf("✨ Ваш премиум активен до: %s\n\n", premiumUntil)
	}
	welcomeMsg += "Выберите действие:"
	days := h.getPremiumDays()
	usdt := h.getPremiumUSDT()
	stars := h.getPremiumStars()
	btnPremium := fmt.Sprintf("💎 Premium — получи персональный proxy на %d дн. (%.2f USDT / %d ⭐)", days, usdt, stars)
	if len(btnPremium) > 64 {
		btnPremium = fmt.Sprintf("💎 Premium — proxy на %d дн.", days)
	}
	// Одна кнопка на получение бесплатного прокси: для новых — «Получить прокси», если уже есть хотя бы один free — «Получить дополнительный прокси».
	btnGetProxy := "🔗 Получить прокси"
	if h.userProxyRepo != nil {
		if list, err := h.userProxyRepo.ListByUserID(user.ID); err == nil {
			for _, up := range list {
				if up.ProxyType == domain.ProxyTypeFree {
					btnGetProxy = "➕ Получить дополнительный прокси"
					break
				}
			}
		}
	}
	rows := [][]models.InlineKeyboardButton{
		{{Text: btnGetProxy, CallbackData: "get_proxy"}},
		{{Text: btnPremium, CallbackData: "buy_premium"}},
	}
	if user.IsPremiumActive() {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "🔐 Получить Premium proxy", CallbackData: "get_premium_proxy"}})
	}
	rows = append(rows, []models.InlineKeyboardButton{{Text: "📋 Мои прокси", CallbackData: "my_proxies"}})
	kb = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	return welcomeMsg, kb
}

// HandleStart обрабатывает команду /start
func (h *BotHandler) HandleStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)
	user, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}
	welcomeMsg, kb := h.mainMenuContent(user)
	h.send(ctx, b, update, welcomeMsg, kb)
	if !user.IsPremiumActive() {
		h.sendActiveAdIfExists(ctx, b, userID)
	}
}

// getForcedSubChannels возвращает список каналов ОП из настроек (JSON); если пусто — один канал из конфига
func (h *BotHandler) getForcedSubChannels() []string {
	if h.settingsRepo == nil {
		if h.forcedSubCh == "" {
			return nil
		}
		return []string{strings.TrimSpace(h.forcedSubCh)}
	}
	v, _ := h.settingsRepo.Get("forced_sub_channels")
	v = strings.TrimSpace(v)
	if v == "" {
		if h.forcedSubCh != "" {
			return []string{strings.TrimSpace(h.forcedSubCh)}
		}
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		if h.forcedSubCh != "" {
			return []string{strings.TrimSpace(h.forcedSubCh)}
		}
		return nil
	}
	return list
}

// setForcedSubChannels сохраняет список каналов ОП в настройки (JSON)
func (h *BotHandler) setForcedSubChannels(channels []string) error {
	if h.settingsRepo == nil {
		return fmt.Errorf("settings repo nil")
	}
	data, err := json.Marshal(channels)
	if err != nil {
		return err
	}
	return h.settingsRepo.Set("forced_sub_channels", string(data))
}

// channelToChatID приводит ссылку/username канала к виду @username для GetChatMember
func channelToChatID(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "https://t.me/") {
		return "@" + strings.TrimPrefix(strings.TrimPrefix(s, "https://t.me/"), "/")
	}
	if strings.HasPrefix(s, "t.me/") {
		return "@" + strings.TrimPrefix(s, "t.me/")
	}
	if !strings.HasPrefix(s, "@") {
		return "@" + s
	}
	return s
}

// channelToURL возвращает URL для кнопки «Подписаться»
func channelToURL(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "http") {
		return s
	}
	s = strings.TrimPrefix(s, "@")
	return "https://t.me/" + s
}

// isSubscribedToForcedChannel проверяет, подписан ли пользователь на все каналы ОП
func (h *BotHandler) isSubscribedToForcedChannel(ctx context.Context, b *bot.Bot, userID int64) bool {
	channels := h.getForcedSubChannels()
	if len(channels) == 0 {
		return true
	}
	for _, ch := range channels {
		chatID := channelToChatID(ch)
		mem, err := b.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
		if err != nil {
			return false
		}
		if mem.Left != nil || mem.Banned != nil {
			return false
		}
	}
	return true
}

// buildForcedSubKeyboard строит клавиатуру: кнопки «Подписаться» на каждый канал + «Проверить подписку»
func (h *BotHandler) buildForcedSubKeyboard(channels []string) *models.InlineKeyboardMarkup {
	if len(channels) == 0 {
		return nil
	}
	var rows [][]models.InlineKeyboardButton
	for i, ch := range channels {
		label := channelToChatID(ch)
		if len(label) > 30 {
			label = fmt.Sprintf("Канал %d", i+1)
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "📢 " + label, URL: channelToURL(ch)},
		})
	}
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "✅ Проверить подписку", CallbackData: "check_sub_forced"},
	})
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendProxyToUser получает прокси для пользователя и отправляет ему сообщение с данными прокси.
// preferFree: true — выдать free-прокси (для кнопки «Получить прокси»); false — премиум-прокси при наличии (для «Получить Premium proxy»).
func (h *BotHandler) sendProxyToUser(ctx context.Context, b *bot.Bot, chatID int64, user *domain.User, preferFree bool) {
	proxy, err := h.proxyUC.GetProxyForUser(user, preferFree)
	if err != nil {
		msg := "❌ В данный момент нет доступных прокси-серверов. Попробуйте позже."
		if errors.Is(err, usecase.ErrNoMoreFreeProxiesForUser) {
			msg = "❌ Доступных прокси больше нет. Вы уже получили все бесплатные прокси."
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML,
		})
		return
	}
	proxyURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", proxy.IP, proxy.Port, proxy.Secret)
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
	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
	if h.userProxyRepo != nil {
		_ = h.userProxyRepo.Create(&domain.UserProxy{
			UserID:    user.ID,
			IP:        proxy.IP,
			Port:      proxy.Port,
			Secret:    proxy.Secret,
			ProxyType: proxy.Type,
		})
	}
	if !user.IsPremiumActive() {
		h.sendActiveAdIfExists(ctx, b, chatID)
	}
}

func (h *BotHandler) getPremiumDays() int {
	if h.settingsRepo == nil {
		return 30
	}
	v, _ := h.settingsRepo.Get("premium_days")
	if v == "" {
		return 30
	}
	n, _ := strconv.Atoi(v)
	if n < 1 || n > 365 {
		return 30
	}
	return n
}

func (h *BotHandler) getPremiumUSDT() float64 {
	if h.settingsRepo == nil {
		return 10
	}
	v, _ := h.settingsRepo.Get("premium_usdt")
	if v == "" {
		return 10
	}
	f, _ := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
	if f < 0.01 {
		return 10
	}
	return f
}

func (h *BotHandler) getPremiumStars() int {
	if h.settingsRepo == nil {
		return 100
	}
	v, _ := h.settingsRepo.Get("premium_stars")
	if v == "" {
		return 100
	}
	n, _ := strconv.Atoi(v)
	if n < 1 {
		return 100
	}
	return n
}

// buildAdKeyboard возвращает клавиатуру с одной кнопкой объявления. Используется CallbackData (ad_click_ID), чтобы бот получал callback и считал клики; ссылка открывается через AnswerCallbackQuery(URL).
func (h *BotHandler) buildAdKeyboard(ad *domain.Ad) *models.InlineKeyboardMarkup {
	hasLink := (ad.ButtonURL != nil && *ad.ButtonURL != "") || ad.ChannelLink != ""
	if !hasLink {
		return nil
	}
	btnText := "Подписаться"
	if ad.ButtonText != nil && *ad.ButtonText != "" {
		btnText = *ad.ButtonText
	}
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: btnText, CallbackData: fmt.Sprintf("ad_click_%d", ad.ID)}},
		},
	}
}

// sendActiveAdIfExists отправляет активное объявление пользователю (для бесплатных) один раз — если для этого объявления ещё нет записи в ad_pins. Закрепляет и сохраняет в ad_pins.
func (h *BotHandler) sendActiveAdIfExists(ctx context.Context, b *bot.Bot, chatID int64) {
	ad, err := h.adRepo.GetActiveOne()
	if err != nil || ad == nil {
		return
	}
	if ad.ExpiresAt != nil && ad.ExpiresAt.Before(time.Now()) {
		return
	}
	if h.adPinRepo != nil {
		exists, _ := h.adPinRepo.ExistsByAdIDAndUserID(ad.ID, chatID)
		if exists {
			return
		}
	}
	kb := h.buildAdKeyboard(ad)
	msg, errSend := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: ad.Text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
	})
	if errSend != nil {
		return
	}
	if msg != nil && msg.ID != 0 {
		if _, errPin := b.PinChatMessage(ctx, &bot.PinChatMessageParams{ChatID: chatID, MessageID: msg.ID}); errPin != nil {
			// Закрепление не критично, объявление уже отправлено
		} else if h.adPinRepo != nil {
			_ = h.adPinRepo.Create(&domain.AdPin{AdID: ad.ID, UserID: chatID, ChatID: chatID, MessageID: msg.ID})
		}
	}
	_ = h.adRepo.IncrementImpressions(ad.ID)
}

// HandleGetProxy обрабатывает запрос на получение прокси. Для бесплатных проверяется обязательная подписка на канал.
func (h *BotHandler) HandleGetProxy(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)

	user, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}

	// Для бесплатных пользователей проверяем обязательную подписку на все каналы ОП
	channels := h.getForcedSubChannels()
	if !user.IsPremiumActive() && len(channels) > 0 {
		if !h.isSubscribedToForcedChannel(ctx, b, userID) {
			kb := h.buildForcedSubKeyboard(channels)
			msg := "⚠️ Чтобы получить прокси, подпишитесь на каналы ниже. После подписки нажмите «Проверить подписку»."
			if len(channels) == 1 {
				msg = "⚠️ Чтобы получить прокси, подпишитесь на канал. После подписки нажмите «Проверить подписку»."
			}
			h.send(ctx, b, update, msg, kb)
			return
		}
	}

	h.sendProxyToUser(ctx, b, userID, user, true)
}

// HandleBuyPremium обрабатывает запрос на покупку премиума (оплата только через Telegram Stars; CryptoPay отключён).
func (h *BotHandler) HandleBuyPremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)

	_, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}

	days := h.getPremiumDays()
	starsCount := h.getPremiumStars()
	msg := fmt.Sprintf("💎 <b>Premium</b> — получи персональный proxy на %d дн.\n\n"+
		"• Без рекламы и ограничений\n"+
		"• Высокий приоритет и стабильность\n\n"+
		"💰 Стоимость: <b>%d ⭐ Stars</b>\n\nОплата через Telegram Stars:", days, starsCount)

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "⭐ Telegram Stars", CallbackData: "buy_stars"}},
			{{Text: "◀️ Назад", CallbackData: "cancel_payment"}},
		},
	}
	h.send(ctx, b, update, msg, kb)
}

// HandleAddProxy обрабатывает команду /addproxy (только для админов). Разрешён только тип Free; Premium создаётся автоматически.
func (h *BotHandler) HandleAddProxy(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 5 {
		h.sendText(ctx, b, update, "❌ Использование: /addproxy <ip> <port> <secret> <type>\nТип: только Free (премиум-прокси создаются автоматически при покупке подписки).")
		return
	}

	ip := args[1]
	port, err := strconv.Atoi(args[2])
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный формат порта")
		return
	}

	secret := args[3]
	proxyTypeStr := strings.ToLower(strings.TrimSpace(args[4]))

	if proxyTypeStr != "free" {
		if proxyTypeStr == "premium" {
			h.sendText(ctx, b, update, "❌ Премиум-прокси добавлять вручную нельзя — они создаются автоматически при покупке пользователем премиум-подписки. Добавляйте только Free-прокси.")
			return
		}
		h.sendText(ctx, b, update, "❌ Тип должен быть Free (регистронезависимо). Премиум-прокси создаются автоматически.")
		return
	}

	if err := h.proxyUC.AddProxy(ip, port, secret, domain.ProxyTypeFree); err != nil {
		h.sendText(ctx, b, update, fmt.Sprintf("❌ Ошибка при добавлении прокси: %v", err))
		return
	}

	h.sendText(ctx, b, update, fmt.Sprintf("✅ Free-прокси добавлен:\nIP: %s\nПорт: %d", ip, port))
}

// managerPanelMessage и клавиатура главного меню (для /manager и кнопки «Назад»)
func (h *BotHandler) managerPanelContent() (msg string, kb *models.InlineKeyboardMarkup) {
	msg = "🛠 <b>Панель менеджера</b>\n\nВыберите действие:"
	kb = &models.InlineKeyboardMarkup{
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
				{Text: "📢 Управление ОП", CallbackData: "mgr_forcedsub"},
			},
			{
				{Text: "💎 Подписки", CallbackData: "mgr_subs"},
				{Text: "⚙️ Настройки подписки", CallbackData: "mgr_pricing"},
			},
			{
				{Text: "✅ Выдать премиум", CallbackData: "mgr_grant"},
				{Text: "❌ Отозвать премиум", CallbackData: "mgr_revoke"},
			},
		},
	}
	return msg, kb
}

// refreshKeyboardStats возвращает клавиатуру [Обновить, Назад] для экрана статистики.
func refreshKeyboardStats() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔄 Обновить", CallbackData: "mgr_refresh_stats"}, {Text: "◀️ Назад", CallbackData: "mgr_back"}},
		},
	}
}

// refreshKeyboardProxies возвращает клавиатуру [Обновить, Назад] для экрана прокси.
func refreshKeyboardProxies() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔄 Обновить", CallbackData: "mgr_refresh_proxies"}, {Text: "◀️ Назад", CallbackData: "mgr_back"}},
		},
	}
}

// buildManagerStatsMessage формирует текст блока «Статистика»: пользователи, прокси (Free / Premium только активные), объявление, ОП.
func (h *BotHandler) buildManagerStatsMessage() (string, error) {
	stats, err := h.proxyUC.GetStats()
	if err != nil {
		return "", err
	}
	userCount, _ := h.userRepo.Count()
	msg := fmt.Sprintf("📊 <b>Статистика</b>\n\n👥 Пользователей: %d\n\n", userCount)
	msg += fmt.Sprintf("🌐 <b>Прокси</b>\n🆓 Free: %d (активных: см. список)\n💎 Активных премиум: %d", stats.FreeProxies, stats.PremiumProxies)
	if stats.UnreachablePremiumCount > 0 {
		msg += fmt.Sprintf(" <b>(!%d не работают)</b>", stats.UnreachablePremiumCount)
	}
	msg += "\n"
	ad, _ := h.adRepo.GetActiveOne()
	if ad != nil {
		msg += fmt.Sprintf("\n📣 <b>Объявление</b> (ID %d)\n👁 Показы: %d\n🖱 Клики: %d", ad.ID, ad.Impressions, ad.Clicks)
	}
	if h.settingsRepo != nil {
		if forcedSubs, _ := h.settingsRepo.Get("forced_subs_count"); forcedSubs != "" {
			msg += fmt.Sprintf("\n\n📢 Подписок по ОП: %s", forcedSubs)
		}
	}
	return msg, nil
}

// buildManagerProxiesMessage формирует текст списка прокси: секция Free, затем секция Premium (только активные), с пометкой "!" для недоступных.
func (h *BotHandler) buildManagerProxiesMessage() (string, error) {
	proxies, err := h.proxyUC.GetAll()
	if err != nil {
		return "", err
	}
	var freeList, premiumList []*domain.ProxyNode
	for _, p := range proxies {
		if p.Type == domain.ProxyTypeFree {
			freeList = append(freeList, p)
		} else if p.Type == domain.ProxyTypePremium && p.Status == domain.ProxyStatusActive {
			premiumList = append(premiumList, p)
		}
	}
	var sb strings.Builder
	sb.WriteString("📋 <b>Прокси</b> (удалить: /delproxy id)\n\n")
	sb.WriteString("🆓 <b>Free-прокси</b>\n")
	if len(freeList) == 0 {
		sb.WriteString("Нет.\n\n")
	} else {
		for _, p := range freeList {
			sb.WriteString(formatProxyLine(p, true) + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("💎 <b>Premium-прокси (только активные)</b>\n")
	if len(premiumList) == 0 {
		sb.WriteString("Нет.\n")
	} else {
		for _, p := range premiumList {
			sb.WriteString(formatProxyLine(p, true) + "\n")
		}
	}
	return sb.String(), nil
}

// HandleManager открывает панель менеджера с inline-кнопками (только админ)
func (h *BotHandler) HandleManager(ctx context.Context, b *bot.Bot, update *models.Update) {
	msg, kb := h.managerPanelContent()
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
	showManagerPanel := func() {
		msg, kb := h.managerPanelContent()
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
		})
	}

	switch data {
	case "mgr_back":
		showManagerPanel()

	case "mgr_stats":
		msg, err := h.buildManagerStatsMessage()
		if err != nil {
			send("❌ Ошибка статистики")
			return
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: refreshKeyboardStats(),
		})

	case "mgr_refresh_stats":
		msgObj := update.CallbackQuery.Message.Message
		if msgObj == nil {
			return
		}
		msg, err := h.buildManagerStatsMessage()
		if err != nil {
			send("❌ Ошибка статистики")
			return
		}
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      msgObj.Chat.ID,
			MessageID:  msgObj.ID,
			Text:       msg,
			ParseMode:  models.ParseModeHTML,
			ReplyMarkup: refreshKeyboardStats(),
		})

	case "mgr_proxies":
		msg, err := h.buildManagerProxiesMessage()
		if err != nil {
			send("❌ Ошибка списка прокси")
			return
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: refreshKeyboardProxies(),
		})

	case "mgr_refresh_proxies":
		msgObj := update.CallbackQuery.Message.Message
		if msgObj == nil {
			return
		}
		msg, err := h.buildManagerProxiesMessage()
		if err != nil {
			send("❌ Ошибка списка прокси")
			return
		}
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      msgObj.Chat.ID,
			MessageID:  msgObj.ID,
			Text:       msg,
			ParseMode:  models.ParseModeHTML,
			ReplyMarkup: refreshKeyboardProxies(),
		})

	case "mgr_addproxy":
		send("➕ <b>Добавить прокси</b>\n\nДобавляются только <b>Free</b>-прокси. Отправьте:\n<code>/addproxy &lt;ip&gt; &lt;port&gt; &lt;secret&gt; Free</code>\n\nПремиум-прокси создаются автоматически при покупке подписки.")

	case "mgr_delproxy":
		send("🗑 <b>Удалить прокси</b>\n\nСначала откройте «📋 Прокси», затем отправьте:\n<code>/delproxy &lt;id&gt;</code>")

	case "mgr_broadcast":
		// Шаг 1: выбор аудитории (без перехода в режим ожидания)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: "📢 <b>Рассылка</b>\n\nВыберите аудиторию:",
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "👥 Всем", CallbackData: "broadcast_audience_all"}},
					{{Text: "🆓 Только бесплатным", CallbackData: "broadcast_audience_free"}},
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "broadcast_audience_all":
		h.broadcastState.SetAwaiting(chatID, BroadcastAudienceAll)
		send("📢 Рассылка <b>всем</b>. Отправьте сообщение: текст, фото, видео или документ. Отмена: /cancel")

	case "broadcast_audience_free":
		h.broadcastState.SetAwaiting(chatID, BroadcastAudienceFree)
		send("📢 Рассылка <b>только бесплатным</b>. Отправьте сообщение: текст, фото, видео или документ. Отмена: /cancel")

	case "mgr_sendad":
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: "📣 <b>Объявления</b>\n\nВыберите действие:",
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "➕ Добавить объявление", CallbackData: "mgr_ad_add"}},
					{{Text: "✏️ Редактировать объявление", CallbackData: "mgr_ad_edit"}},
					{{Text: "📴 Снять объявление", CallbackData: "mgr_ad_deactivate"}},
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "mgr_ad_add":
		h.adComposeState.Set(chatID, &AdComposeData{Step: AdComposeText, EditingID: 0})
		send("📣 <b>Новое объявление</b>\n\nВведите текст объявления (HTML). Отмена: /cancel")

	case "mgr_ad_edit":
		active, err := h.adRepo.GetActiveOne()
		if err != nil || active == nil {
			send("❌ Нет активного объявления для редактирования.")
			return
		}
		d := &AdComposeData{
			Step: AdComposeText, EditingID: active.ID,
			Text: active.Text, ChannelLink: active.ChannelLink, ChannelUsername: active.ChannelUsername,
		}
		if active.ButtonText != nil {
			d.ButtonText = *active.ButtonText
		}
		if active.ButtonURL != nil {
			d.ButtonURL = *active.ButtonURL
		}
		if active.ExpiresAt != nil {
			d.ExpiresHours = int(time.Until(*active.ExpiresAt).Hours())
			if d.ExpiresHours < 0 {
				d.ExpiresHours = 24
			}
		}
		h.adComposeState.Set(chatID, d)
		send("✏️ Редактирование объявления.\nВведите новый текст (HTML). Отмена: /cancel")

	case "mgr_ad_deactivate":
		activeAd, errAd := h.adRepo.GetActiveOne()
		if errAd != nil || activeAd == nil {
			send("✅ Нет активного объявления.")
			return
		}
		if h.adPinRepo != nil {
			pins, _ := h.adPinRepo.ListByAdID(activeAd.ID)
			for _, pin := range pins {
				_, _ = b.UnpinChatMessage(ctx, &bot.UnpinChatMessageParams{ChatID: pin.ChatID, MessageID: pin.MessageID})
				time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
			}
			_ = h.adPinRepo.DeleteByAdID(activeAd.ID)
		}
		if err := h.adRepo.DeactivateAll(); err != nil {
			send("❌ Ошибка снятия объявления")
			return
		}
		send("✅ Объявление снято с закрепа и деактивировано.")

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

	case "mgr_pricing":
		days := h.getPremiumDays()
		usdt := h.getPremiumUSDT()
		stars := h.getPremiumStars()
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf("⚙️ <b>Настройки подписки</b>\n\n📅 Дней: <b>%d</b>\n💵 USDT: <b>%.2f</b>\n⭐ Stars (XTR): <b>%d</b>\n\nИзменить:\n<code>/setpricing &lt;дней&gt;</code>\n<code>/setprice_usdt &lt;сумма&gt;</code>\n<code>/setprice_stars &lt;звёзды&gt;</code>", days, usdt, stars),
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "mgr_grant":
		send("✅ <b>Выдать премиум</b>\n\nОтправьте:\n<code>/grantpremium &lt;tg_id&gt; &lt;дней&gt;</code>")

	case "mgr_revoke":
		send("❌ <b>Отозвать премиум</b>\n\nОтправьте:\n<code>/revokepremium &lt;tg_id&gt;</code>")

	case "mgr_forcedsub":
		channels := h.getForcedSubChannels()
		msg := "📢 <b>Управление каналами обязательной подписки (ОП)</b>\n\n"
		if len(channels) == 0 {
			msg += "Каналов пока нет. Добавьте канал — бесплатные пользователи должны будут подписаться на все каналы перед получением прокси."
		} else {
			msg += fmt.Sprintf("Каналов: <b>%d</b>. Для получения прокси пользователь должен быть подписан на все.\n\n", len(channels))
			for i, ch := range channels {
				msg += fmt.Sprintf("%d. <code>%s</code>\n", i+1, ch)
			}
		}
		rows := [][]models.InlineKeyboardButton{{{Text: "➕ Добавить канал", CallbackData: "mgr_op_add"}}}
		for i := range channels {
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("🗑 Удалить канал %d", i+1), CallbackData: fmt.Sprintf("mgr_op_del_%d", i)},
			})
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: "◀️ Назад", CallbackData: "mgr_back"}})
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML,
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: rows},
		})

	case "mgr_op_add":
		h.setOPAwaiting(chatID, true)
		send("📢 Введите <b>@username</b> канала или ссылку (например <code>https://t.me/channel</code>).\nОтмена: /cancel")

	default:
		// Удаление канала ОП по индексу: mgr_op_del_0, mgr_op_del_1, ...
		if strings.HasPrefix(data, "mgr_op_del_") {
			idxStr := strings.TrimPrefix(data, "mgr_op_del_")
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				send("❌ Неверный индекс")
				return
			}
			channels := h.getForcedSubChannels()
			if idx < 0 || idx >= len(channels) {
				send("❌ Канал не найден")
				return
			}
			newCh := make([]string, 0, len(channels)-1)
			for i, ch := range channels {
				if i != idx {
					newCh = append(newCh, ch)
				}
			}
			if err := h.setForcedSubChannels(newCh); err != nil {
				send("❌ Ошибка сохранения")
				return
			}
			send(fmt.Sprintf("✅ Канал «%s» удалён из ОП.", channels[idx]))
		} else {
			send("Неизвестное действие.")
		}
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

	msg := fmt.Sprintf("📊 Статистика:\n\n👥 Всего пользователей: %d\n\n🌐 Прокси: всего %d, активных %d\n🆓 Free: %d\n💎 Активных премиум: %d",
		userCount, stats.TotalProxies, stats.ActiveProxies, stats.FreeProxies, stats.PremiumProxies)
	if stats.UnreachablePremiumCount > 0 {
		msg += fmt.Sprintf(" (!%d не работают)", stats.UnreachablePremiumCount)
	}
	msg += "\n"
	ad, _ := h.adRepo.GetActiveOne()
	if ad != nil {
		msg += fmt.Sprintf("\n\n📣 Объявление: показы %d, клики %d", ad.Impressions, ad.Clicks)
	}
	if h.settingsRepo != nil {
		if forcedSubs, _ := h.settingsRepo.Get("forced_subs_count"); forcedSubs != "" {
			msg += fmt.Sprintf("\n📢 Подписок по ОП: %s", forcedSubs)
		}
	}
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

// HandleBroadcast обрабатывает команду /broadcast (только для админов): выбор аудитории, затем сообщение
func (h *BotHandler) HandleBroadcast(ctx context.Context, b *bot.Bot, update *models.Update) {
	msg := "📢 <b>Рассылка</b>\n\nВыберите аудиторию:"
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "👥 Всем", CallbackData: "broadcast_audience_all"}},
			{{Text: "🆓 Только бесплатным", CallbackData: "broadcast_audience_free"}},
		},
	}
	h.send(ctx, b, update, msg, kb)
}

// flushBroadcastMediaGroup выполняет рассылку одного альбома по списку пользователей.
// audience передаётся явно (захватывается при добавлении в буфер), чтобы при срабатывании таймера не опираться на уже очищенный broadcastState.
func (h *BotHandler) flushBroadcastMediaGroup(adminID int64, fromChatID int64, messageIDs []int, audience BroadcastAudience) {
	if h.botRef == nil || len(messageIDs) == 0 {
		return
	}
	// Telegram ожидает сообщения альбома в порядке по message_id
	sort.Ints(messageIDs)
	ctx := context.Background()
	users, err := h.userRepo.GetAll()
	if err != nil {
		h.botRef.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: adminID, Text: "❌ Ошибка получения списка пользователей", ParseMode: models.ParseModeHTML,
		})
		return
	}
	sent, failed := 0, 0
	var lastErr error
	for _, u := range users {
		if audience == BroadcastAudienceFree && u.IsPremiumActive() {
			continue
		}
		_, err := h.botRef.CopyMessages(ctx, &bot.CopyMessagesParams{
			ChatID:     u.TGID,
			FromChatID: fromChatID,
			MessageIDs: messageIDs,
		})
		if err != nil {
			failed++
			lastErr = err
		} else {
			sent++
		}
		time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
	}
	resultMsg := fmt.Sprintf("✅ Рассылка альбома завершена. Доставлено: %d, ошибок: %d", sent, failed)
	if failed > 0 && lastErr != nil {
		resultMsg += fmt.Sprintf("\n\n⚠️ Пример ошибки: %v", lastErr)
	}
	h.botRef.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: adminID, Text: resultMsg, ParseMode: models.ParseModeHTML,
	})
}

// HandleBroadcastMessage выполняет рассылку по списку пользователей (текст, фото, видео, документ) с rate limit.
// Альбомы (несколько фото/видео с одним media_group_id) буферизуются и отправляются одним CopyMessages на получателя.
func (h *BotHandler) HandleBroadcastMessage(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	adminID := chatID(update)
	if !h.broadcastState.IsAwaiting(adminID) {
		return
	}

	mediaGroupID := update.Message.MediaGroupID
	if mediaGroupID != "" && h.broadcastMediaGroup != nil {
		aud := h.broadcastState.Audience(adminID)
		h.broadcastMediaGroup.Add(adminID, mediaGroupID, update.Message.Chat.ID, update.Message.ID, aud, func(aid int64, fromChat int64, ids []int, a BroadcastAudience) {
			// Не рассылать, если рассылку отменили до срабатывания таймера
			if !h.broadcastState.IsAwaiting(aid) {
				return
			}
			h.flushBroadcastMediaGroup(aid, fromChat, ids, a)
		})
		return
	}

	// Досрочный сброс: отправить накопленные альбомы с той аудиторией, которая была при добавлении каждой группы
	if h.broadcastMediaGroup != nil {
		pending := h.broadcastMediaGroup.FlushAllForAdmin(adminID)
		for _, g := range pending {
			if len(g.MessageIDs) > 0 {
				h.flushBroadcastMediaGroup(adminID, g.FromChatID, g.MessageIDs, g.Audience)
			}
		}
	}

	// Контент: текст из Message.Text или подпись к медиа
	text := update.Message.Text
	if text == "" && update.Message.Caption != "" {
		text = update.Message.Caption
	}
	hasMedia := len(update.Message.Photo) > 0 || update.Message.Video != nil || update.Message.Document != nil
	if !hasMedia && text == "" {
		h.sendText(ctx, b, update, "❌ Отправьте текст, фото, видео или документ. Отмена: /cancel")
		return
	}

	users, err := h.userRepo.GetAll()
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка получения списка пользователей")
		h.broadcastState.Clear(adminID)
		return
	}

	sent, failed := 0, 0
	aud := h.broadcastState.Audience(adminID)
	fromChatID := update.Message.Chat.ID
	messageID := update.Message.ID

	for _, u := range users {
		if aud == BroadcastAudienceFree && u.IsPremiumActive() {
			continue
		}

		var err error
		if hasMedia {
			_, err = b.CopyMessage(ctx, &bot.CopyMessageParams{
				ChatID:     u.TGID,
				FromChatID: fromChatID,
				MessageID:  messageID,
				Caption:    text,
				ParseMode:  models.ParseModeHTML,
			})
		} else {
			// Сохраняем форматирование: при наличии Entities передаём их (и не задаём ParseMode, иначе сервер их игнорирует)
			params := &bot.SendMessageParams{ChatID: u.TGID, Text: text}
			if len(update.Message.Entities) > 0 {
				params.Entities = update.Message.Entities
			} else if text == update.Message.Caption && len(update.Message.CaptionEntities) > 0 {
				params.Entities = update.Message.CaptionEntities
			} else {
				params.ParseMode = models.ParseModeHTML
			}
			_, err = b.SendMessage(ctx, params)
		}
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

// DefaultHandler вызывается, если ни один обработчик не сработал (для broadcast, ad compose и т.д.)
func (h *BotHandler) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	cid := chatID(update)
	if !h.isAdmin(cid) {
		return
	}
	// Команды не обрабатываем здесь (кроме /cancel — у него свой обработчик)
	if strings.HasPrefix(update.Message.Text, "/") {
		return
	}
	if h.broadcastState.IsAwaiting(cid) {
		hasText := update.Message.Text != ""
		hasCaption := update.Message.Caption != ""
		hasMedia := len(update.Message.Photo) > 0 || update.Message.Video != nil || update.Message.Document != nil
		if hasText || hasCaption || hasMedia {
			h.HandleBroadcastMessage(ctx, b, update)
		}
		return
	}
	if d := h.adComposeState.Get(cid); d != nil && d.Step != AdComposeIdle {
		h.HandleAdComposeMessage(ctx, b, update)
		return
	}
	if h.isOPAwaiting(cid) {
		h.HandleOPChannelInput(ctx, b, update)
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
		h.sendText(ctx, b, update, "Нет прокси. Добавьте Free-прокси: /addproxy <ip> <port> <secret> Free")
		return
	}
	var sb strings.Builder
	sb.WriteString("📋 Прокси (удаление: /delproxy <id>):\n\n")
	for _, p := range proxies {
		sb.WriteString(formatProxyLine(p, false) + "\n")
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
	err := h.userUC.ActivatePremium(tgID, days)
	if err != nil {
		if errors.Is(err, usecase.ErrPremiumProxyCreationFailed) {
			h.sendText(ctx, b, update, fmt.Sprintf("✅ Премиум выдан пользователю %d на %d дн. Прокси не создан после 3 попыток — пользователь может нажать «Получить Premium proxy» в боте.", tgID, days))
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: tgID, ParseMode: models.ParseModeHTML,
				Text: "✅ Премиум активирован. Нажмите «Получить Premium proxy» в меню для создания персонального прокси.",
			})
			return
		}
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

// HandleSetPricing обновляет количество дней премиума: /setpricing <дней>
func (h *BotHandler) HandleSetPricing(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setpricing <дней>\nНапример: /setpricing 30")
		return
	}
	days, err := strconv.Atoi(args[1])
	if err != nil || days < 1 || days > 365 {
		h.sendText(ctx, b, update, "❌ Неверное значение дней (1–365).")
		return
	}
	if err := h.settingsRepo.Set("premium_days", fmt.Sprintf("%d", days)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить настройки.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Премиум: %d дней за платёж.", days))
}

// HandleSetPriceUSDT обновляет цену в USDT: /setprice_usdt <сумма>
func (h *BotHandler) HandleSetPriceUSDT(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setprice_usdt <сумма>\nНапример: /setprice_usdt 10")
		return
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(args[1], ",", "."), 64)
	if err != nil || f < 0.01 {
		h.sendText(ctx, b, update, "❌ Введите сумму не менее 0.01 USDT.")
		return
	}
	if err := h.settingsRepo.Set("premium_usdt", fmt.Sprintf("%.2f", f)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена премиума: %.2f USDT.", f))
}

// HandleSetPriceStars обновляет цену в Stars (XTR): /setprice_stars <звёзды>
func (h *BotHandler) HandleSetPriceStars(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setprice_stars <звёзды>\nНапример: /setprice_stars 100")
		return
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		h.sendText(ctx, b, update, "❌ Введите число звёзд (XTR) не менее 1.")
		return
	}
	if err := h.settingsRepo.Set("premium_stars", fmt.Sprintf("%d", n)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена премиума: %d Stars (XTR).", n))
}

// HandleSendAd открывает меню объявлений (Добавить/Редактировать/Снять). Рассылка выполняется автоматически при сохранении объявления.
func (h *BotHandler) HandleSendAd(ctx context.Context, b *bot.Bot, update *models.Update) {
	chatID := chatID(update)
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, ParseMode: models.ParseModeHTML,
		Text: "📣 <b>Объявления</b>\n\nВыберите действие:",
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "➕ Добавить объявление", CallbackData: "mgr_ad_add"}},
				{{Text: "✏️ Редактировать объявление", CallbackData: "mgr_ad_edit"}},
				{{Text: "📴 Снять объявление", CallbackData: "mgr_ad_deactivate"}},
				{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
			},
		},
	})
}

// HandleAdComposeMessage обрабатывает пошаговый ввод объявления (текст → канал → кнопка → часы)
func (h *BotHandler) HandleAdComposeMessage(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}
	adminID := chatID(update)
	d := h.adComposeState.Get(adminID)
	if d == nil || d.Step == AdComposeIdle {
		return
	}
	text := strings.TrimSpace(update.Message.Text)
	send := func(msg string) {
		b.SendMessage(ctx, &bot.SendMessageParams{ChatID: adminID, Text: msg, ParseMode: models.ParseModeHTML})
	}

	switch d.Step {
	case AdComposeText:
		d.Text = text
		d.Step = AdComposeChannel
		h.adComposeState.Set(adminID, d)
		send("Введите ссылку на канал (например <code>https://t.me/channel</code> или <code>@channel</code>):")
	case AdComposeChannel:
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, "@") {
			d.ChannelUsername = strings.TrimPrefix(text, "@")
			d.ChannelLink = "https://t.me/" + d.ChannelUsername
		} else {
			d.ChannelLink = text
			if d.ChannelLink != "" && !strings.HasPrefix(d.ChannelLink, "http") {
				d.ChannelLink = "https://t.me/" + strings.TrimPrefix(strings.TrimPrefix(text, "t.me/"), "/")
			}
			// Извлекаем username из ссылки (https://t.me/channel или t.me/channel)
			if d.ChannelLink != "" {
				if idx := strings.Index(d.ChannelLink, "t.me/"); idx != -1 {
					rest := d.ChannelLink[idx+5:]
					if end := strings.IndexAny(rest, "?/#"); end != -1 {
						rest = rest[:end]
					}
					rest = strings.Trim(rest, "/")
					if rest != "" {
						d.ChannelUsername = rest
					}
				}
			}
		}
		d.Step = AdComposeButton
		h.adComposeState.Set(adminID, d)
		send("Введите текст кнопки и URL через пробел (например: Подписаться https://t.me/channel). Или отправьте <code>-</code>, чтобы кнопки не было.")
	case AdComposeButton:
		if text == "-" {
			d.ButtonText = ""
			d.ButtonURL = ""
		} else {
			parts := strings.Fields(text)
			if len(parts) >= 2 {
				d.ButtonText = parts[0]
				d.ButtonURL = strings.Join(parts[1:], " ")
				if !strings.HasPrefix(d.ButtonURL, "http") {
					d.ButtonURL = "https://" + d.ButtonURL
				}
			} else {
				send("Нужны текст и URL через пробел. Или отправьте -")
				return
			}
		}
		d.Step = AdComposeHours
		h.adComposeState.Set(adminID, d)
		send("Введите время показа в часах (например <code>24</code> или <code>168</code> для недели):")
	case AdComposeHours:
		hours, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || hours < 1 {
			send("❌ Введите число часов (1 и больше).")
			return
		}
		expiresAt := time.Now().Add(time.Duration(hours) * time.Hour)
		ad := &domain.Ad{
			Text: d.Text, ChannelLink: d.ChannelLink, ChannelUsername: d.ChannelUsername,
			ExpiresAt: &expiresAt, Active: true,
		}
		if d.ButtonText != "" {
			ad.ButtonText = &d.ButtonText
		}
		if d.ButtonURL != "" {
			ad.ButtonURL = &d.ButtonURL
		}
		if d.EditingID == 0 {
			// Снимаем предыдущее активное объявление с закрепа у пользователей до деактивации в БД
			if prevAd, _ := h.adRepo.GetActiveOne(); prevAd != nil && h.adPinRepo != nil {
				pins, _ := h.adPinRepo.ListByAdID(prevAd.ID)
				for _, pin := range pins {
					_, _ = b.UnpinChatMessage(ctx, &bot.UnpinChatMessageParams{ChatID: pin.ChatID, MessageID: pin.MessageID})
					time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
				}
				_ = h.adPinRepo.DeleteByAdID(prevAd.ID)
			}
			if err := h.adRepo.DeactivateAll(); err != nil {
				send("❌ Ошибка деактивации предыдущих объявлений")
				h.adComposeState.Clear(adminID)
				return
			}
			if err := h.adRepo.Create(ad); err != nil {
				send("❌ Ошибка создания объявления: " + err.Error())
				h.adComposeState.Clear(adminID)
				return
			}
		} else {
			ad.ID = d.EditingID
			if err := h.adRepo.Update(ad); err != nil {
				send("❌ Ошибка обновления: " + err.Error())
				h.adComposeState.Clear(adminID)
				return
			}
			// При редактировании снимаем старые закрепления перед рассылкой нового текста
			if h.adPinRepo != nil {
				pins, _ := h.adPinRepo.ListByAdID(ad.ID)
				for _, pin := range pins {
					_, _ = b.UnpinChatMessage(ctx, &bot.UnpinChatMessageParams{ChatID: pin.ChatID, MessageID: pin.MessageID})
					time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
				}
				_ = h.adPinRepo.DeleteByAdID(ad.ID)
			}
		}
		// Рассылка только бесплатным пользователям с закреплением (один раз при сохранении)
		var successMsg string
		users, errUsers := h.userRepo.GetAll()
		if errUsers == nil && h.adPinRepo != nil {
			sent := 0
			kb := h.buildAdKeyboard(ad)
			for _, u := range users {
				if u.IsPremiumActive() {
					continue
				}
				msg, errSend := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: u.TGID, Text: ad.Text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
				})
				if errSend == nil && msg != nil && msg.ID != 0 {
					if _, errPin := b.PinChatMessage(ctx, &bot.PinChatMessageParams{ChatID: u.TGID, MessageID: msg.ID}); errPin == nil {
						_ = h.adPinRepo.Create(&domain.AdPin{AdID: ad.ID, UserID: u.TGID, ChatID: u.TGID, MessageID: msg.ID})
						sent++
					}
				}
				time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
			}
			if d.EditingID == 0 {
				successMsg = fmt.Sprintf("✅ Объявление добавлено и активировано. Разослано и закреплено у %d бесплатных пользователей.", sent)
			} else {
				successMsg = fmt.Sprintf("✅ Объявление обновлено. Разослано и закреплено у %d бесплатных пользователей.", sent)
			}
		} else {
			if d.EditingID == 0 {
				successMsg = "✅ Объявление добавлено и активировано."
			} else {
				successMsg = "✅ Объявление обновлено."
			}
			if errUsers != nil {
				successMsg += fmt.Sprintf(" Рассылка не выполнена: %v", errUsers)
			} else if h.adPinRepo == nil {
				successMsg += " Рассылка не выполнена (недоступен репозиторий закреплений)."
			}
		}
		send(successMsg)
		h.adComposeState.Clear(adminID)
	}
}

// HandleOPChannelInput обрабатывает ввод канала ОП (после нажатия «Добавить канал»)
func (h *BotHandler) HandleOPChannelInput(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}
	adminID := chatID(update)
	if !h.isOPAwaiting(adminID) {
		return
	}
	raw := strings.TrimSpace(update.Message.Text)
	if raw == "" {
		h.sendText(ctx, b, update, "❌ Введите @username или ссылку на канал. Отмена: /cancel")
		return
	}
	// Нормализуем: сохраняем как есть или добавляем @
	ch := raw
	if !strings.HasPrefix(ch, "@") && !strings.HasPrefix(ch, "http") && !strings.HasPrefix(ch, "t.me/") {
		ch = "@" + ch
	}
	if strings.HasPrefix(ch, "t.me/") {
		ch = "https://t.me/" + strings.TrimPrefix(ch, "t.me/")
	}
	channels := h.getForcedSubChannels()
	for _, c := range channels {
		if strings.EqualFold(channelToChatID(c), channelToChatID(ch)) {
			h.setOPAwaiting(adminID, false)
			h.sendText(ctx, b, update, "⚠️ Этот канал уже в списке ОП.")
			return
		}
	}
	channels = append(channels, ch)
	if err := h.setForcedSubChannels(channels); err != nil {
		h.setOPAwaiting(adminID, false)
		h.sendText(ctx, b, update, "❌ Ошибка сохранения.")
		return
	}
	h.setOPAwaiting(adminID, false)
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Канал добавлен в ОП: %s", ch))
}

// HandleCancel отмена ввода рассылки, диалога объявления или ввода канала ОП
func (h *BotHandler) HandleCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	adminID := chatID(update)
	cancelled := false
	if h.broadcastState.IsAwaiting(adminID) {
		h.broadcastState.Clear(adminID)
		cancelled = true
	}
	if h.adComposeState.Get(adminID) != nil && h.adComposeState.Get(adminID).Step != AdComposeIdle {
		h.adComposeState.Clear(adminID)
		cancelled = true
	}
	if h.isOPAwaiting(adminID) {
		h.setOPAwaiting(adminID, false)
		cancelled = true
	}
	if cancelled {
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
	case "get_extra_proxy":
		// Та же логика, что и «Получить прокси» (кнопка могла остаться в старых сообщениях).
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleGetProxy(ctx, b, update)
	case "my_proxies":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		user, errMy := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
		if errMy != nil || user == nil || h.userProxyRepo == nil {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Ошибка. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		list, errList := h.userProxyRepo.ListByUserID(user.ID)
		if errList != nil || len(list) == 0 {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, Text: "У вас пока нет сохранённых прокси. Получите прокси через «Получить proxy».", ParseMode: models.ParseModeHTML,
			})
			return
		}
		text := "📋 <b>Ваши прокси</b> (нажмите для подключения):"
		var rows [][]models.InlineKeyboardButton
		for i, up := range list {
			label := fmt.Sprintf("🌐 %s:%d", up.IP, up.Port)
			if len(label) > 64 {
				label = fmt.Sprintf("Прокси %d", i+1)
			}
			rows = append(rows, []models.InlineKeyboardButton{{Text: label, CallbackData: "my_proxy_" + strconv.FormatUint(uint64(up.ID), 10)}})
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML,
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: rows},
		})
	case "get_premium_proxy":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		user, err := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
		if err != nil || user == nil || !user.IsPremiumActive() {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Доступно только для премиум-пользователей.", ParseMode: models.ParseModeHTML})
			return
		}
		_, err = h.userUC.RetryPremiumProxyCreation(chatID)
		if err != nil {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Не удалось создать Premium proxy. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		h.sendProxyToUser(ctx, b, chatID, user, false)
	case "check_sub_forced":
		userID := update.CallbackQuery.From.ID
		user, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
		if err != nil {
			b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID, Text: "Ошибка. Попробуйте позже."})
			return
		}
		if !h.isSubscribedToForcedChannel(ctx, b, userID) {
			b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID, Text: "Подпишитесь на канал и нажмите «Проверить подписку» снова."})
			return
		}
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID, Text: "✅ Спасибо! Выдаём прокси."})
		if !user.ForcedSubCounted && h.settingsRepo != nil {
			user.ForcedSubCounted = true
			_ = h.userRepo.Update(user)
			if cur, _ := h.settingsRepo.Get("forced_subs_count"); cur != "" {
				if n, err := strconv.Atoi(cur); err == nil {
					_ = h.settingsRepo.Set("forced_subs_count", strconv.Itoa(n+1))
				}
			} else {
				_ = h.settingsRepo.Set("forced_subs_count", "1")
			}
		}
		h.sendProxyToUser(ctx, b, chatID, user, true)
	case "buy_stars":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleBuyStars(ctx, b, update)
	case "cancel_payment":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "В меню",
		})
		user, err := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
		if err != nil {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Ошибка. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		msg, kb := h.mainMenuContent(user)
		b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
	case "reminder_later":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Ок, напомним позже",
		})
		return
	default:
		if strings.HasPrefix(data, "my_proxy_") {
			idStr := strings.TrimPrefix(data, "my_proxy_")
			upID, err := strconv.ParseUint(idStr, 10, 32)
			if err == nil && h.userProxyRepo != nil {
				user, _ := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
				if user != nil {
					up, _ := h.userProxyRepo.GetByID(uint(upID))
					if up != nil && up.UserID == user.ID {
						b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
						proxyURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", up.IP, up.Port, up.Secret)
						msg := fmt.Sprintf("✅ Подключение к прокси:\n\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>\n\nНажмите кнопку для настройки:",
							up.IP, up.Port, up.Secret)
						kb := &models.InlineKeyboardMarkup{
							InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "🔗 Подключиться", URL: proxyURL}}},
						}
						b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
						return
					}
				}
			}
		}
		// Клик по кнопке объявления (ad_click_ID) — считаем клик и открываем ссылку
		if strings.HasPrefix(data, "ad_click_") {
			idStr := strings.TrimPrefix(data, "ad_click_")
			adID, err := strconv.ParseUint(idStr, 10, 32)
			if err == nil {
				_ = h.adRepo.IncrementClicks(uint(adID))
				ad, _ := h.adRepo.GetByID(uint(adID))
				if ad != nil {
					openURL := ""
					if ad.ButtonURL != nil && *ad.ButtonURL != "" {
						openURL = *ad.ButtonURL
					} else if ad.ChannelLink != "" {
						openURL = channelToURL(ad.ChannelLink)
					}
					if openURL != "" {
						b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
							CallbackQueryID: cqID,
							URL:             openURL,
						})
						return
					}
				}
			}
		}
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Неизвестная команда",
		})
	}
}

// HandleBuyStars отправляет счёт на оплату Telegram Stars (XTR)
func (h *BotHandler) HandleBuyStars(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)
	_, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка. Попробуйте позже.")
		return
	}
	days := h.getPremiumDays()
	starsAmount := h.getPremiumStars()
	payload := fmt.Sprintf("premium_%d_%d", days, userID)
	link, err := b.CreateInvoiceLink(ctx, &bot.CreateInvoiceLinkParams{
		Title:       fmt.Sprintf("Premium %d дней", days),
		Description: fmt.Sprintf("Премиум подписка на %d дней — доступ к быстрым прокси", days),
		Payload:     payload,
		Currency:    "XTR",
		Prices:      []models.LabeledPrice{{Label: fmt.Sprintf("Premium %d дней", days), Amount: starsAmount}},
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
	if !strings.HasPrefix(payload, "premium_") {
		return
	}
	rest := strings.TrimPrefix(payload, "premium_")
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 {
		return
	}
	days, err1 := strconv.Atoi(parts[0])
	userID, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || days < 1 {
		return
	}
	_ = h.paymentUC.RecordStarPayment(userID, int64(sp.TotalAmount), sp.Currency, days, sp.TelegramPaymentChargeID)

	err := h.userUC.ActivatePremium(userID, days)
	if err != nil {
		if errors.Is(err, usecase.ErrPremiumProxyCreationFailed) {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID, ParseMode: models.ParseModeHTML,
				Text: fmt.Sprintf("✅ Оплата получена! Премиум на %d дн. активирован. Нажмите «Получить Premium proxy» в меню для создания персонального прокси.", days),
			})
			return
		}
		log.Printf("HandleSuccessfulPayment ActivatePremium tg_id=%d days=%d: %v", userID, days, err)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID, Text: "❌ Временная ошибка при активации. Попробуйте позже или обратитесь в поддержку.",
		})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   fmt.Sprintf("✅ Оплата получена! Премиум на %d дн. активирован.", days),
	})
}