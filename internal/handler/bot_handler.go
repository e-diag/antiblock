package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

const broadcastDelayMs = 50 // ~20 сообщений в секунду (лимит Telegram ~30/сек)

const proxiesPerPage = 10

// VPSSetupStep — состояние пошагового диалога создания Premium VPS.
type VPSSetupStep int

const (
	VPSSetupIdle VPSSetupStep = iota
	VPSSetupName
	VPSSetupRegion
	VPSSetupConfig
	VPSSetupConfirm
)

type VPSSetupData struct {
	Step VPSSetupStep

	RequestID uint

	// Вводится менеджером
	Name string

	Region    string
	OSImageID string
	ConfigID  int

	Processing bool // защита от двойного нажатия «Создать VPS»
}

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

// buildManagerProxiesPage возвращает текст и клавиатуру для страницы списка прокси.
// proxyType: "free", "premium", "pro", "all"
func (h *BotHandler) buildManagerProxiesPage(proxyType string, page int) (string, *models.InlineKeyboardMarkup, error) {
	if proxyType == "pro" {
		if h.proUC == nil {
			return "⚡ <b>Pro-прокси</b>\n\nPro не настроен.", &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{
						{Text: markActive("🆓", false), CallbackData: "mgr_proxies_free_0"},
						{Text: markActive("⚡ Pro", true), CallbackData: "mgr_proxies_pro_0"},
						{Text: markActive("💎", false), CallbackData: "mgr_proxies_premium_0"},
						{Text: markActive("Все", false), CallbackData: "mgr_proxies_all_0"},
					},
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			}, nil
		}
		groups, err := h.proUC.GetActiveGroups()
		if err != nil {
			return "", nil, err
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("⚡ <b>Pro-прокси</b> (активных групп: %d)\n\n", len(groups)))
		if len(groups) == 0 {
			sb.WriteString("Активных Pro-групп пока нет.\n")
		} else {
			for _, g := range groups {
				sb.WriteString(fmt.Sprintf(
					"• group <code>%d</code> (%s)\n  DD: <code>%s:%d</code>\n  EE: <code>%s:%d</code>\n",
					g.ID,
					g.Date.UTC().Format("2006-01-02"),
					g.ServerIP, g.PortDD,
					g.ServerIP, g.PortEE,
				))
			}
		}
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: markActive("🆓", false), CallbackData: "mgr_proxies_free_0"},
					{Text: markActive("⚡ Pro", true), CallbackData: "mgr_proxies_pro_0"},
					{Text: markActive("💎", false), CallbackData: "mgr_proxies_premium_0"},
					{Text: markActive("Все", false), CallbackData: "mgr_proxies_all_0"},
				},
				{
					{Text: "🔄 Обновить", CallbackData: "mgr_proxies_pro_0"},
					{Text: "◀️ Назад", CallbackData: "mgr_back"},
				},
			},
		}
		return sb.String(), kb, nil
	}

	proxies, err := h.proxyUC.GetAll()
	if err != nil {
		return "", nil, err
	}

	// Фильтрация
	var filtered []*domain.ProxyNode
	for _, p := range proxies {
		switch proxyType {
		case "free":
			if p.Type == domain.ProxyTypeFree {
				filtered = append(filtered, p)
			}
		case "premium":
			if p.Type == domain.ProxyTypePremium {
				filtered = append(filtered, p)
			}
		case "pro":
			// Чтобы не зависеть от наличия константы domain.ProxyTypePro до миграций,
			// сравниваем по строковому значению.
			if string(p.Type) == "pro" {
				filtered = append(filtered, p)
			}
		default: // "all"
			filtered = append(filtered, p)
		}
	}

	total := len(filtered)
	totalPages := (total + proxiesPerPage - 1) / proxiesPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * proxiesPerPage
	end := start + proxiesPerPage
	if end > total {
		end = total
	}
	pageItems := filtered[start:end]

	var sb strings.Builder
	typeLabel := map[string]string{
		"free": "🆓 Free", "premium": "💎 Premium", "pro": "⚡ Pro", "all": "Все",
	}[proxyType]
	if typeLabel == "" {
		typeLabel = "Все"
		proxyType = "all"
	}
	sb.WriteString(fmt.Sprintf("📋 <b>Прокси — %s</b> (стр. %d/%d, всего: %d)\n\n", typeLabel, page+1, totalPages, total))

	for _, p := range pageItems {
		status := "✅"
		if p.Status != domain.ProxyStatusActive {
			status = "🔴"
		}
		line := fmt.Sprintf("%s ID %d: %s:%d [%s]", status, p.ID, p.IP, p.Port, p.Type)
		if p.Type == domain.ProxyTypeFree {
			line += fmt.Sprintf(" Load:%d", p.Load)
		}
		if p.UnreachableSince != nil {
			line += " ⚠️недоступен"
		}
		if p.LastRTTMs != nil {
			line += fmt.Sprintf(" RTT:%dмс", *p.LastRTTMs)
		}
		if p.LastCheck != nil {
			line += fmt.Sprintf(" (%s)", p.LastCheck.Format("15:04"))
		}
		sb.WriteString("• " + line + "\n")
	}

	// Клавиатура: фильтры + пагинация + назад
	var rows [][]models.InlineKeyboardButton

	// Строка фильтров
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: markActive("🆓", proxyType == "free"), CallbackData: "mgr_proxies_free_0"},
		{Text: markActive("⚡ Pro", proxyType == "pro"), CallbackData: "mgr_proxies_pro_0"},
		{Text: markActive("💎", proxyType == "premium"), CallbackData: "mgr_proxies_premium_0"},
		{Text: markActive("Все", proxyType == "all"), CallbackData: "mgr_proxies_all_0"},
	})

	// Строка пагинации
	var navRow []models.InlineKeyboardButton
	if page > 0 {
		navRow = append(navRow, models.InlineKeyboardButton{
			Text: "◀️", CallbackData: fmt.Sprintf("mgr_proxies_%s_%d", proxyType, page-1),
		})
	}
	navRow = append(navRow, models.InlineKeyboardButton{
		Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: "mgr_noop",
	})
	if page < totalPages-1 {
		navRow = append(navRow, models.InlineKeyboardButton{
			Text: "▶️", CallbackData: fmt.Sprintf("mgr_proxies_%s_%d", proxyType, page+1),
		})
	}
	rows = append(rows, navRow)
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "🔄 Обновить", CallbackData: fmt.Sprintf("mgr_proxies_%s_%d", proxyType, page)},
		{Text: "◀️ Назад", CallbackData: "mgr_back"},
	})

	return sb.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// markActive добавляет ✓ к тексту кнопки если active == true
func markActive(text string, active bool) string {
	if active {
		return "✓ " + text
	}
	return text
}

// BotHandler обрабатывает команды бота
type BotHandler struct {
	userUC              usecase.UserUseCase
	proxyUC             usecase.ProxyUseCase
	proUC               usecase.ProUseCase
	paymentUC           usecase.PaymentUseCase
	userRepo            repository.UserRepository
	userProxyRepo       repository.UserProxyRepository
	proxyRepo           repository.ProxyRepository
	adRepo              repository.AdRepository
	adPinRepo           repository.AdPinRepository
	settingsRepo        repository.SettingsRepository
	opStatsRepo         repository.OPStatsRepository
	maintenanceWaitRepo repository.MaintenanceWaitRepository

	// Premium provisioning через TimeWeb (floating IP + docker запуск по SSH).
	twClient           *timeweb.Client
	premiumProvisioner *usecase.PremiumProvisioner
	vpsReqRepo         repository.VPSProvisionRequestRepository
	premiumServerRepo  repository.PremiumServerRepository
	sshKeyPath         string
	premiumServerOSID  int // 0 = авто Ubuntu 24.04 из TimeWeb /os/servers

	proDockerMgr *docker.Manager
	proServerIP  string
	forcedSubCh  string // из конфига (fallback, если в БД пусто)

	broadcastState      *BroadcastState
	broadcastMediaGroup *BroadcastMediaGroupBuffer
	adComposeState      *AdComposeState
	msgState            *MessageState
	opAwaiting          map[int64]bool // админ вводит новый канал ОП
	opMu                sync.Mutex
	instrAwaitingText   map[int64]bool
	instrAwaitingPhoto  map[int64]bool
	instrMu             sync.Mutex
	adminIDs            []int64
	botRef              *bot.Bot // для асинхронной рассылки альбомов (устанавливается из main)
	broadcastSem        chan struct{}

	vpsSetupSteps map[int64]*VPSSetupData
	vpsSetupMu    sync.Mutex
}

// NewBotHandler создает новый обработчик бота
func NewBotHandler(
	userUC usecase.UserUseCase,
	proxyUC usecase.ProxyUseCase,
	proUC usecase.ProUseCase,
	paymentUC usecase.PaymentUseCase,
	userRepo repository.UserRepository,
	userProxyRepo repository.UserProxyRepository,
	proxyRepo repository.ProxyRepository,
	adRepo repository.AdRepository,
	adPinRepo repository.AdPinRepository,
	settingsRepo repository.SettingsRepository,
	opStatsRepo repository.OPStatsRepository,
	maintenanceWaitRepo repository.MaintenanceWaitRepository,
	proDockerMgr *docker.Manager,
	proServerIP string,
	forcedSubCh string,
	broadcastState *BroadcastState,
	broadcastMediaGroup *BroadcastMediaGroupBuffer,
	adComposeState *AdComposeState,
	twClient *timeweb.Client,
	premiumProvisioner *usecase.PremiumProvisioner,
	vpsReqRepo repository.VPSProvisionRequestRepository,
	premiumServerRepo repository.PremiumServerRepository,
	sshKeyPath string,
	premiumServerOSID int,
	adminIDs []int64,
) *BotHandler {
	return &BotHandler{
		userUC:              userUC,
		proxyUC:             proxyUC,
		proUC:               proUC,
		paymentUC:           paymentUC,
		userRepo:            userRepo,
		userProxyRepo:       userProxyRepo,
		proxyRepo:           proxyRepo,
		adRepo:              adRepo,
		adPinRepo:           adPinRepo,
		settingsRepo:        settingsRepo,
		opStatsRepo:         opStatsRepo,
		maintenanceWaitRepo: maintenanceWaitRepo,
		proDockerMgr:        proDockerMgr,
		proServerIP:         proServerIP,
		forcedSubCh:         forcedSubCh,
		broadcastState:      broadcastState,
		broadcastMediaGroup: broadcastMediaGroup,
		adComposeState:      adComposeState,
		msgState:            NewMessageState(),
		opAwaiting:          make(map[int64]bool),
		instrAwaitingText:   make(map[int64]bool),
		instrAwaitingPhoto:  make(map[int64]bool),
		twClient:            twClient,
		premiumProvisioner:  premiumProvisioner,
		vpsReqRepo:          vpsReqRepo,
		premiumServerRepo:   premiumServerRepo,
		sshKeyPath:          sshKeyPath,
		premiumServerOSID:   premiumServerOSID,
		vpsSetupSteps:       make(map[int64]*VPSSetupData),
		adminIDs:            adminIDs,
		broadcastSem:        make(chan struct{}, 1),
	}
}

func (h *BotHandler) getVPSSetup(adminID int64) *VPSSetupData {
	h.vpsSetupMu.Lock()
	defer h.vpsSetupMu.Unlock()
	return h.vpsSetupSteps[adminID]
}

func (h *BotHandler) setVPSSetup(adminID int64, d *VPSSetupData) {
	h.vpsSetupMu.Lock()
	defer h.vpsSetupMu.Unlock()
	h.vpsSetupSteps[adminID] = d
}

func (h *BotHandler) clearVPSSetup(adminID int64) {
	h.vpsSetupMu.Lock()
	defer h.vpsSetupMu.Unlock()
	delete(h.vpsSetupSteps, adminID)
}

func (h *BotHandler) activateProAndSend(ctx context.Context, b *bot.Bot, tgID int64, days int) error {
	if h.proUC == nil || h.userRepo == nil {
		return fmt.Errorf("pro is not configured")
	}
	if h.proServerIP == "" {
		return fmt.Errorf("pro server ip is not configured")
	}
	user, err := h.userRepo.GetByTGID(tgID)
	if err != nil || user == nil {
		return fmt.Errorf("user not found")
	}

	// days=0 — только показать текущие Pro-прокси без активации/продления.
	if days == 0 {
		sub, err := h.proUC.GetActiveSubscription(user.ID)
		if err != nil || sub == nil {
			return fmt.Errorf("no active pro subscription")
		}
		return h.sendProGroupProxies(ctx, b, tgID, sub.ProGroupID)
	}

	cycle := h.getProDays()
	if cycle < 1 {
		cycle = 30
	}
	group, extendedOnly, err := h.proUC.ActivateProSubscription(user, days, h.proServerIP, h.proDockerMgr, cycle)
	if err != nil {
		return err
	}
	if extendedOnly {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: tgID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf("✅ <b>Pro продлён</b> на %d дн.\n\nТекущие прокси не меняются. Раз в <b>%d</b> дн. на сервере ключи обновляются — тогда вы получите <b>новые</b> данные в этом чате.", days, cycle),
		})
		return nil
	}
	h.SendProGroupProxiesToUser(ctx, b, tgID, group)
	return nil
}

// sendProGroupProxies отправляет dd+ee прокси по ID группы.
func (h *BotHandler) sendProGroupProxies(ctx context.Context, b *bot.Bot, tgID int64, groupID uint) error {
	if h.proUC == nil {
		return fmt.Errorf("pro is not configured")
	}
	group, err := h.proUC.GetGroupByID(groupID)
	if err != nil || group == nil {
		return fmt.Errorf("pro group not found")
	}
	h.SendProGroupProxiesToUser(ctx, b, tgID, group)
	return nil
}

// SendProGroupProxiesToUser отправляет два сообщения с Pro-прокси (dd + ee).
func (h *BotHandler) SendProGroupProxiesToUser(ctx context.Context, b *bot.Bot, tgID int64, group *domain.ProGroup) {
	if group == nil {
		return
	}
	ddURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", group.ServerIP, group.PortDD, group.SecretDD)
	msgDD := fmt.Sprintf("⚡ <b>Ваш Pro proxy (стандартный dd)</b>\n\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>",
		group.ServerIP, group.PortDD, group.SecretDD)
	kbDD := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔗 Подключиться (dd)", URL: ddURL}},
		},
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: tgID, Text: msgDD, ParseMode: models.ParseModeHTML, ReplyMarkup: kbDD,
	})

	eeURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", group.ServerIP, group.PortEE, group.SecretEE)
	msgEE := fmt.Sprintf("🛡 <b>Ваш Pro proxy (маскировка ee/fake-TLS)</b>\n\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>\n\n<i>Используйте если стандартный заблокирован</i>",
		group.ServerIP, group.PortEE, group.SecretEE)
	kbEE := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔗 Подключиться (ee)", URL: eeURL}},
		},
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: tgID, Text: msgEE, ParseMode: models.ParseModeHTML, ReplyMarkup: kbEE,
	})
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

func (h *BotHandler) setInstrAwaitingText(adminID int64, v bool) {
	h.instrMu.Lock()
	defer h.instrMu.Unlock()
	if v {
		h.instrAwaitingText[adminID] = true
	} else {
		delete(h.instrAwaitingText, adminID)
	}
}
func (h *BotHandler) setInstrAwaitingPhoto(adminID int64, v bool) {
	h.instrMu.Lock()
	defer h.instrMu.Unlock()
	if v {
		h.instrAwaitingPhoto[adminID] = true
	} else {
		delete(h.instrAwaitingPhoto, adminID)
	}
}
func (h *BotHandler) isInstrAwaitingText(id int64) bool {
	h.instrMu.Lock()
	defer h.instrMu.Unlock()
	return h.instrAwaitingText[id]
}
func (h *BotHandler) isInstrAwaitingPhoto(id int64) bool {
	h.instrMu.Lock()
	defer h.instrMu.Unlock()
	return h.instrAwaitingPhoto[id]
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
		ChatID:      chatID(update),
		Text:        text,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: replyMarkup,
	}
	b.SendMessage(ctx, params)
}

// sendOrEdit редактирует последнее сообщение бота пользователю, если есть сохранённый messageID.
// При неудаче отправляет новое. НЕ использовать для сообщений с прокси и объявлений.
func (h *BotHandler) sendOrEdit(ctx context.Context, b *bot.Bot, userID int64, text string, replyMarkup *models.InlineKeyboardMarkup) {
	if msgID, ok := h.msgState.Get(userID); ok {
		_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      userID,
			MessageID:   msgID,
			Text:        text,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: replyMarkup,
		})
		if err == nil {
			return
		}
		h.msgState.Clear(userID)
	}
	msg, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      userID,
		Text:        text,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: replyMarkup,
	})
	if err == nil && msg != nil {
		h.msgState.Set(userID, msg.ID)
	}
}

func (h *BotHandler) sendText(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	h.send(ctx, b, update, text, nil)
}

// mainMenuContent возвращает текст и клавиатуру главного меню пользователя (для /start и кнопки «Назад»).
func (h *BotHandler) mainMenuContent(user *domain.User) (welcomeMsg string, kb *models.InlineKeyboardMarkup) {
	welcomeMsg = "👋 <b>Добро пожаловать в eDiag Proxy Bot!</b>\n\n"
	welcomeMsg += "🔐 Безопасный доступ к Telegram через MTProto-прокси.\n\n"
	welcomeMsg += "Нажми: <b>«Получить proxy»</b> → <b>«Подключиться»</b> → <b>«Включить»</b> — и Telegram работает без замедлений!\n\n"

	hasPremium := user.IsPremiumActive()
	hasPro := false
	var proSub *domain.ProSubscription
	if h.proUC != nil {
		proSub, _ = h.proUC.GetActiveSubscription(user.ID)
		hasPro = proSub != nil
	}

	if hasPremium {
		premiumUntil := "неограниченно"
		if user.PremiumUntil != nil {
			premiumUntil = user.PremiumUntil.Format("02.01.2006 15:04")
		}
		welcomeMsg += fmt.Sprintf("💎 Premium активен до: <b>%s</b>\n", premiumUntil)
	}
	if hasPro && proSub != nil {
		welcomeMsg += fmt.Sprintf("⚡ Pro активен до: <b>%s</b>\n", proSub.ExpiresAt.Format("02.01.2006 15:04"))
	}
	if hasPremium || hasPro {
		welcomeMsg += "\n"
	}
	welcomeMsg += "Выберите действие:"

	days := h.getPremiumDays()
	usdt := h.getPremiumUSDT()
	stars := h.getPremiumStars()
	proDays := h.getProDays()
	proUSDT := h.getProUSDT()
	proStars := h.getProStars()

	var rows [][]models.InlineKeyboardButton

	// Free-кнопка только если нет активных Pro и Premium.
	if !hasPremium && !hasPro {
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
		rows = append(rows, []models.InlineKeyboardButton{{Text: btnGetProxy, CallbackData: "get_proxy"}})
	}

	if !hasPro {
		btnPro := fmt.Sprintf("⚡ Купить Pro на %d дн. (%.2f TON / %d ⭐)", proDays, proUSDT, proStars)
		if len(btnPro) > 64 {
			btnPro = fmt.Sprintf("⚡ Купить Pro на %d дн.", proDays)
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: btnPro, CallbackData: "buy_pro"}})
	}
	if !hasPremium {
		btnPremium := fmt.Sprintf("💎 Купить Premium на %d дн. (%.2f TON / %d ⭐)", days, usdt, stars)
		if len(btnPremium) > 64 {
			btnPremium = fmt.Sprintf("💎 Купить Premium на %d дн.", days)
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: btnPremium, CallbackData: "buy_premium"}})
	}

	if hasPro {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "⚡ Получить Pro proxy (dd + ee)", CallbackData: "get_pro_proxy"}})
	}
	if hasPremium {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "🔐 Получить Premium proxy (dd + ee)", CallbackData: "get_premium_proxy"}})
	}

	if hasPro {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "⚡ Продлить Pro", CallbackData: "buy_pro"}})
	}
	if hasPremium {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "💎 Продлить Premium", CallbackData: "buy_premium"}})
	}

	rows = append(rows, []models.InlineKeyboardButton{{Text: "📋 Мои прокси", CallbackData: "my_proxies"}})
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "📖 Инструкция", CallbackData: "show_instructions"},
	})
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
	h.sendOrEdit(ctx, b, userID, welcomeMsg, kb)
	if !user.IsPremiumActive() {
		runCtx := context.WithoutCancel(ctx)
		go h.sendActiveAdIfExists(runCtx, b, userID)
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

// isValidChannelInput проверяет, что ввод похож на username канала или t.me-ссылку.
func isValidChannelInput(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "https://t.me/") || strings.HasPrefix(s, "t.me/") {
		return true
	}
	s = strings.TrimPrefix(s, "@")
	matched, _ := regexp.MatchString(`^[a-zA-Z0-9_]{4,64}$`, s)
	return matched
}

// channelToURL возвращает URL для кнопки «Подписаться»
func channelToURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http") {
		return s
	}
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimPrefix(s, "t.me/")
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
	var keyKind string
	if strings.HasPrefix(proxy.Secret, "dd") {
		keyKind = "dd"
	} else if strings.HasPrefix(proxy.Secret, "ee") {
		keyKind = "ee"
	}
	prefixLine := ""
	if proxy.Type == domain.ProxyTypeFree && keyKind != "" {
		prefixLine = fmt.Sprintf("🆓 <b>Free proxy:</b> %s\n\n", keyKind)
	}
	msg := fmt.Sprintf("✅ Ваш прокси-сервер:\n\n%s"+
		"🌐 IP: <code>%s</code>\n"+
		"🔌 Порт: <code>%d</code>\n"+
		"🔑 Секрет: <code>%s</code>\n\n"+
		"Нажмите на кнопку ниже для автоматической настройки:",
		prefixLine, proxy.IP, proxy.Port, proxy.Secret)
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔗 Подключиться", URL: proxyURL}},
		},
	}
	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
	if h.userProxyRepo != nil {
		existing, err := h.userProxyRepo.GetByUserIDAndProxy(user.ID, proxy.IP, proxy.Port, proxy.Secret)
		if err != nil {
			log.Printf("sendProxyToUser: dedupe check failed user_id=%d: %v", user.ID, err)
			return
		}
		if existing == nil {
			_ = h.userProxyRepo.Create(&domain.UserProxy{
				UserID:    user.ID,
				IP:        proxy.IP,
				Port:      proxy.Port,
				Secret:    proxy.Secret,
				ProxyType: proxy.Type,
			})
		}
	}
	if !user.IsPremiumActive() {
		runCtx := context.WithoutCancel(ctx)
		go h.sendActiveAdIfExists(runCtx, b, chatID)
	}
}

// sendPremiumProxyToUser отправляет пользователю 2 сообщения: dd (8443) и ee (443) для TimeWeb Premium.
// Для legacy Premium оставляем прежнюю схему портов: ee = ddPort + 10000.
func (h *BotHandler) SendPremiumProxyToUser(ctx context.Context, b *bot.Bot, chatID int64, user *domain.User, proxy *domain.ProxyNode) {
	if proxy == nil || user == nil {
		return
	}

	// Для legacy-юзера TimewebFloatingIPID = "" (и PremiumServerID обычно nil).
	isTimeweb := proxy.TimewebFloatingIPID != ""
	ddPort := proxy.Port
	eePort := proxy.Port + 10000
	if isTimeweb {
		ddPort = domain.PremiumPortDD
		eePort = domain.PremiumPortEE
	}

	ddURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", proxy.IP, ddPort, proxy.Secret)
	kbDD := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🔗 Подключиться (dd)", URL: ddURL}},
		},
	}
	msgDD := fmt.Sprintf("✅ <b>Ваш Premium proxy готов!</b>\n\n🔐 <b>Тип: стандартный (dd)</b>\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>\n\nНажмите для подключения:", proxy.IP, ddPort, proxy.Secret)

	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        msgDD,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kbDD,
	})

	// Новый Premium: отдаём ещё и ee (всегда dd+ee для новых).
	if isTimeweb && proxy.SecretEE != "" {
		eeURL := fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", proxy.IP, eePort, proxy.SecretEE)
		kbEE := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "🔗 Подключиться (ee)", URL: eeURL}},
			},
		}
		msgEE := fmt.Sprintf(
			"🛡 <b>Дополнительный proxy с маскировкой (ee/fake-TLS)</b>\n\n"+
				"🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>\n\n"+
				"<i>Используйте этот прокси если стандартный заблокирован</i>",
			proxy.IP, eePort, proxy.SecretEE,
		)

		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        msgEE,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: kbEE,
		})
	}

	// Сохраняем оба прокси в «Мои прокси», чтобы /replace_ip мог корректно работать.
	if h.userProxyRepo != nil {
		// dd
		if ddPort > 0 && proxy.Secret != "" {
			existingDD, _ := h.userProxyRepo.GetByUserIDAndProxy(user.ID, proxy.IP, ddPort, proxy.Secret)
			if existingDD == nil {
				_ = h.userProxyRepo.Create(&domain.UserProxy{
					UserID:    user.ID,
					IP:        proxy.IP,
					Port:      ddPort,
					Secret:    proxy.Secret,
					ProxyType: domain.ProxyTypePremium,
				})
			}
		}
		// ee
		if eePort > 0 && proxy.SecretEE != "" {
			existingEE, _ := h.userProxyRepo.GetByUserIDAndProxy(user.ID, proxy.IP, eePort, proxy.SecretEE)
			if existingEE == nil {
				_ = h.userProxyRepo.Create(&domain.UserProxy{
					UserID:    user.ID,
					IP:        proxy.IP,
					Port:      eePort,
					Secret:    proxy.SecretEE,
					ProxyType: domain.ProxyTypePremium,
				})
			}
		}
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

func (h *BotHandler) getProDays() int {
	if h.settingsRepo == nil {
		return 30
	}
	v, _ := h.settingsRepo.Get("pro_days")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 30
}

func (h *BotHandler) getProUSDT() float64 {
	if h.settingsRepo == nil {
		return 3
	}
	v, _ := h.settingsRepo.Get("pro_price_usdt")
	if f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64); err == nil && f > 0 {
		return f
	}
	return 3
}

func (h *BotHandler) getProStars() int {
	if h.settingsRepo == nil {
		return 50
	}
	v, _ := h.settingsRepo.Get("pro_price_stars")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 50
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

// HandleBuyPremium обрабатывает запрос на покупку премиума (оплата через Telegram Stars и/или TON через xRocket).
func (h *BotHandler) HandleBuyPremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)

	_, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
	if err != nil {
		h.sendText(ctx, b, update, "❌ Произошла ошибка. Попробуйте позже.")
		return
	}

	days := h.getPremiumDays()
	usdt := h.getPremiumUSDT()
	starsCount := h.getPremiumStars()
	msg := fmt.Sprintf("💎 <b>Premium</b> — получи персональный proxy на %d дн.\n\n"+
		"• Без рекламы и ограничений\n"+
		"• Высокий приоритет и стабильность\n\n"+
		"💰 Стоимость: <b>%.2f TON</b> или <b>%d ⭐ Stars</b>\n\nВыберите способ оплаты:", days, usdt, starsCount)

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "💵 TON (xRocket)", CallbackData: "buy_premium_usdt"}},
			{{Text: "⭐ Telegram Stars", CallbackData: "buy_stars"}},
			{{Text: "◀️ Назад", CallbackData: "cancel_payment"}},
		},
	}
	h.sendOrEdit(ctx, b, userID, msg, kb)
}

// HandleAddProxy обрабатывает команду /addproxy (только для админов). Разрешён только тип Free; Premium создаётся автоматически.
func (h *BotHandler) HandleAddProxy(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	log.Printf("[addproxy] command from chat %d", update.Message.Chat.ID)
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
		log.Printf("[addproxy] AddProxy error: %v", err)
		h.sendText(ctx, b, update, fmt.Sprintf("❌ Ошибка при добавлении прокси: %v", err))
		return
	}
	log.Printf("[addproxy] added Free proxy %s:%d", ip, port)
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
				{Text: h.maintenanceManagerButtonLabel(), CallbackData: "mgr_maintenance_menu"},
			},
			{
				{Text: "⚡ Pro-группы", CallbackData: "mgr_pro_groups"},
				{Text: "⚙️ Цена Pro", CallbackData: "mgr_pro_pricing"},
			},
			{
				{Text: "📖 Инструкция", CallbackData: "mgr_instruction"},
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

func (h *BotHandler) isMaintenanceMode() bool {
	if h.settingsRepo == nil {
		return false
	}
	v, _ := h.settingsRepo.Get("maintenance_mode")
	return v == "1" || strings.EqualFold(v, "true")
}

func (h *BotHandler) maintenanceManagerButtonLabel() string {
	if h.isMaintenanceMode() {
		return "🔧 Тех. работы: ВКЛ ⚠️"
	}
	return "🔧 Технические работы"
}

const maintenanceResumeUserMsg = "✅ Технические работы завершены. Бот снова доступен — можете пользоваться."

// runMaintenanceResumeBroadcast рассылает уведомление только пользователям из очереди техработ (задержка как у рассылки).
func (h *BotHandler) runMaintenanceResumeBroadcast(adminChatID int64, tgIDs []int64) {
	go func() {
		bg := context.Background()
		br := h.botRef
		if br == nil || h.maintenanceWaitRepo == nil {
			return
		}
		sent, failed := 0, 0
		for _, tid := range tgIDs {
			_, err := br.SendMessage(bg, &bot.SendMessageParams{ChatID: tid, Text: maintenanceResumeUserMsg})
			if err != nil {
				failed++
			} else {
				sent++
			}
			time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
		}
		_ = h.maintenanceWaitRepo.Clear()
		_, _ = br.SendMessage(bg, &bot.SendMessageParams{
			ChatID: adminChatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf("🔧 <b>Техработы:</b> уведомление о возобновлении — отправлено <b>%d</b>, ошибок <b>%d</b>.", sent, failed),
		})
	}()
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

	// Разбивка Legacy vs New Premium (TimeWeb).
	var legacyPremium, newPremium int
	if proxies, err := h.proxyUC.GetAll(); err == nil {
		for _, p := range proxies {
			if p == nil {
				continue
			}
			if p.Type == domain.ProxyTypePremium && p.Status == domain.ProxyStatusActive {
				if p.TimewebFloatingIPID == "" && (p.PremiumServerID == nil || *p.PremiumServerID == 0) {
					legacyPremium++
				} else {
					newPremium++
				}
			}
		}
	}

	msg += fmt.Sprintf(
		"🌐 <b>Прокси</b>\n🆓 Free: %d (активных: см. список)\n💎 Активных премиум: %d (Legacy: %d, New: %d)",
		stats.FreeProxies, stats.PremiumProxies, legacyPremium, newPremium,
	)
	if h.proUC != nil {
		if groups, err := h.proUC.GetActiveGroups(); err == nil {
			msg += fmt.Sprintf("\n⚡ Активных Pro-групп: %d (прокси-эндпоинтов: %d)", len(groups), len(groups)*2)
		}
	}
	if stats.UnreachablePremiumCount > 0 {
		msg += fmt.Sprintf(" <b>(!%d не работают)</b>", stats.UnreachablePremiumCount)
	}
	msg += "\n"
	ad, _ := h.adRepo.GetActiveOne()
	if ad != nil {
		msg += fmt.Sprintf("\n📣 <b>Объявление</b> (ID %d)\n👁 Показы: %d\n🖱 Клики: %d", ad.ID, ad.Impressions, ad.Clicks)
	}
	if h.opStatsRepo != nil {
		if clicksByChannel, err := h.opStatsRepo.GetClicksByChannel(); err == nil && len(clicksByChannel) > 0 {
			msg += "\n\n📢 <b>Статистика ОП по каналам</b>\n"
			channels := h.getForcedSubChannels()
			for _, ch := range channels {
				count := clicksByChannel[ch]
				label := channelToChatID(ch)
				msg += fmt.Sprintf("• %s: <b>%d</b>\n", label, count)
			}
		}
	} else if h.settingsRepo != nil {
		if forcedSubs, _ := h.settingsRepo.Get("forced_subs_count"); forcedSubs != "" {
			msg += fmt.Sprintf("\n\n📢 Подписок по ОП: %s", forcedSubs)
		}
	}
	return msg, nil
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
			MessageID:   msgObj.ID,
			Text:        msg,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: refreshKeyboardStats(),
		})

	case "mgr_proxies":
		text, kb, err := h.buildManagerProxiesPage("all", 0)
		if err != nil {
			send("❌ Ошибка списка прокси")
			return
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
		})

	case "mgr_refresh_proxies":
		msgObj := update.CallbackQuery.Message.Message
		if msgObj == nil {
			return
		}
		text, kb, err := h.buildManagerProxiesPage("all", 0)
		if err != nil {
			send("❌ Ошибка списка прокси")
			return
		}
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      msgObj.Chat.ID,
			MessageID:   msgObj.ID,
			Text:        text,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: kb,
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
			Text: fmt.Sprintf("⚙️ <b>Настройки подписки</b>\n\n📅 Дней: <b>%d</b>\n💵 TON: <b>%.2f</b>\n⭐ Stars (XTR): <b>%d</b>\n\nИзменить:\n<code>/setpricing &lt;дней&gt;</code>\n<code>/setprice_usdt &lt;сумма&gt;</code>\n<code>/setprice_stars &lt;звёзды&gt;</code>", days, usdt, stars),
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
			if h.opStatsRepo != nil {
				if clicksByChannel, err := h.opStatsRepo.GetClicksByChannel(); err == nil && len(clicksByChannel) > 0 {
					msg += "\n<b>📊 Статистика подписок:</b>\n"
					for _, ch := range channels {
						count := clicksByChannel[ch]
						label := channelToChatID(ch)
						msg += fmt.Sprintf("%s: <b>%d</b>\n", label, count)
					}
				}
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

	case "mgr_pro_pricing":
		days := h.getProDays()
		usdt := h.getProUSDT()
		stars := h.getProStars()
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf("⚙️ <b>Настройки Pro</b>\n\n📅 Дней: <b>%d</b>\n💵 TON: <b>%.2f</b>\n⭐ Stars: <b>%d</b>\n\nИзменить:\n<code>/setpro_days &lt;дней&gt;</code>\n<code>/setpro_price_usdt &lt;сумма&gt;</code>\n<code>/setpro_price_stars &lt;звёзды&gt;</code>", days, usdt, stars),
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "mgr_maintenance_menu":
		if h.settingsRepo == nil || h.maintenanceWaitRepo == nil {
			send("❌ Техработы: хранилище недоступно.")
			return
		}
		on := h.isMaintenanceMode()
		n, _ := h.maintenanceWaitRepo.Count()
		st := "выключен"
		if on {
			st = "<b>включён</b> — обычные пользователи не могут пользоваться ботом"
		}
		msg := fmt.Sprintf(
			"🔧 <b>Технические работы</b>\n\nСтатус: %s\nВ очереди на уведомление после отключения: <b>%d</b>\n\nПосле выключения сообщение получат <b>только</b> те, кто обращался к боту во время техработ. Задержка между сообщениями — как у рассылки (%d мс).",
			st, n, broadcastDelayMs,
		)
		var rows [][]models.InlineKeyboardButton
		if !on {
			rows = append(rows, []models.InlineKeyboardButton{{Text: "▶️ Включить", CallbackData: "mgr_maintenance_on"}})
		} else {
			rows = append(rows, []models.InlineKeyboardButton{{Text: "⏹ Выключить и уведомить", CallbackData: "mgr_maintenance_off"}})
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: "◀️ Назад", CallbackData: "mgr_back"}})
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML,
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: rows},
		})

	case "mgr_maintenance_on":
		if h.settingsRepo == nil || h.maintenanceWaitRepo == nil {
			send("❌ Техработы: хранилище недоступно.")
			return
		}
		_ = h.maintenanceWaitRepo.Clear()
		if err := h.settingsRepo.Set("maintenance_mode", "1"); err != nil {
			send("❌ Не удалось включить режим.")
			return
		}
		send("🔧 Режим технических работ <b>включён</b>.\n\nПользователи (кроме менеджеров) при любом обращении получают сообщение о техработах.")

	case "mgr_maintenance_off":
		if h.settingsRepo == nil || h.maintenanceWaitRepo == nil {
			send("❌ Техработы: хранилище недоступно.")
			return
		}
		ids, err := h.maintenanceWaitRepo.ListTGIDs()
		if err != nil {
			send("❌ Ошибка чтения очереди.")
			return
		}
		if err := h.settingsRepo.Set("maintenance_mode", "0"); err != nil {
			send("❌ Не удалось выключить режим.")
			return
		}
		if len(ids) == 0 {
			_ = h.maintenanceWaitRepo.Clear()
			send("✅ Режим выключен. Очередь пуста — рассылка не требуется.")
			return
		}
		sec := (len(ids) * broadcastDelayMs) / 1000
		if sec < 1 && len(ids) > 0 {
			sec = 1
		}
		send(fmt.Sprintf("✅ Режим выключен. Рассылка <b>%d</b> пользователям (~%d с). Отчёт придёт сюда.", len(ids), sec))
		h.runMaintenanceResumeBroadcast(chatID, ids)

	case "mgr_pro_groups":
		if h.proUC == nil {
			send("⚡ <b>Pro-группы</b>\n\nPro не настроен.")
			return
		}
		groups, err := h.proUC.GetActiveGroups()
		if err != nil {
			send("❌ Не удалось загрузить Pro-группы.")
			return
		}
		msg := "⚡ <b>Pro-группы</b>\n\n"
		if len(groups) == 0 {
			msg += "Активных групп пока нет."
		} else {
			msg += fmt.Sprintf("Активных групп: <b>%d</b>\n\n", len(groups))
			for _, g := range groups {
				subsCount, errCount := h.proUC.CountActiveSubscribersByGroup(g.ID)
				if errCount != nil {
					subsCount = 0
				}
				msg += fmt.Sprintf(
					"• ID <code>%d</code> | дата: <code>%s</code> | юзеров: <b>%d</b>\n  DD: <code>%s:%d</code>\n  EE: <code>%s:%d</code>\n  infra до: <code>%s</code>\n\n",
					g.ID,
					g.Date.UTC().Format("2006-01-02"),
					subsCount,
					g.ServerIP, g.PortDD,
					g.ServerIP, g.PortEE,
					g.InfrastructureExpiresAt.UTC().Format("2006-01-02 15:04"),
				)
			}
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      msg,
			ParseMode: models.ParseModeHTML,
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "mgr_instruction":
		instrText := "(не задан)"
		photoID := ""
		if h.settingsRepo != nil {
			if t, _ := h.settingsRepo.Get("instruction_text"); t != "" {
				instrText = t
			}
			if p, _ := h.settingsRepo.Get("instruction_photo_id"); p != "" {
				photoID = p
			}
		}
		preview := instrText
		{
			n := 0
			for i := range preview {
				n++
				if n > 100 {
					preview = preview[:i] + "..."
					break
				}
			}
		}
		photoStatus := "не задано"
		if photoID != "" {
			photoStatus = "✅ задано"
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf("📖 <b>Настройка инструкции</b>\n\n📷 Фото: %s\n📝 Текст: <code>%s</code>\n\nДля изменения используйте команды:\n<code>/set_instruction_text</code> — следующим сообщением отправьте новый текст (HTML)\n<code>/set_instruction_photo</code> — следующим сообщением отправьте фото", photoStatus, preview),
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "🗑 Удалить фото", CallbackData: "mgr_instruction_clear_photo"}},
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "mgr_instruction_clear_photo":
		if h.settingsRepo != nil {
			_ = h.settingsRepo.Set("instruction_photo_id", "")
		}
		send("✅ Фото инструкции удалено.")

	case "mgr_op_add":
		h.setOPAwaiting(chatID, true)
		send("📢 Введите <b>@username</b> канала или ссылку (например <code>https://t.me/channel</code>).\nОтмена: /cancel")

	default:
		// Диалог создания Premium VPS (TimeWeb): mgr_vps_create_<reqID>, затем region → конфиг (ОС всегда Ubuntu 24.04).
		if strings.HasPrefix(data, "mgr_vps_create_") {
			reqStr := strings.TrimPrefix(data, "mgr_vps_create_")
			reqID, err := strconv.ParseUint(reqStr, 10, 32)
			if err != nil || reqID == 0 {
				send("❌ Неверный request ID")
				return
			}
			h.setVPSSetup(chatID, &VPSSetupData{
				Step:      VPSSetupName,
				RequestID: uint(reqID),
			})
			send("✏️ Введите имя Premium VPS (например premium-vps-2).\nОтмена: /cancel")
			return
		}

		if strings.HasPrefix(data, "mgr_vps_region_") {
			region := strings.TrimPrefix(data, "mgr_vps_region_")
			h.vpsSetupMu.Lock()
			st := h.vpsSetupSteps[chatID]
			if st == nil || st.Step != VPSSetupRegion {
				h.vpsSetupMu.Unlock()
				send("❌ Сначала выберите имя и регион.")
				return
			}
			st.Region = region
			h.vpsSetupMu.Unlock()

			if h.twClient == nil {
				send("❌ TimeWeb не настроен.")
				return
			}
			osID := h.premiumServerOSID
			if osID <= 0 {
				var err error
				osID, err = h.twClient.ResolveUbuntu2404OSID(ctx)
				if err != nil {
					send("❌ Не удалось определить Ubuntu 24.04 в TimeWeb (<code>/api/v1/os/servers</code>). Задайте <code>TIMEWEB_PREMIUM_OS_ID</code> в конфиге.")
					return
				}
			}
			h.vpsSetupMu.Lock()
			st = h.vpsSetupSteps[chatID]
			if st == nil || st.Step != VPSSetupRegion {
				h.vpsSetupMu.Unlock()
				return
			}
			st.OSImageID = strconv.Itoa(osID)
			st.Step = VPSSetupConfig
			h.vpsSetupMu.Unlock()

			configs, err := h.twClient.GetConfigurations(ctx)
			if err != nil {
				h.vpsSetupMu.Lock()
				if st2 := h.vpsSetupSteps[chatID]; st2 != nil {
					st2.Step = VPSSetupRegion
					st2.OSImageID = ""
				}
				h.vpsSetupMu.Unlock()
				send("❌ Ошибка получения configurations.")
				return
			}
			if len(configs) == 0 {
				h.vpsSetupMu.Lock()
				if st2 := h.vpsSetupSteps[chatID]; st2 != nil {
					st2.Step = VPSSetupRegion
					st2.OSImageID = ""
				}
				h.vpsSetupMu.Unlock()
				send("❌ Нет доступных configurations.")
				return
			}
			if len(configs) > 8 {
				configs = configs[:8]
			}
			kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
			for i, cfg := range configs {
				label := fmt.Sprintf("%dCPU/%dRAM/%dGB", cfg.CPU, cfg.RAM, cfg.Disk)
				btn := models.InlineKeyboardButton{Text: label, CallbackData: fmt.Sprintf("mgr_vps_cfg_%d", cfg.ID)}
				rowIdx := i / 2
				if rowIdx >= len(kb.InlineKeyboard) {
					kb.InlineKeyboard = append(kb.InlineKeyboard, []models.InlineKeyboardButton{btn})
				} else {
					kb.InlineKeyboard[rowIdx] = append(kb.InlineKeyboard[rowIdx], btn)
				}
			}
			// Одно сообщение: текст + inline-клавиатура. Отдельное сообщение с Text " " даёт 400
			// «message text is empty» у Telegram — кнопки не отображаются.
			cfgMsg := fmt.Sprintf(
				"🐧 Регион: <code>%s</code>\nОС: <b>Ubuntu 24.04</b> (os_id=<code>%d</code>)\n\n⚙️ Выберите конфигурацию:",
				region, osID,
			)
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      chatID,
				Text:        cfgMsg,
				ParseMode:   models.ParseModeHTML,
				ReplyMarkup: kb,
			})
			return
		}

		if strings.HasPrefix(data, "mgr_vps_os_") {
			send("ℹ️ Выбор ОС отключён: для Premium VPS всегда используется <b>Ubuntu 24.04</b>. Начните создание VPS заново из заявки.")
			return
		}

		if strings.HasPrefix(data, "mgr_vps_cfg_") {
			cfgIDStr := strings.TrimPrefix(data, "mgr_vps_cfg_")
			cfgID, err := strconv.Atoi(cfgIDStr)
			if err != nil {
				send("❌ Неверный config ID")
				return
			}
			h.vpsSetupMu.Lock()
			st := h.vpsSetupSteps[chatID]
			if st == nil || st.Step != VPSSetupConfig {
				h.vpsSetupMu.Unlock()
				send("❌ Сначала выберите регион и конфигурацию.")
				return
			}
			st.ConfigID = cfgID
			st.Step = VPSSetupConfirm
			name := st.Name
			region := st.Region
			osImgID := st.OSImageID
			h.vpsSetupMu.Unlock()

			summary := fmt.Sprintf(
				"🧾 <b>Подтверждение создания VPS</b>\n\nИмя: <code>%s</code>\nРегион: <code>%s</code>\nОС: <b>Ubuntu 24.04</b> (os_id=<code>%s</code>)\nConfigID: <code>%d</code>\n\nПодтвердите:",
				name, region, osImgID, cfgID,
			)
			kb := &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "✅ Создать VPS и обработать очередь", CallbackData: "mgr_vps_confirm"}},
				},
			}
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: summary, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
			return
		}

		if data == "mgr_vps_confirm" {
			h.vpsSetupMu.Lock()
			st := h.vpsSetupSteps[chatID]
			if st == nil || st.Step != VPSSetupConfirm {
				h.vpsSetupMu.Unlock()
				send("❌ Нет активного подтверждения.")
				return
			}
			if st.Processing {
				h.vpsSetupMu.Unlock()
				send("⏳ Создание VPS уже запущено, дождитесь завершения.")
				return
			}
			st.Processing = true
			cpy := *st
			h.vpsSetupMu.Unlock()

			if h.premiumProvisioner == nil || h.vpsReqRepo == nil || h.proxyRepo == nil {
				h.clearVPSSetup(chatID)
				send("❌ TimeWeb Premium не настроен на сервере.")
				return
			}

			send("⏳ Создаём VPS и обрабатываем очередь пользователей...")

			go func() {
				defer h.clearVPSSetup(chatID)
				runCtx := context.Background()
				req, err := h.vpsReqRepo.GetByID(cpy.RequestID)
				if err != nil || req == nil {
					_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Ошибка: request не найден."})
					return
				}
				// "creating" не блокируем — можно возобновить после сбоя (TimewebServerID на заявке).
				if req.Status == "done" {
					_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{
						ChatID: chatID,
						Text:   "ℹ️ Заявка уже выполнена (status=done).",
					})
					return
				}
				req.Name = cpy.Name
				req.RegionID = cpy.Region
				req.OSImageID = cpy.OSImageID
				req.ConfigID = cpy.ConfigID
				_ = h.vpsReqRepo.Update(req)

				if _, err := h.premiumProvisioner.CreateVPSFromRequest(runCtx, req); err != nil {
					_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Ошибка создания VPS: " + err.Error()})
					return
				}

				var tgIDs []int64
				if err := json.Unmarshal([]byte(req.PendingUserIDs), &tgIDs); err != nil {
					_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Ошибка очереди PendingUserIDs: " + err.Error()})
					return
				}

				processed := 0
				remaining := make([]int64, 0)
				for idx, tgid := range tgIDs {
					user, err := h.userRepo.GetByTGID(tgid)
					if err != nil || user == nil {
						continue
					}
					proxy, _ := h.proxyRepo.GetByOwnerID(user.ID)
					if proxy == nil {
						continue
					}

					updatedProxy, err := h.premiumProvisioner.ProvisionExistingProxyForUser(runCtx, user, proxy)
					if err != nil {
						if errors.Is(err, usecase.ErrFloatingIPDailyLimit) {
							remaining = append(remaining, tgIDs[idx:]...)
							break
						}
						_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{
							ChatID: chatID,
							Text:   "❌ Ошибка провижининга tg_id=" + fmt.Sprintf("%d", tgid) + ": " + err.Error(),
						})
						continue
					}

					_ = h.proxyRepo.Update(updatedProxy)
					h.SendPremiumProxyToUser(runCtx, b, tgid, user, updatedProxy)
					processed++

					if idx < len(tgIDs)-1 {
						time.Sleep(2 * time.Second)
					}
				}

				if len(remaining) > 0 && h.vpsReqRepo != nil {
					// Откатываем статус и оставляем хвост очереди.
					req.Status = "pending"
					raw, _ := json.Marshal(remaining)
					req.PendingUserIDs = string(raw)
					_ = h.vpsReqRepo.Update(req)
					_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{
						ChatID: chatID,
						Text:   fmt.Sprintf("⚠️ Floating IP лимит всё ещё исчерпан. Обработано: %d. Осталось в очереди: %d", processed, len(remaining)),
					})
				} else {
					req.Status = "done"
					req.PendingUserIDs = "[]"
					_ = h.vpsReqRepo.Update(req)
					_, _ = b.SendMessage(runCtx, &bot.SendMessageParams{
						ChatID: chatID,
						Text:   fmt.Sprintf("✅ VPS создан. Обработано из очереди: %d", processed),
					})
				}
			}()
			return
		}

		// Пагинация/фильтр прокси: mgr_proxies_<type>_<page>
		if strings.HasPrefix(data, "mgr_proxies_") {
			parts := strings.Split(strings.TrimPrefix(data, "mgr_proxies_"), "_")
			if len(parts) == 2 {
				proxyType := parts[0]
				page, _ := strconv.Atoi(parts[1])
				text, kb, err := h.buildManagerProxiesPage(proxyType, page)
				if err != nil {
					send("❌ Ошибка")
					return
				}
				msgObj := update.CallbackQuery.Message.Message
				if msgObj != nil {
					_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID: msgObj.Chat.ID, MessageID: msgObj.ID,
						Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
					})
				} else {
					b.SendMessage(ctx, &bot.SendMessageParams{
						ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
					})
				}
				return
			}
		}
		if data == "mgr_noop" {
			return
		}
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

	fip := strings.TrimSpace(proxy.TimewebFloatingIPID)
	isLegacy := (fip == "" || fip == "0") && (proxy.PremiumServerID == nil || *proxy.PremiumServerID == 0)

	ddPort := proxy.Port
	// Для legacy ee-секрет используется на порту ddPort + 10000,
	// для TimeWeb — фиксированные порты домена (8443/443).
	if !isLegacy {
		ddPort = domain.PremiumPortDD
	}

	nameDD := fmt.Sprintf(docker.UserContainerNameDD, tgID)
	nameEE := fmt.Sprintf(docker.UserContainerNameEE, tgID)
	ddStatus := "⚪ неизвестен"
	eeStatus := "⚪ неизвестен"
	if h.proDockerMgr != nil {
		if running, err := h.proDockerMgr.IsContainerRunning(ctx, nameDD); err == nil {
			if running {
				ddStatus = "🟢 запущен"
			} else {
				ddStatus = "🔴 остановлен"
			}
		}
		if running, err := h.proDockerMgr.IsContainerRunning(ctx, nameEE); err == nil {
			if running {
				eeStatus = "🟢 запущен"
			} else {
				eeStatus = "🔴 остановлен"
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
			"🔌 Порт dd: <code>%d</code>\n"+
			"🔑 Secret dd: <code>%s</code>\n"+
			"📦 <code>%s</code> — %s\n"+
			"📦 <code>%s</code> — %s\n",
		tgID, until, ddPort, proxy.Secret, nameDD, ddStatus, nameEE, eeStatus,
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

	if h.proDockerMgr == nil {
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

	nameDD := fmt.Sprintf(docker.UserContainerNameDD, tgID)
	nameEE := fmt.Sprintf(docker.UserContainerNameEE, tgID)

	if err := h.proDockerMgr.RemoveUserContainer(ctx, nameDD); err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка удаления контейнера")
		return
	}

	// ee контейнер — best-effort (его может не быть, если SecretEE пустой).
	_ = h.proDockerMgr.RemoveUserContainer(ctx, nameEE)

	if err := h.proDockerMgr.CreateUserContainer(ctx, tgID, proxy); err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка создания контейнера")
		return
	}

	if proxy.SecretEE != "" {
		if err := h.proDockerMgr.CreateUserContainerEE(ctx, tgID, proxy); err != nil {
			log.Printf("[AdminRebuild] CreateUserContainerEE tg_id=%d: %v (non-fatal)", tgID, err)
		}
	}

	h.sendText(ctx, b, update, "✅ Контейнеры пересозданы (dd + ee)")
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

// flushBroadcastMediaGroup выполняет рассылку одного альбома по списку пользователей в отдельной горутине.
// audience передаётся явно (захватывается при добавлении в буфер), чтобы при срабатывании таймера не опираться на уже очищенный broadcastState.
func (h *BotHandler) flushBroadcastMediaGroup(adminID int64, fromChatID int64, messageIDs []int, audience BroadcastAudience) {
	botRef := h.botRef
	if botRef == nil || len(messageIDs) == 0 {
		return
	}
	go func() {
		h.broadcastSem <- struct{}{}
		defer func() { <-h.broadcastSem }()

		sort.Ints(messageIDs)
		ctx := context.Background()
		users, err := h.userRepo.GetAll()
		if err != nil {
			botRef.SendMessage(ctx, &bot.SendMessageParams{
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
			_, err := botRef.CopyMessages(ctx, &bot.CopyMessagesParams{
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
		botRef.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: adminID, Text: resultMsg, ParseMode: models.ParseModeHTML,
		})
	}()
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

	// Контент: текст из Message.Text или подпись к медиа; источник фиксируется явно.
	text := update.Message.Text
	textFromCaption := false
	if text == "" && update.Message.Caption != "" {
		text = update.Message.Caption
		textFromCaption = true
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

	// Копируем данные для горутины (контекст и update не должны использоваться после возврата).
	aud := h.broadcastState.Audience(adminID)
	fromChatID := update.Message.Chat.ID
	messageID := update.Message.ID
	// Entities берём строго из того поля, откуда взят text, чтобы не применить
	// CaptionEntities к тексту из Message.Text или наоборот.
	var entitiesForText []models.MessageEntity
	if !textFromCaption && len(update.Message.Entities) > 0 {
		entitiesForText = append([]models.MessageEntity(nil), update.Message.Entities...)
	} else if textFromCaption && len(update.Message.CaptionEntities) > 0 {
		entitiesForText = append([]models.MessageEntity(nil), update.Message.CaptionEntities...)
	}

	h.broadcastState.Clear(adminID)
	h.sendText(ctx, b, update, "📢 Рассылка запущена, вы получите отчёт по завершении.")

	go func() {
		h.broadcastSem <- struct{}{}
		defer func() { <-h.broadcastSem }()

		bgCtx := context.Background()
		sent, failed := 0, 0
		botRef := h.botRef
		if botRef == nil {
			return
		}
		for _, u := range users {
			if aud == BroadcastAudienceFree && u.IsPremiumActive() {
				continue
			}
			var sendErr error
			if hasMedia {
				_, sendErr = botRef.CopyMessage(bgCtx, &bot.CopyMessageParams{
					ChatID:     u.TGID,
					FromChatID: fromChatID,
					MessageID:  messageID,
					Caption:    text,
					ParseMode:  models.ParseModeHTML,
				})
			} else {
				params := &bot.SendMessageParams{ChatID: u.TGID, Text: text}
				if len(entitiesForText) > 0 {
					params.Entities = entitiesForText
				} else {
					params.ParseMode = models.ParseModeHTML
				}
				_, sendErr = botRef.SendMessage(bgCtx, params)
			}
			if sendErr != nil {
				failed++
			} else {
				sent++
			}
			time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
		}
		reportMsg := fmt.Sprintf("✅ Рассылка завершена. Доставлено: %d, ошибок: %d", sent, failed)
		botRef.SendMessage(bgCtx, &bot.SendMessageParams{
			ChatID: adminID, Text: reportMsg, ParseMode: models.ParseModeHTML,
		})
	}()
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
		return
	}
	if st := h.getVPSSetup(cid); st != nil && st.Step == VPSSetupName {
		text := update.Message.Text
		text = strings.TrimSpace(text)
		if text == "" {
			h.sendText(ctx, b, update, "❌ Имя не может быть пустым. Отправьте /cancel для отмены.")
			return
		}
		// Примитивная защита от HTML в режиме ParseModeHTML.
		nameEsc := strings.ReplaceAll(text, "&", "&amp;")
		nameEsc = strings.ReplaceAll(nameEsc, "<", "&lt;")
		nameEsc = strings.ReplaceAll(nameEsc, ">", "&gt;")

		// Переводим в следующий шаг.
		h.vpsSetupMu.Lock()
		st.Name = text
		st.Step = VPSSetupRegion
		h.vpsSetupMu.Unlock()

		if h.twClient == nil {
			h.sendText(ctx, b, update, "❌ TimeWeb не настроен (twClient=nil).")
			return
		}
		regions, err := h.twClient.GetRegionsWithOSImages(ctx)
		if err != nil {
			h.sendText(ctx, b, update, "❌ Ошибка получения зон доступности TimeWeb (/api/v2/locations). Проверьте токен API.")
			return
		}
		if len(regions) == 0 {
			h.sendText(ctx, b, update, "❌ Нет доступных зон (availability zones).")
			return
		}
		// Ограничиваем количество кнопок, чтобы сообщение не стало слишком большим.
		if len(regions) > 8 {
			regions = regions[:8]
		}
		kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
		for i, r := range regions {
			btn := models.InlineKeyboardButton{
				Text:         r,
				CallbackData: fmt.Sprintf("mgr_vps_region_%s", r),
			}
			// 2 кнопки в строке
			rowIdx := i / 2
			if rowIdx >= len(kb.InlineKeyboard) {
				kb.InlineKeyboard = append(kb.InlineKeyboard, []models.InlineKeyboardButton{btn})
			} else {
				kb.InlineKeyboard[rowIdx] = append(kb.InlineKeyboard[rowIdx], btn)
			}
		}
		h.send(ctx, b, update, fmt.Sprintf("✅ Имя сохранено: <code>%s</code>\n\nВыберите регион:", nameEsc), kb)
		return
	}
	if h.isInstrAwaitingText(cid) {
		text := update.Message.Text
		if text == "" {
			h.sendText(ctx, b, update, "❌ Текст не может быть пустым. Отправьте текст или /cancel для отмены.")
			return
		}
		h.setInstrAwaitingText(cid, false)
		if h.settingsRepo == nil {
			h.sendText(ctx, b, update, "❌ Ошибка: хранилище настроек недоступно.")
			return
		}
		if err := h.settingsRepo.Set("instruction_text", text); err != nil {
			h.sendText(ctx, b, update, "❌ Ошибка сохранения текста.")
			return
		}
		h.sendText(ctx, b, update, "✅ Текст инструкции обновлён.")
		return
	}
	if h.isInstrAwaitingPhoto(cid) {
		if len(update.Message.Photo) == 0 {
			h.sendText(ctx, b, update, "❌ Отправьте фото (не файл). Отмена: /cancel")
			return
		}
		photo := update.Message.Photo[len(update.Message.Photo)-1]
		h.setInstrAwaitingPhoto(cid, false)
		if h.settingsRepo == nil {
			h.sendText(ctx, b, update, "❌ Ошибка: хранилище настроек недоступно.")
			return
		}
		if err := h.settingsRepo.Set("instruction_photo_id", photo.FileID); err != nil {
			h.sendText(ctx, b, update, "❌ Ошибка сохранения фото.")
			return
		}
		h.sendText(ctx, b, update, "✅ Фото инструкции обновлено.")
		return
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

// HandlePremiumInfo — админская команда /premium_info <tg_id>
func (h *BotHandler) HandlePremiumInfo(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 2 {
		h.sendText(ctx, b, update, "❌ Использование: /premium_info <tg_id>")
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
	if h.proxyRepo == nil {
		h.sendText(ctx, b, update, "❌ proxyRepo недоступен")
		return
	}
	proxy, _ := h.proxyRepo.GetByOwnerID(user.ID)
	if proxy == nil || proxy.Type != domain.ProxyTypePremium {
		h.sendText(ctx, b, update, "ℹ️ У пользователя нет Premium-прокси в базе.")
		return
	}

	hasFloat := proxy.TimewebFloatingIPID != ""
	hasPremiumSrv := proxy.PremiumServerID != nil && *proxy.PremiumServerID != 0
	ddPort := proxy.Port
	eePort := proxy.Port + 10000
	if hasFloat || hasPremiumSrv {
		ddPort = domain.PremiumPortDD
		eePort = domain.PremiumPortEE
	}
	isLegacy := !hasFloat && !hasPremiumSrv

	var premiumServerLine string
	if proxy.PremiumServerID != nil && h.premiumServerRepo != nil {
		srv, _ := h.premiumServerRepo.GetByID(*proxy.PremiumServerID)
		if srv != nil {
			premiumServerLine = fmt.Sprintf("\n💎 PremiumServer: <code>%d</code> (TimewebID: <code>%d</code>)\n🌐 VPS IP: <code>%s</code>", srv.ID, srv.TimewebID, srv.IP)
		}
	}

	premUntil := ""
	if user.PremiumUntil != nil {
		premUntil = user.PremiumUntil.UTC().Format("2006-01-02 15:04 UTC")
	}

	var typeBlock string
	if hasFloat {
		psID := uint(0)
		if proxy.PremiumServerID != nil {
			psID = *proxy.PremiumServerID
		}
		typeBlock = fmt.Sprintf(
			"💎 <b>Тип: Premium (floating IP)</b>\n"+
				"🌐 Floating IP: <code>%s</code>\n"+
				"🧷 TimewebFloatingIPID: <code>%s</code>\n"+
				"🧩 PremiumServerID: <code>%d</code>\n",
			proxy.FloatingIP, proxy.TimewebFloatingIPID, psID,
		)
	} else if hasPremiumSrv {
		psID := *proxy.PremiumServerID
		typeBlock = fmt.Sprintf(
			"⏳ <b>Тип: Premium (ожидание floating IP)</b>\n🧩 PremiumServerID: <code>%d</code>\n",
			psID,
		)
	} else if isLegacy {
		containerName := fmt.Sprintf(docker.UserContainerNameDD, user.TGID)
		containerStatus := "⚪ неизвестен (нет подключения к Pro-серверу)"
		if h.proDockerMgr != nil {
			running, err := h.proDockerMgr.IsContainerRunning(ctx, containerName)
			if err == nil {
				if running {
					containerStatus = "🟢 запущен"
				} else {
					containerStatus = "🔴 остановлен"
				}
			}
		}
		typeBlock = fmt.Sprintf(
			"🔶 <b>Тип: Legacy Premium</b>\n"+
				"🌐 IP: <code>%s</code> (Pro-сервер)\n"+
				"🔌 Порт dd: <code>%d</code>\n"+
				"🔑 Secret dd: <code>%s</code>\n"+
				"📦 Контейнер: <code>%s</code> — %s\n"+
				"📦 EE: <code>%s</code>",
			proxy.IP, proxy.Port, proxy.Secret, containerName, containerStatus,
			fmt.Sprintf(docker.UserContainerNameEE, user.TGID),
		)
	}

	h.sendText(ctx, b, update, fmt.Sprintf(
		"💎 <b>Premium info</b>\n\n"+
			"👤 TG ID: <code>%d</code>\n"+
			"📅 Подписка до: %s\n\n"+
			"%s\n"+
			"🔌 DD порт: <code>%d</code>\n🔑 DD секрет: <code>%s</code>\n\n"+
			"🛡 EE порт: <code>%d</code>\n🔑 EE секрет: <code>%s</code>\n"+
			"%s",
		tgID, premUntil, typeBlock,
		ddPort, proxy.Secret,
		eePort, proxy.SecretEE,
		premiumServerLine,
	))
}

// HandleReplaceIP — админская команда /replace_ip <tg_id>
func (h *BotHandler) HandleReplaceIP(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 2 {
		h.sendText(ctx, b, update, "❌ Использование: /replace_ip <tg_id>")
		return
	}
	tgID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Неверный tg_id")
		return
	}
	if h.premiumProvisioner == nil || h.proxyRepo == nil || h.userProxyRepo == nil {
		h.sendText(ctx, b, update, "❌ TimeWeb Premium не настроен (provisioner/proxyRepo/userProxyRepo=nil).")
		return
	}
	user, err := h.userRepo.GetByTGID(tgID)
	if err != nil || user == nil {
		h.sendText(ctx, b, update, "❌ Пользователь не найден")
		return
	}
	proxy, _ := h.proxyRepo.GetByOwnerID(user.ID)
	if proxy == nil || proxy.Type != domain.ProxyTypePremium {
		h.sendText(ctx, b, update, "❌ У пользователя нет Premium-прокси.")
		return
	}

	isTimeweb := proxy.TimewebFloatingIPID != "" || proxy.PremiumServerID != nil
	if proxy.PremiumServerID == nil {
		h.sendText(ctx, b, update, "❌ PremiumServerID отсутствует в proxy_nodes.")
		return
	}

	// Старые значения для обновления user_proxies.
	oldIP := proxy.IP
	ddPort := proxy.Port
	eePort := proxy.Port + 10000
	if isTimeweb {
		ddPort = domain.PremiumPortDD
		eePort = domain.PremiumPortEE
	}
	oldDDSecret := proxy.Secret
	oldEESecret := proxy.SecretEE

	h.sendText(ctx, b, update, "⏳ Замена floating IP и перезапуск контейнеров...")

	newIP, newFloatingID, err := h.premiumProvisioner.ReplaceFloatingIP(ctx, user, proxy)
	if err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка replace_ip: "+err.Error())
		return
	}

	proxy.IP = newIP
	proxy.FloatingIP = newIP
	proxy.TimewebFloatingIPID = newFloatingID
	proxy.Status = domain.ProxyStatusActive
	_ = h.proxyRepo.Update(proxy)

	// Удаляем старые записи прокси в «Мои прокси».
	_ = h.userProxyRepo.DeleteByIPPortSecret(oldIP, ddPort, oldDDSecret)
	if oldEESecret != "" {
		_ = h.userProxyRepo.DeleteByIPPortSecret(oldIP, eePort, oldEESecret)
	}

	// Отправляем пользователю dd+ee (и создаём новые user_proxies).
	h.SendPremiumProxyToUser(ctx, b, tgID, user, proxy)

	h.sendText(ctx, b, update, fmt.Sprintf("✅ IP заменён: <code>%s</code> → <code>%s</code>", oldIP, newIP))
}

// HandleSetupSSHKey — проверка и загрузка SSH-ключа бота в TimeWeb.
// Использование:
//
//	/setup_ssh_key
//	/setup_ssh_key upload
func (h *BotHandler) HandleSetupSSHKey(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	action := ""
	if len(args) >= 2 {
		action = strings.TrimSpace(args[1])
	}

	keyPath := strings.TrimSpace(h.sshKeyPath)
	if keyPath == "" {
		keyPath = "/antiblock/premium-keys/premium_bot_key"
	}
	pubKeyPath := keyPath + ".pub"

	_, errKey := os.Stat(keyPath)
	keyExists := errKey == nil

	if action == "" {
		if keyExists {
			pubBytes, _ := os.ReadFile(pubKeyPath)
			pubKey := strings.TrimSpace(string(pubBytes))
			if pubKey == "" {
				h.sendText(ctx, b, update, fmt.Sprintf(
					"⚠️ Приватный ключ найден: <code>%s</code>\nПубличный ключ не найден или пуст: <code>%s</code>\n\nДля генерации:\n<pre>ssh-keygen -t ed25519 -f %s -N \"\"</pre>",
					keyPath, pubKeyPath, keyPath,
				))
				return
			}
			h.sendText(ctx, b, update, fmt.Sprintf(
				"✅ SSH-ключ найден: <code>%s</code>\n\n📋 Публичный ключ:\n<pre>%s</pre>\n\nДля загрузки в TimeWeb: <code>/setup_ssh_key upload</code>",
				keyPath, pubKey,
			))
			return
		}
		h.sendText(ctx, b, update, fmt.Sprintf(
			"❌ SSH-ключ не найден по пути <code>%s</code>\n\nСгенерируйте ключ на сервере:\n<pre>ssh-keygen -t ed25519 -f %s -N \"\"</pre>\n\nЗатем выполните: <code>/setup_ssh_key upload</code>",
			keyPath, keyPath,
		))
		return
	}

	if action == "upload" {
		if !keyExists {
			h.sendText(ctx, b, update, "❌ Ключ не найден. Сначала сгенерируйте его на сервере.")
			return
		}
		if h.twClient == nil {
			h.sendText(ctx, b, update, "❌ TimeWeb не настроен (TIMEWEB_API_TOKEN пустой).")
			return
		}

		pubBytes, err := os.ReadFile(pubKeyPath)
		if err != nil {
			h.sendText(ctx, b, update, "❌ Не удалось прочитать публичный ключ: "+err.Error())
			return
		}
		pubKey := strings.TrimSpace(string(pubBytes))
		if pubKey == "" {
			h.sendText(ctx, b, update, "❌ Публичный ключ пустой.")
			return
		}

		key, err := h.twClient.UploadSSHKey(ctx, "antiblock-bot", pubKey)
		if err != nil {
			h.sendText(ctx, b, update, "❌ Ошибка загрузки ключа в TimeWeb: "+err.Error())
			return
		}
		h.sendText(ctx, b, update, fmt.Sprintf(
			"✅ Ключ загружен в TimeWeb!\n\n🔑 ID ключа: <code>%d</code>\n\nДобавьте в .env:\n<pre>TIMEWEB_SSH_KEY_ID=%d</pre>\n\nИ перезапустите бота.",
			key.ID, key.ID,
		))
		return
	}

	h.sendText(ctx, b, update, "❌ Неизвестное действие. Используйте: /setup_ssh_key или /setup_ssh_key upload")
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

// HandleSetPriceUSDT обновляет цену в TON: /setprice_usdt <сумма>
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
		h.sendText(ctx, b, update, "❌ Использование: /setprice_usdt <сумма>\nНапример: /setprice_usdt 10 (TON)")
		return
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(args[1], ",", "."), 64)
	if err != nil || f < 0.01 {
		h.sendText(ctx, b, update, "❌ Введите сумму не менее 0.01 TON.")
		return
	}
	if err := h.settingsRepo.Set("premium_usdt", fmt.Sprintf("%.2f", f)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена премиума: %.2f TON.", f))
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

// HandleSetProDays обновляет количество дней Pro: /setpro_days <дней>
func (h *BotHandler) HandleSetProDays(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setpro_days <дней>\nНапример: /setpro_days 30")
		return
	}
	days, err := strconv.Atoi(args[1])
	if err != nil || days < 1 || days > 365 {
		h.sendText(ctx, b, update, "❌ Неверное значение дней (1–365).")
		return
	}
	if err := h.settingsRepo.Set("pro_days", fmt.Sprintf("%d", days)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить настройки.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Pro: %d дней за платёж.", days))
}

// HandleSetProPriceUSDT обновляет цену Pro в TON: /setpro_price_usdt <сумма>
func (h *BotHandler) HandleSetProPriceUSDT(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setpro_price_usdt <сумма>\nНапример: /setpro_price_usdt 3")
		return
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(args[1], ",", "."), 64)
	if err != nil || f < 0.01 {
		h.sendText(ctx, b, update, "❌ Введите сумму не менее 0.01 TON.")
		return
	}
	if err := h.settingsRepo.Set("pro_price_usdt", fmt.Sprintf("%.2f", f)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена Pro: %.2f TON.", f))
}

// HandleSetProPriceStars обновляет цену Pro в Stars: /setpro_price_stars <звёзды>
func (h *BotHandler) HandleSetProPriceStars(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setpro_price_stars <звёзды>\nНапример: /setpro_price_stars 50")
		return
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		h.sendText(ctx, b, update, "❌ Введите число звёзд не менее 1.")
		return
	}
	if err := h.settingsRepo.Set("pro_price_stars", fmt.Sprintf("%d", n)); err != nil {
		h.sendText(ctx, b, update, "❌ Не удалось сохранить.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена Pro: %d Stars.", n))
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
			// При редактировании правим существующие сообщения, не рассылаем заново
			edited := 0
			if h.adPinRepo != nil {
				pins, _ := h.adPinRepo.ListByAdID(ad.ID)
				kb := h.buildAdKeyboard(ad)
				for _, pin := range pins {
					_, errEdit := b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:      pin.ChatID,
						MessageID:   pin.MessageID,
						Text:        ad.Text,
						ParseMode:   models.ParseModeHTML,
						ReplyMarkup: kb,
					})
					if errEdit == nil {
						edited++
					}
					time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
				}
			}
			send(fmt.Sprintf("✅ Объявление обновлено. Отредактировано в %d чатах.", edited))
			h.adComposeState.Clear(adminID)
			return
		}
		// Рассылка только для нового объявления — в фоне, бесплатным пользователям с закреплением
		send("✅ Объявление создано. Рассылка запущена в фоне, вы получите отчёт по завершении.")
		h.adComposeState.Clear(adminID)

		go func(adCopy domain.Ad) {
			bgCtx := context.Background()
			botRef := h.botRef
			if botRef == nil {
				return
			}
			users, errUsers := h.userRepo.GetAll()
			if errUsers != nil {
				botRef.SendMessage(bgCtx, &bot.SendMessageParams{
					ChatID:    adminID,
					Text:      fmt.Sprintf("❌ Ошибка рассылки объявления: %v", errUsers),
					ParseMode: models.ParseModeHTML,
				})
				return
			}
			sent := 0
			kb := h.buildAdKeyboard(&adCopy)
			for _, u := range users {
				if u.IsPremiumActive() {
					continue
				}
				msg, errSend := botRef.SendMessage(bgCtx, &bot.SendMessageParams{
					ChatID: u.TGID, Text: adCopy.Text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
				})
				if errSend == nil && msg != nil && msg.ID != 0 {
					if _, errPin := botRef.PinChatMessage(bgCtx, &bot.PinChatMessageParams{
						ChatID: u.TGID, MessageID: msg.ID,
					}); errPin == nil {
						_ = h.adPinRepo.Create(&domain.AdPin{
							AdID: adCopy.ID, UserID: u.TGID, ChatID: u.TGID, MessageID: msg.ID,
						})
						sent++
					}
				}
				time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
			}
			botRef.SendMessage(bgCtx, &bot.SendMessageParams{
				ChatID:    adminID,
				Text:      fmt.Sprintf("✅ Рассылка объявления завершена. Закреплено у %d пользователей.", sent),
				ParseMode: models.ParseModeHTML,
			})
		}(*ad)
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
	if !isValidChannelInput(raw) {
		h.setOPAwaiting(adminID, false)
		h.sendText(ctx, b, update,
			"❌ Неверный формат. Введите @username или https://t.me/channel. Отмена: /cancel")
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

	// Проверяем, что бот может вызывать GetChatMember для этого канала (т.е. добавлен и имеет нужные права).
	// Если бот не админ/не добавлен, Telegram вернёт ошибку (CHAT_ADMIN_REQUIRED / BOT_NOT_MEMBER),
	// и дальше проверка подписки работать не сможет.
	chatID := channelToChatID(ch)
	if _, err := b.GetChatMember(ctx, &bot.GetChatMemberParams{
		ChatID: chatID,
		UserID: adminID, // достаточно попытаться получить участника-админа; при нехватке прав вернётся ошибка
	}); err != nil {
		h.setOPAwaiting(adminID, false)
		text := fmt.Sprintf("❌ Бот не является администратором канала %s.\n\n"+
			"Добавьте бота в админы этого канала и затем снова нажмите «➕ Добавить канал».", chatID)
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "🔄 Добавить канал ещё раз", CallbackData: "mgr_op_add"}},
			},
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      adminID,
			Text:        text,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: kb,
		})
		return
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
	if st := h.getVPSSetup(adminID); st != nil && st.Step != VPSSetupIdle {
		h.clearVPSSetup(adminID)
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
	if h.isInstrAwaitingText(adminID) {
		h.setInstrAwaitingText(adminID, false)
		cancelled = true
	}
	if h.isInstrAwaitingPhoto(adminID) {
		h.setInstrAwaitingPhoto(adminID, false)
		cancelled = true
	}
	if cancelled {
		h.sendText(ctx, b, update, "❌ Ввод отменён.")
	}
}

func (h *BotHandler) HandleSetInstructionText(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	adminID := chatID(update)
	h.setInstrAwaitingText(adminID, true)
	h.sendText(ctx, b, update, "📝 Отправьте новый текст инструкции (поддерживается HTML). Отмена: /cancel")
}

func (h *BotHandler) HandleSetInstructionPhoto(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	adminID := chatID(update)
	h.setInstrAwaitingPhoto(adminID, true)
	h.sendText(ctx, b, update, "📷 Отправьте фото для инструкции. Отмена: /cancel")
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
	case "buy_pro":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		days := h.getProDays()
		usdt := h.getProUSDT()
		stars := h.getProStars()
		msg := fmt.Sprintf("⚡ <b>Pro</b> — быстрый прокси без рекламы на %d дн.\n\n"+
			"• Два прокси: стандартный (dd) + с маскировкой (ee/fake-TLS)\n"+
			"• Без рекламы и обязательной подписки\n"+
			"• Общий выделенный сервер (быстрый)\n\n"+
			"💰 Стоимость: <b>%.2f TON</b> или <b>%d ⭐ Stars</b>\n\nВыберите способ оплаты:",
			days, usdt, stars)
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "💵 TON (xRocket)", CallbackData: "buy_pro_usdt"}},
				{{Text: "⭐ Telegram Stars", CallbackData: "buy_pro_stars"}},
				{{Text: "◀️ Назад", CallbackData: "cancel_payment"}},
			},
		}
		h.sendOrEdit(ctx, b, chatID, msg, kb)
	case "buy_pro_usdt":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		userID := update.CallbackQuery.From.ID
		user, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
		if err != nil || user == nil {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: "❌ Ошибка. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		days := h.getProDays()
		usdt := h.getProUSDT()
		desc := fmt.Sprintf("Pro %d дней для пользователя %d", days, userID)
		payURL, invoiceID, err := h.paymentUC.CreateInvoice(usdt, "TON", desc, userID)
		if err != nil {
			log.Printf("[payment] xRocket CreateInvoice (pro) error: %v", err)
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: "❌ Не удалось создать счёт TON. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		_ = h.paymentUC.SetInvoiceMeta(invoiceID, "pro", days)
		log.Printf("[payment] xRocket pro invoice %d created for user %d", invoiceID, userID)
		text := fmt.Sprintf("⚡ Оплата Pro в TON через xRocket.\n\nСумма: <b>%.2f TON</b>\n\nНажмите кнопку ниже, чтобы перейти к оплате.", usdt)
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "💵 Оплатить Pro в TON (xRocket)", URL: payURL}},
			},
		}
		msg, errSend := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
		if errSend == nil && msg != nil && msg.ID != 0 {
			_ = h.paymentUC.SetInvoiceMessage(invoiceID, userID, int64(msg.ID))
		}
	case "buy_pro_stars":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		userID := update.CallbackQuery.From.ID
		_, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
		if err != nil {
			h.sendText(ctx, b, update, "❌ Ошибка. Попробуйте позже.")
			return
		}
		days := h.getProDays()
		starsAmount := h.getProStars()
		payload := fmt.Sprintf("pro_%d_%d", days, userID)
		link, err := b.CreateInvoiceLink(ctx, &bot.CreateInvoiceLinkParams{
			Title:       fmt.Sprintf("Pro %d дней", days),
			Description: fmt.Sprintf("Pro подписка на %d дней — быстрый прокси без рекламы", days),
			Payload:     payload,
			Currency:    "XTR",
			Prices:      []models.LabeledPrice{{Label: fmt.Sprintf("Pro %d дней", days), Amount: starsAmount}},
		})
		if err != nil {
			h.sendText(ctx, b, update, "❌ Не удалось создать счёт Stars. Попробуйте позже.")
			return
		}
		msg := "⚡ Оплата Telegram Stars (⭐)\n\nНажмите кнопку для оплаты:"
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "⭐ Оплатить Stars", URL: link}},
			},
		}
		h.send(ctx, b, update, msg, kb)
	case "buy_premium":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleBuyPremium(ctx, b, update)
	case "buy_premium_usdt":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		userID := update.CallbackQuery.From.ID
		user, err := h.userUC.GetOrCreateUser(userID, h.getUsername(update))
		if err != nil || user == nil {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: "❌ Ошибка. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		days := h.getPremiumDays()
		usdt := h.getPremiumUSDT()
		desc := fmt.Sprintf("Premium %d дней для пользователя %d", days, userID)
		payURL, invoiceID, err := h.paymentUC.CreateInvoice(usdt, "TON", desc, userID)
		if err != nil {
			log.Printf("[payment] xRocket CreateInvoice error: %v", err)
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: "❌ Не удалось создать счёт TON. Попробуйте позже.", ParseMode: models.ParseModeHTML})
			return
		}
		_ = h.paymentUC.SetInvoiceMeta(invoiceID, "premium", days)
		log.Printf("[payment] xRocket invoice %d created for user %d", invoiceID, userID)
		text := fmt.Sprintf("💵 Оплата премиума в TON через xRocket.\n\n"+
			"Сумма: <b>%.2f TON</b>\n\nНажмите кнопку ниже, чтобы перейти к оплате.", usdt)
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "💵 Оплатить в TON (xRocket)", URL: payURL}},
			},
		}
		msg, errSend := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: kb})
		if errSend == nil && msg != nil && msg.ID != 0 {
			_ = h.paymentUC.SetInvoiceMessage(invoiceID, userID, int64(msg.ID))
		}
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
			typeLabel := ""
			switch up.ProxyType {
			case domain.ProxyTypeFree:
				typeLabel = "🆓"
			case domain.ProxyTypePro:
				typeLabel = "⚡"
			case domain.ProxyTypePremium:
				typeLabel = "💎"
			}
			label := fmt.Sprintf("%s %s:%d", typeLabel, up.IP, up.Port)
			if len(label) > 64 {
				label = fmt.Sprintf("%s Прокси %d", typeLabel, i+1)
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
		proxy, err := h.userUC.RetryPremiumProxyCreation(chatID)
		if err != nil {
			log.Printf("[Premium] get_premium_proxy tg_id=%d user_id=%d err=%v", chatID, user.ID, err)
			var msg string
			switch {
			case errors.Is(err, usecase.ErrFloatingIPDailyLimit):
				msg = "✅ Оплата получена! Ваш Premium прокси будет готов в течение нескольких минут — мы уведомим вас."
			case errors.Is(err, usecase.ErrNoActivePremiumServer):
				msg = "⏳ Создаём персональный сервер для вашего Premium proxy.\n\n" +
					"Это занимает несколько минут — мы пришлём уведомление как только всё будет готово."
			default:
				msg = "❌ Не удалось создать Premium proxy. Попробуйте позже."
			}
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML})
			return
		}
		h.SendPremiumProxyToUser(ctx, b, chatID, user, proxy)
	case "get_pro_proxy":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		user, err := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
		if err != nil || user == nil {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, Text: "❌ Ошибка. Попробуйте позже.", ParseMode: models.ParseModeHTML,
			})
			return
		}
		if h.proUC == nil {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, Text: "❌ Pro не настроен.", ParseMode: models.ParseModeHTML,
			})
			return
		}
		sub, err := h.proUC.GetActiveSubscription(user.ID)
		if err != nil || sub == nil {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, Text: "❌ У вас нет активной Pro подписки.", ParseMode: models.ParseModeHTML,
			})
			return
		}
		if err := h.activateProAndSend(ctx, b, chatID, 0); err != nil {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, Text: "❌ Ошибка получения Pro proxy. Попробуйте позже.", ParseMode: models.ParseModeHTML,
			})
		}
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
		if !user.ForcedSubCounted {
			user.ForcedSubCounted = true
			_ = h.userRepo.Update(user)
			channels := h.getForcedSubChannels()
			for _, ch := range channels {
				if h.opStatsRepo != nil {
					_ = h.opStatsRepo.RecordClick(ch, userID)
				}
			}
			if h.settingsRepo != nil {
				_ = h.settingsRepo.Increment("forced_subs_count", 1)
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
		h.sendOrEdit(ctx, b, chatID, msg, kb)
	case "reminder_later":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Ок, напомним позже",
		})
		return
	case "show_instructions":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		instrText := "📖 Инструкция пока не настроена. Обратитесь к администратору."
		photoID := ""
		if h.settingsRepo != nil {
			if t, _ := h.settingsRepo.Get("instruction_text"); t != "" {
				instrText = t
			}
			if p, _ := h.settingsRepo.Get("instruction_photo_id"); p != "" {
				photoID = p
			}
		}
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "◀️ Назад", CallbackData: "back_to_main"}},
			},
		}
		if photoID != "" {
			// Удаляем предыдущее отслеживаемое сообщение, чтобы не плодить дубликаты.
			if prevID, ok := h.msgState.Get(chatID); ok {
				_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
					ChatID:    chatID,
					MessageID: prevID,
				})
				h.msgState.Clear(chatID)
			}
			msg, err := b.SendPhoto(ctx, &bot.SendPhotoParams{
				ChatID:      chatID,
				Photo:       &models.InputFileString{Data: photoID},
				Caption:     instrText,
				ParseMode:   models.ParseModeHTML,
				ReplyMarkup: kb,
			})
			if err == nil && msg != nil {
				h.msgState.Set(chatID, msg.ID)
			}
		} else {
			h.sendOrEdit(ctx, b, chatID, instrText, kb)
		}
	case "back_to_main":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		user, err := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
		if err != nil {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Ошибка.", ParseMode: models.ParseModeHTML})
			return
		}
		msg, kb := h.mainMenuContent(user)
		h.sendOrEdit(ctx, b, chatID, msg, kb)
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
		// Клик по кнопке объявления (ad_click_ID) — считаем клик и открываем ссылку. Всегда отвечаем на callback, иначе бесконечная загрузка.
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
					}
					if openURL == "" && ad.ChannelLink != "" {
						openURL = channelToURL(strings.TrimSpace(ad.ChannelLink))
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
			// Ссылка не найдена или ошибка — всё равно снимаем загрузку с кнопки
			b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID, Text: "Ссылка недоступна"})
			return
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

// HandleSuccessfulPayment выдача подписки после оплаты Stars (Premium/Pro)
func (h *BotHandler) HandleSuccessfulPayment(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.SuccessfulPayment == nil {
		return
	}
	sp := update.Message.SuccessfulPayment
	payload := sp.InvoicePayload
	kind := ""
	if strings.HasPrefix(payload, "premium_") {
		kind = "premium"
		payload = strings.TrimPrefix(payload, "premium_")
	} else if strings.HasPrefix(payload, "pro_") {
		kind = "pro"
		payload = strings.TrimPrefix(payload, "pro_")
	} else {
		return
	}

	parts := strings.SplitN(payload, "_", 2)
	if len(parts) != 2 {
		return
	}
	days, err1 := strconv.Atoi(parts[0])
	userID, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || days < 1 {
		return
	}
	_ = h.paymentUC.RecordStarPayment(userID, int64(sp.TotalAmount), sp.Currency, days, sp.TelegramPaymentChargeID)

	if kind == "pro" {
		if err := h.activateProAndSend(ctx, b, userID, days); err != nil {
			log.Printf("HandleSuccessfulPayment ActivatePro tg_id=%d days=%d: %v", userID, days, err)
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID, Text: "❌ Временная ошибка при активации Pro. Попробуйте позже или обратитесь в поддержку.",
			})
			return
		}
		return
	}

	err := h.userUC.ActivatePremium(userID, days)
	if err != nil {
		log.Printf("HandleSuccessfulPayment ActivatePremium tg_id=%d days=%d: %v", userID, days, err)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID, Text: "❌ Временная ошибка при активации. Попробуйте позже или обратитесь в поддержку.",
		})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   fmt.Sprintf("✅ Оплата получена! Премиум на %d дн. активирован. Когда прокси будет готов, вы получите отдельное сообщение.", days),
	})
}
