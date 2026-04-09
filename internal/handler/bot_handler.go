package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/botbuttons"
	"github.com/yourusername/antiblock/internal/botmessage"
	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/alert"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/telegramx"
	"github.com/yourusername/antiblock/internal/usecase"
	"gorm.io/gorm"
)

const broadcastDelayMs = 50 // ~20 сообщений в секунду (лимит Telegram ~30/сек)

const proxiesPerPage = 10

// Макс. длина HTML-текстов поддержки (руны); дальше Telegram всё равно режется по частям при показе.
const maxSupportHTMLRunes = 120000

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
func (h *BotHandler) buildManagerProxiesPage(proxyType string, page int) (string, models.ReplyMarkup, error) {
	if proxyType == "pro" {
		if h.proUC == nil {
			return "⚡ <b>Pro-прокси</b>\n\nPro не настроен.", &telegramx.InlineKeyboardMarkup{
				InlineKeyboard: [][]telegramx.InlineKeyboardButton{
					{
						h.markActiveBtn("proxies_filter_free", h.txt("proxies_filter_free", "🆓"), false, "mgr_proxies_free_0"),
						h.markActiveBtn("proxies_filter_pro", h.txt("proxies_filter_pro", "⚡ Pro"), true, "mgr_proxies_pro_0"),
						h.markActiveBtn("proxies_filter_premium", h.txt("proxies_filter_premium", "💎"), false, "mgr_proxies_premium_0"),
						h.markActiveBtn("proxies_filter_all", h.txt("proxies_filter_all", "Все"), false, "mgr_proxies_all_0"),
					},
					{h.cb("mgr_back", h.txt("mgr_back", "◀️ Назад"), "mgr_back")},
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
		kb := &telegramx.InlineKeyboardMarkup{
			InlineKeyboard: [][]telegramx.InlineKeyboardButton{
				{
					h.markActiveBtn("proxies_filter_free", h.txt("proxies_filter_free", "🆓"), false, "mgr_proxies_free_0"),
					h.markActiveBtn("proxies_filter_pro", h.txt("proxies_filter_pro", "⚡ Pro"), true, "mgr_proxies_pro_0"),
					h.markActiveBtn("proxies_filter_premium", h.txt("proxies_filter_premium", "💎"), false, "mgr_proxies_premium_0"),
					h.markActiveBtn("proxies_filter_all", h.txt("proxies_filter_all", "Все"), false, "mgr_proxies_all_0"),
				},
				{
					h.cb("proxies_refresh", h.txt("proxies_refresh", "🔄 Обновить"), "mgr_proxies_pro_0"),
					h.cb("mgr_back", h.txt("mgr_back", "◀️ Назад"), "mgr_back"),
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
	var rows [][]telegramx.InlineKeyboardButton

	// Строка фильтров
	rows = append(rows, []telegramx.InlineKeyboardButton{
		h.markActiveBtn("proxies_filter_free", h.txt("proxies_filter_free", "🆓"), proxyType == "free", "mgr_proxies_free_0"),
		h.markActiveBtn("proxies_filter_pro", h.txt("proxies_filter_pro", "⚡ Pro"), proxyType == "pro", "mgr_proxies_pro_0"),
		h.markActiveBtn("proxies_filter_premium", h.txt("proxies_filter_premium", "💎"), proxyType == "premium", "mgr_proxies_premium_0"),
		h.markActiveBtn("proxies_filter_all", h.txt("proxies_filter_all", "Все"), proxyType == "all", "mgr_proxies_all_0"),
	})

	// Строка пагинации (без записей в JSON — только иконки/цифры)
	var navRow []telegramx.InlineKeyboardButton
	if page > 0 {
		navRow = append(navRow, telegramx.InlineKeyboardButton{
			Text: "◀️", CallbackData: fmt.Sprintf("mgr_proxies_%s_%d", proxyType, page-1),
		})
	}
	navRow = append(navRow, telegramx.InlineKeyboardButton{
		Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: "mgr_noop",
	})
	if page < totalPages-1 {
		navRow = append(navRow, telegramx.InlineKeyboardButton{
			Text: "▶️", CallbackData: fmt.Sprintf("mgr_proxies_%s_%d", proxyType, page+1),
		})
	}
	rows = append(rows, navRow)
	rows = append(rows, []telegramx.InlineKeyboardButton{
		h.cb("proxies_refresh", h.txt("proxies_refresh", "🔄 Обновить"), fmt.Sprintf("mgr_proxies_%s_%d", proxyType, page)),
		h.cb("mgr_back", h.txt("mgr_back", "◀️ Назад"), "mgr_back"),
	})

	return sb.String(), &telegramx.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
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
	waitingMsgMu        sync.Mutex
	waitingMsgIDs       map[int64][]int
	opAwaiting          map[int64]bool // админ вводит новый канал ОП
	opMu                sync.Mutex
	instrAwaitingText   map[int64]bool
	instrAwaitingPhoto  map[int64]bool
	instrMu             sync.Mutex
	adminIDs            []int64
	techAlerts          *alert.TelegramAlerter // служебный чат технических ошибок (не личка менеджера)
	botRef              *bot.Bot               // для асинхронной рассылки альбомов (устанавливается из main)
	broadcastSem        chan struct{}

	yooKassaProviderToken string // токен провайдера Telegram Payments (ЮKassa)
	yooKassaShopID        string
	yooKassaSecretKey     string
	yooKassaReturnURL     string

	vpsSetupSteps map[int64]*VPSSetupData
	vpsSetupMu    sync.Mutex

	// Ожидание ввода значений поддержки (ключ app_settings → следующее сообщение админа).
	supportAwaitKey map[int64]string
	supportMu       sync.Mutex

	// btnCatalog — тексты и icon_custom_emoji_id из assets/bot_buttons.json (может быть nil).
	btnCatalog *botbuttons.Catalog

	gormDB                *gorm.DB
	paidOps               *usecase.PaidOps
	managerProgressChatID int64
	opsTariffRunning      atomic.Bool
	opsMigrateRunning     atomic.Bool
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
	yooKassaProviderToken string,
	yooKassaShopID string,
	yooKassaSecretKey string,
	yooKassaReturnURL string,
	gormDB *gorm.DB,
	paidOps *usecase.PaidOps,
	managerProgressChatID int64,
	btnCatalog *botbuttons.Catalog,
) *BotHandler {
	return &BotHandler{
		userUC:                userUC,
		proxyUC:               proxyUC,
		proUC:                 proUC,
		paymentUC:             paymentUC,
		userRepo:              userRepo,
		userProxyRepo:         userProxyRepo,
		proxyRepo:             proxyRepo,
		adRepo:                adRepo,
		adPinRepo:             adPinRepo,
		settingsRepo:          settingsRepo,
		opStatsRepo:           opStatsRepo,
		maintenanceWaitRepo:   maintenanceWaitRepo,
		proDockerMgr:          proDockerMgr,
		proServerIP:           proServerIP,
		forcedSubCh:           forcedSubCh,
		broadcastState:        broadcastState,
		broadcastMediaGroup:   broadcastMediaGroup,
		adComposeState:        adComposeState,
		msgState:              NewMessageState(),
		waitingMsgIDs:         make(map[int64][]int),
		opAwaiting:            make(map[int64]bool),
		instrAwaitingText:     make(map[int64]bool),
		instrAwaitingPhoto:    make(map[int64]bool),
		twClient:              twClient,
		premiumProvisioner:    premiumProvisioner,
		vpsReqRepo:            vpsReqRepo,
		premiumServerRepo:     premiumServerRepo,
		sshKeyPath:            sshKeyPath,
		premiumServerOSID:     premiumServerOSID,
		vpsSetupSteps:         make(map[int64]*VPSSetupData),
		supportAwaitKey:       make(map[int64]string),
		adminIDs:              adminIDs,
		broadcastSem:          make(chan struct{}, 1),
		yooKassaProviderToken: yooKassaProviderToken,
		yooKassaShopID:        yooKassaShopID,
		yooKassaSecretKey:     yooKassaSecretKey,
		yooKassaReturnURL:     yooKassaReturnURL,
		gormDB:                gormDB,
		paidOps:               paidOps,
		managerProgressChatID: managerProgressChatID,
		btnCatalog:            btnCatalog,
	}
}

func (h *BotHandler) txt(key, fallback string) string {
	if h.btnCatalog == nil {
		return fallback
	}
	if t := strings.TrimSpace(h.btnCatalog.Text(key)); t != "" {
		return t
	}
	return fallback
}

func (h *BotHandler) menuFmt(key, fallback string, args ...any) string {
	tpl := fallback
	if h.btnCatalog != nil {
		if t := strings.TrimSpace(h.btnCatalog.Text(key)); t != "" {
			tpl = t
		}
	}
	return fmt.Sprintf(tpl, args...)
}

func (h *BotHandler) cb(key, text, data string) telegramx.InlineKeyboardButton {
	return botbuttons.Callback(h.btnCatalog, key, text, data)
}

func (h *BotHandler) urlB(key, text, u string) telegramx.InlineKeyboardButton {
	return botbuttons.URLButton(h.btnCatalog, key, text, u)
}

func (h *BotHandler) markActiveBtn(key, text string, active bool, cb string) telegramx.InlineKeyboardButton {
	if active {
		text = "✓ " + text
	}
	return h.cb(key, text, cb)
}

// discardBroadcastSession сбрасывает FSM рассылки и отбрасывает незавершённые альбомы в буфере.
func (h *BotHandler) discardBroadcastSession(adminID int64) {
	h.broadcastState.Clear(adminID)
	if h.broadcastMediaGroup != nil {
		_ = h.broadcastMediaGroup.FlushAllForAdmin(adminID)
	}
}

func (h *BotHandler) sendBroadcastAudiencePrompt(ctx context.Context, b *bot.Bot, chatID int64) {
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
}

// SetTechAlerts подключает отправку технических инцидентов в служебный Telegram-чат (после bot.New).
func (h *BotHandler) SetTechAlerts(a *alert.TelegramAlerter) {
	h.techAlerts = a
}

func (h *BotHandler) techReport(ctx context.Context, r alert.Report) {
	if h.techAlerts == nil {
		log.Printf("[tech] %s", r.LogLine())
		return
	}
	h.techAlerts.Send(ctx, r)
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

// sendProGroupProxies отправляет два ee-прокси (nineseconds) по ID группы.
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

// SendProGroupProxiesToUser отправляет два ee-прокси (nineseconds); поля PortDD/SecretDD в БД — первый слот ee.
func (h *BotHandler) SendProGroupProxiesToUser(ctx context.Context, b *bot.Bot, tgID int64, group *domain.ProGroup) {
	if group == nil {
		return
	}
	botmessage.SendProGroupTwoEE(ctx, b, tgID, group, botmessage.ProGroupStyleBot)

	// Сохраняем Pro-прокси в «Мои прокси».
	if h.userProxyRepo != nil && h.userRepo != nil {
		u, errU := h.userRepo.GetByTGID(tgID)
		if errU == nil && u != nil {
			if existingDD, _ := h.userProxyRepo.GetByUserIDAndProxy(u.ID, group.ServerIP, group.PortDD, group.SecretDD); existingDD == nil {
				_ = h.userProxyRepo.Create(&domain.UserProxy{
					UserID:    u.ID,
					IP:        group.ServerIP,
					Port:      group.PortDD,
					Secret:    group.SecretDD,
					ProxyType: domain.ProxyTypePro,
				})
			}
			if existingEE, _ := h.userProxyRepo.GetByUserIDAndProxy(u.ID, group.ServerIP, group.PortEE, group.SecretEE); existingEE == nil {
				_ = h.userProxyRepo.Create(&domain.UserProxy{
					UserID:    u.ID,
					IP:        group.ServerIP,
					Port:      group.PortEE,
					Secret:    group.SecretEE,
					ProxyType: domain.ProxyTypePro,
				})
			}
		}
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
// Длинный HTML (> лимита Telegram) режется на несколько сообщений; клавиатура — только на последнем.
func (h *BotHandler) sendOrEdit(ctx context.Context, b *bot.Bot, userID int64, text string, replyMarkup models.ReplyMarkup) {
	const chunkRunes = 3800 // запас под «часть N/M» и разметку
	parts := telegramx.SplitMessageRunes(text, chunkRunes)
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		h.sendOrEditOne(ctx, b, userID, parts[0], replyMarkup)
		return
	}
	for i, p := range parts {
		chunk := p + fmt.Sprintf("\n\n<i>часть %d из %d</i>", i+1, len(parts))
		var kb models.ReplyMarkup
		if i == len(parts)-1 {
			kb = replyMarkup
		}
		if i == 0 {
			h.sendOrEditOne(ctx, b, userID, chunk, kb)
			continue
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      userID,
			Text:        chunk,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: kb,
		})
	}
}

func (h *BotHandler) sendOrEditOne(ctx context.Context, b *bot.Bot, userID int64, text string, replyMarkup models.ReplyMarkup) {
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

func (h *BotHandler) trackWaitingMessage(chatID int64, msgID int) {
	if msgID == 0 {
		return
	}
	h.waitingMsgMu.Lock()
	defer h.waitingMsgMu.Unlock()
	h.waitingMsgIDs[chatID] = append(h.waitingMsgIDs[chatID], msgID)
}

func (h *BotHandler) clearWaitingMessages(ctx context.Context, b *bot.Bot, chatID int64) {
	h.waitingMsgMu.Lock()
	ids := h.waitingMsgIDs[chatID]
	delete(h.waitingMsgIDs, chatID)
	h.waitingMsgMu.Unlock()
	for _, id := range ids {
		_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: id})
	}
}

// ClearWaitingMessages удаляет сообщения ожидания, если они были зарегистрированы.
func (h *BotHandler) ClearWaitingMessages(ctx context.Context, b *bot.Bot, chatID int64) {
	h.clearWaitingMessages(ctx, b, chatID)
}

// RegisterWaitingMessage сохраняет сообщение ожидания, чтобы удалить его после успешной выдачи прокси.
func (h *BotHandler) RegisterWaitingMessage(chatID int64, msgID int) {
	h.trackWaitingMessage(chatID, msgID)
}

// mainMenuContent возвращает текст и клавиатуру главного меню пользователя (для /start и кнопки «Назад»).
func (h *BotHandler) mainMenuContent(user *domain.User) (welcomeMsg string, kb models.ReplyMarkup) {
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
	proDays := h.getProDays()

	var rows [][]telegramx.InlineKeyboardButton

	extraFree := false
	if h.userProxyRepo != nil {
		if list, err := h.userProxyRepo.ListByUserID(user.ID); err == nil {
			for _, up := range list {
				if up.ProxyType == domain.ProxyTypeFree {
					extraFree = true
					break
				}
			}
		}
	}
	proxyKey := "main_get_proxy"
	btnGetProxy := h.txt("main_get_proxy", "🔗 Получить прокси")
	if extraFree {
		btnGetProxy = h.txt("main_get_proxy_extra", "➕ Получить дополнительный free прокси")
		proxyKey = "main_get_proxy_extra"
	}
	rows = append(rows, []telegramx.InlineKeyboardButton{h.cb(proxyKey, btnGetProxy, "get_proxy")})

	if !hasPro {
		btnPro := h.menuFmt("main_buy_pro", "⚡ Купить Pro на %d дн.", proDays)
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_buy_pro", btnPro, "buy_pro")})
	}
	if !hasPremium {
		btnPremium := h.menuFmt("main_buy_premium", "💎 Купить Premium на %d дн.", days)
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_buy_premium", btnPremium, "buy_premium")})
	}

	if hasPro {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_get_pro_proxy", h.txt("main_get_pro_proxy", "⚡ Получить Pro proxy"), "get_pro_proxy")})
	}
	if hasPremium {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_get_premium_proxy", h.txt("main_get_premium_proxy", "🔐 Получить Premium proxy"), "get_premium_proxy")})
	}

	if hasPro {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_extend_pro", h.txt("main_extend_pro", "⚡ Продлить Pro"), "buy_pro")})
	}
	if hasPremium {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_extend_premium", h.txt("main_extend_premium", "💎 Продлить Premium"), "buy_premium")})
	}

	rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("main_my_proxies", h.txt("main_my_proxies", "📋 Мои прокси"), "my_proxies")})
	rows = append(rows, []telegramx.InlineKeyboardButton{
		h.cb("main_instructions", h.txt("main_instructions", "📖 Инструкция"), "show_instructions"),
	})
	rows = append(rows, []telegramx.InlineKeyboardButton{
		h.cb("main_support", h.txt("main_support", "🛟 Поддержка"), "support_menu"),
	})
	kb = &telegramx.InlineKeyboardMarkup{InlineKeyboard: rows}
	return welcomeMsg, kb
}

func (h *BotHandler) setSupportAwaiting(adminID int64, settingKey string) {
	h.supportMu.Lock()
	defer h.supportMu.Unlock()
	if h.supportAwaitKey == nil {
		h.supportAwaitKey = make(map[int64]string)
	}
	if settingKey == "" {
		delete(h.supportAwaitKey, adminID)
		return
	}
	h.supportAwaitKey[adminID] = settingKey
}

func (h *BotHandler) getSupportAwaitingKey(adminID int64) string {
	h.supportMu.Lock()
	defer h.supportMu.Unlock()
	return h.supportAwaitKey[adminID]
}

func (h *BotHandler) clearSupportAwaiting(adminID int64) bool {
	h.supportMu.Lock()
	defer h.supportMu.Unlock()
	if h.supportAwaitKey == nil {
		return false
	}
	if _, ok := h.supportAwaitKey[adminID]; !ok {
		return false
	}
	delete(h.supportAwaitKey, adminID)
	return true
}

func (h *BotHandler) buildSupportMenuKeyboard() models.ReplyMarkup {
	var rows [][]telegramx.InlineKeyboardButton
	rows = append(rows, []telegramx.InlineKeyboardButton{
		h.cb("support_proxy_issue", h.txt("support_proxy_issue", "🔌 Не работает прокси"), "support_proxy_issue"),
		h.cb("support_payment", h.txt("support_payment", "💳 Проблема с оплатой"), "support_payment"),
	})
	pLink, oLink := "", ""
	if h.settingsRepo != nil {
		pLink, _ = h.settingsRepo.Get(domain.SettingSupportPartnershipLink)
		oLink, _ = h.settingsRepo.Get(domain.SettingSupportOtherQuestionLink)
	}
	pLink = strings.TrimSpace(pLink)
	oLink = strings.TrimSpace(oLink)
	if pLink != "" {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.urlB("support_partnership", h.txt("support_partnership", "🤝 Вопрос сотрудничества"), pLink)})
	} else {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("support_partnership", h.txt("support_partnership", "🤝 Вопрос сотрудничества"), "support_partner_missing")})
	}
	if oLink != "" {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.urlB("support_other", h.txt("support_other", "✉️ Другой вопрос"), oLink)})
	} else {
		rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("support_other", h.txt("support_other", "✉️ Другой вопрос"), "support_other_missing")})
	}
	rows = append(rows, []telegramx.InlineKeyboardButton{h.cb("support_back_main", h.txt("support_back_main", "◀️ В главное меню"), "back_to_main")})
	return &telegramx.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (h *BotHandler) supportTextOrPlaceholder(settingKey string) string {
	if h.settingsRepo == nil {
		return "Текст пока не настроен. Обратитесь к администратору."
	}
	t, _ := h.settingsRepo.Get(settingKey)
	t = strings.TrimSpace(t)
	if t == "" {
		return "Текст пока не настроен. Обратитесь к администратору."
	}
	return t
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
	if !h.isPaidActive(user) {
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
func (h *BotHandler) buildForcedSubKeyboard(channels []string) models.ReplyMarkup {
	if len(channels) == 0 {
		return nil
	}
	var rows [][]telegramx.InlineKeyboardButton
	for i, ch := range channels {
		label := channelToChatID(ch)
		if len(label) > 30 {
			label = fmt.Sprintf("Канал %d", i+1)
		}
		rows = append(rows, []telegramx.InlineKeyboardButton{
			{Text: "📢 " + label, URL: channelToURL(ch)},
		})
	}
	rows = append(rows, []telegramx.InlineKeyboardButton{
		h.cb("forced_sub_check", h.txt("forced_sub_check", "✅ Проверить подписку"), "check_sub_forced"),
	})
	return &telegramx.InlineKeyboardMarkup{InlineKeyboard: rows}
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
	if !h.isPaidActive(user) {
		runCtx := context.WithoutCancel(ctx)
		go h.sendActiveAdIfExists(runCtx, b, chatID)
	}
}

// premiumTimewebClientIP — для tg://proxy у TimeWeb Premium только персональный floating IP (не IP VPS).
func premiumTimewebClientIP(p *domain.ProxyNode) string {
	if p == nil {
		return ""
	}
	if ip := strings.TrimSpace(p.FloatingIP); ip != "" {
		return ip
	}
	return strings.TrimSpace(p.IP)
}

// sendPremiumProxyToUser:
// - TimeWeb: два ee-прокси (8443 и 443),
// - legacy Premium: один ee-прокси на исходном порту proxy.Port.
func (h *BotHandler) SendPremiumProxyToUser(ctx context.Context, b *bot.Bot, chatID int64, user *domain.User, proxy *domain.ProxyNode) {
	if proxy == nil || user == nil {
		return
	}
	if proxy.Status != domain.ProxyStatusActive {
		return
	}
	h.clearWaitingMessages(ctx, b, chatID)

	isTimeweb := usecase.IsTimewebFloatingIDSet(proxy.TimewebFloatingIPID)
	port1 := proxy.Port
	port2 := 0
	if isTimeweb {
		port1 = domain.PremiumPortEE1
		port2 = domain.PremiumPortEE2
	}

	clientIP := strings.TrimSpace(proxy.IP)
	if isTimeweb {
		clientIP = premiumTimewebClientIP(proxy)
		if clientIP == "" {
			log.Printf("[Premium] SendPremiumProxyToUser: TimeWeb proxy без floating IP, сообщения не отправляем")
			return
		}
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
			"🛡 <b>%s</b>\n\n🔐 <b>ee / fake-TLS</b>\n🌐 IP: <code>%s</code>\n🔌 Порт: <code>%d</code>\n🔑 Секрет: <code>%s</code>%s",
			title, clientIP, port, secret, hint,
		)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
		})
	}
	title1 := "Ваш Premium proxy"
	if isTimeweb {
		title1 = "Ваш Premium proxy (1/2)"
	}
	sendEE(title1, port1, proxy.Secret, isTimeweb && proxy.SecretEE != "")
	if isTimeweb && proxy.SecretEE != "" {
		sendEE("Ваш Premium proxy (2/2)", port2, proxy.SecretEE, false)
	}

	if h.userProxyRepo != nil {
		if port1 > 0 && proxy.Secret != "" {
			if existing, _ := h.userProxyRepo.GetByUserIDAndProxy(user.ID, clientIP, port1, proxy.Secret); existing == nil {
				_ = h.userProxyRepo.Create(&domain.UserProxy{
					UserID: user.ID, IP: clientIP, Port: port1, Secret: proxy.Secret, ProxyType: domain.ProxyTypePremium,
				})
			}
		}
		if port2 > 0 && proxy.SecretEE != "" {
			if existing, _ := h.userProxyRepo.GetByUserIDAndProxy(user.ID, clientIP, port2, proxy.SecretEE); existing == nil {
				_ = h.userProxyRepo.Create(&domain.UserProxy{
					UserID: user.ID, IP: clientIP, Port: port2, Secret: proxy.SecretEE, ProxyType: domain.ProxyTypePremium,
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

func (h *BotHandler) isYooKassaConfigured() bool {
	// RUB-сценарий (Smart Payment) скрываем, если не заполнены обязательные поля.
	return h.yooKassaShopID != "" && h.yooKassaSecretKey != "" && h.yooKassaReturnURL != ""
}

// getProPriceRub возвращает цену Pro в рублях из настроек.
func (h *BotHandler) getProPriceRub() int {
	if h.settingsRepo == nil {
		return 299
	}
	v, _ := h.settingsRepo.Get("pro_price_rub")
	if v == "" {
		return 299
	}
	n, _ := strconv.Atoi(v)
	if n < 1 {
		return 299
	}
	return n
}

// getPremiumPriceRub возвращает цену Premium в рублях из настроек.
func (h *BotHandler) getPremiumPriceRub() int {
	if h.settingsRepo == nil {
		return 499
	}
	v, _ := h.settingsRepo.Get("premium_price_rub")
	if v == "" {
		return 499
	}
	n, _ := strconv.Atoi(v)
	if n < 1 {
		return 499
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

func (h *BotHandler) isProActiveUser(userID uint) bool {
	if h.proUC == nil || userID == 0 {
		return false
	}
	sub, err := h.proUC.GetActiveSubscription(userID)
	return err == nil && sub != nil
}

func (h *BotHandler) isPaidActive(user *domain.User) bool {
	if user == nil {
		return false
	}
	if user.IsPremiumActive() {
		return true
	}
	return h.isProActiveUser(user.ID)
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
	if !h.isPaidActive(user) && len(channels) > 0 {
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
	premiumPriceRub := h.getPremiumPriceRub()
	msg := fmt.Sprintf("💎 <b>Premium</b> — два ee-прокси на %d дн.\n\n"+
		"• Минимальные риски блокировок\n"+
		"• Индивидуальный сервер\n"+
		"• 2 прокси: ee / fake-TLS (разные порты)\n"+
		"• Максимальная скорость и стабильность\n"+
		"• Можно использовать на нескольких устройствах\n"+
		"• Без рекламы\n\n"+
		"💰 Стоимость: <b>%d ₽</b>, <b>%.2f TON</b> или <b>%d ⭐ Stars</b>\n\nВыберите способ оплаты:",
		days, premiumPriceRub, usdt, starsCount)

	var rows [][]telegramx.InlineKeyboardButton
	if h.isYooKassaConfigured() {
		rows = append(rows, []telegramx.InlineKeyboardButton{{Text: fmt.Sprintf("💳 Банковская карта — %d ₽", premiumPriceRub), CallbackData: "buy_premium_rub"}})
	}
	rows = append(rows,
		[]telegramx.InlineKeyboardButton{{Text: fmt.Sprintf("💵 TON — %.2f", usdt), CallbackData: "buy_premium_usdt"}},
		[]telegramx.InlineKeyboardButton{{Text: fmt.Sprintf("⭐ Telegram Stars — %d ⭐", starsCount), CallbackData: "buy_stars"}},
		[]telegramx.InlineKeyboardButton{h.cb("payment_back", h.txt("payment_back", "◀️ Назад"), "cancel_payment")},
	)
	kb := &telegramx.InlineKeyboardMarkup{InlineKeyboard: rows}
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
		h.techReport(ctx, alert.Report{
			Type:    "add_free_proxy_failed",
			Source:  "handler/HandleAddProxy",
			Tariff:  "free",
			IP:      ip,
			Port:    port,
			ErrText: err.Error(),
		})
		h.sendText(ctx, b, update, "❌ Не удалось добавить прокси. Подробности — в служебном чате ошибок.")
		return
	}
	log.Printf("[addproxy] added Free proxy %s:%d", ip, port)
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Free-прокси добавлен:\nIP: %s\nПорт: %d", ip, port))
}

// managerPanelMessage и клавиатура главного меню (для /manager и кнопки «Назад»)
func (h *BotHandler) managerPanelContent() (msg string, kb models.ReplyMarkup) {
	msg = "🛠 <b>Панель менеджера</b>\n\nВыберите действие:"
	maintKey := "mgr_maintenance"
	if h.isMaintenanceMode() {
		maintKey = "mgr_maintenance_active"
	}
	kb = &telegramx.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegramx.InlineKeyboardButton{
			{
				h.cb("mgr_stats", h.txt("mgr_stats", "📊 Статистика"), "mgr_stats"),
				h.cb("mgr_proxies", h.txt("mgr_proxies", "📋 Прокси"), "mgr_proxies"),
			},
			{
				h.cb("mgr_addproxy", h.txt("mgr_addproxy", "➕ Добавить прокси"), "mgr_addproxy"),
				h.cb("mgr_delproxy", h.txt("mgr_delproxy", "🗑 Удалить прокси"), "mgr_delproxy"),
			},
			{
				h.cb("mgr_broadcast", h.txt("mgr_broadcast", "📢 Рассылка"), "mgr_broadcast"),
				h.cb("mgr_sendad", h.txt("mgr_sendad", "📣 Объявления"), "mgr_sendad"),
			},
			{
				h.cb("mgr_forcedsub", h.txt("mgr_forcedsub", "📢 Управление ОП"), "mgr_forcedsub"),
			},
			{
				h.cb(maintKey, h.maintenanceManagerButtonLabel(), "mgr_maintenance_menu"),
			},
			{
				h.cb("mgr_pro_groups", h.txt("mgr_pro_groups", "⚡ Pro-группы"), "mgr_pro_groups"),
				h.cb("mgr_pro_pricing", h.txt("mgr_pro_pricing", "⚙️ Цена Pro"), "mgr_pro_pricing"),
			},
			{
				h.cb("mgr_instruction", h.txt("mgr_instruction", "📖 Инструкция"), "mgr_instruction"),
			},
			{
				h.cb("mgr_subs", h.txt("mgr_subs", "💎 Premium"), "mgr_subs"),
				h.cb("mgr_pricing", h.txt("mgr_pricing", "⚙️ Цена Premium"), "mgr_pricing"),
			},
			{
				h.cb("mgr_grant", h.txt("mgr_grant", "✅ Выдать премиум"), "mgr_grant"),
				h.cb("mgr_revoke", h.txt("mgr_revoke", "❌ Отозвать премиум"), "mgr_revoke"),
			},
			{
				h.cb("mgr_grant_pro", h.txt("mgr_grant_pro", "✅ Выдать Pro"), "mgr_grant_pro"),
				h.cb("mgr_revoke_pro", h.txt("mgr_revoke_pro", "❌ Отозвать Pro"), "mgr_revoke_pro"),
			},
			{
				h.cb("mgr_support_settings", h.txt("mgr_support_settings", "🛟 Тексты поддержки"), "mgr_support_settings"),
				h.cb("mgr_premium_reissue", h.txt("mgr_premium_reissue", "🔄 Перевыпустить Premium-прокси"), "mgr_premium_reissue"),
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
func (h *BotHandler) refreshKeyboardStats() models.ReplyMarkup {
	return &telegramx.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegramx.InlineKeyboardButton{
			{
				h.cb("mgr_refresh_stats", h.txt("mgr_refresh_stats", "🔄 Обновить"), "mgr_refresh_stats"),
				h.cb("mgr_back", h.txt("mgr_back", "◀️ Назад"), "mgr_back"),
			},
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

	// Любой callback вне сценария broadcast_* сбрасывает ожидание рассылки (в т.ч. предпросмотр и буфер альбома).
	if !strings.HasPrefix(data, "broadcast_") {
		h.discardBroadcastSession(chatID)
	}

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
			ChatID: chatID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: h.refreshKeyboardStats(),
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
			ReplyMarkup: h.refreshKeyboardStats(),
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
		h.sendBroadcastAudiencePrompt(ctx, b, chatID)

	case "broadcast_audience_all":
		h.broadcastState.SetAwaitingMessage(chatID, BroadcastAudienceAll)
		send("📢 Рассылка <b>всем</b>. Отправьте сообщение: текст, фото, видео или документ.\n\nДалее будет <b>предпросмотр</b> — подтвердите отправку. Отмена: /cancel")

	case "broadcast_audience_free":
		h.broadcastState.SetAwaitingMessage(chatID, BroadcastAudienceFree)
		send("📢 Рассылка <b>только бесплатным</b>. Отправьте сообщение: текст, фото, видео или документ.\n\nДалее будет <b>предпросмотр</b> — подтвердите отправку. Отмена: /cancel")

	case "broadcast_confirm":
		h.handleBroadcastConfirm(ctx, b, chatID)

	case "broadcast_cancel":
		h.discardBroadcastSession(chatID)
		send("❌ Рассылка отменена.")

	case "broadcast_preview_back":
		h.discardBroadcastSession(chatID)
		h.sendBroadcastAudiencePrompt(ctx, b, chatID)

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
		proDays := h.getProDays()
		priceRubPro := h.getProPriceRub()
		proStars := h.getProStars()
		premiumDays := h.getPremiumDays()
		priceRubPremium := h.getPremiumPriceRub()
		premiumTON := h.getPremiumUSDT()
		premiumStars := h.getPremiumStars()
		yooEnabled := h.isYooKassaConfigured()
		proRubLine := ""
		premiumRubLine := ""
		rubCommands := ""
		if yooEnabled {
			proRubLine = fmt.Sprintf("💳 Pro (ЮКасса): <b>%d ₽</b>\n", priceRubPro)
			premiumRubLine = fmt.Sprintf("💳 Premium (ЮКасса): <b>%d ₽</b>\n", priceRubPremium)
			rubCommands = "<code>/setprice_rub_pro &lt;₽&gt;</code>\n" +
				"<code>/setprice_rub_premium &lt;₽&gt;</code>\n"
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf(
				"⚙️ <b>Настройки подписки</b>\n\n"+
					"📅 Pro дней: <b>%d</b>\n"+
					"%s"+
					"⭐ Pro Stars: <b>%d</b>\n\n"+
					"📅 Premium дней: <b>%d</b>\n"+
					"%s"+
					"💵 Premium TON: <b>%.2f</b>\n"+
					"⭐ Premium Stars: <b>%d</b>\n\n"+
					"Изменить:\n"+
					"%s"+
					"<code>/setpro_days &lt;дней&gt;</code>\n"+
					"<code>/setpricing &lt;дней&gt;</code>\n"+
					"<code>/setprice_usdt &lt;TON&gt;</code>\n"+
					"<code>/setprice_stars &lt;⭐&gt;</code>",
				proDays, proRubLine, proStars,
				premiumDays, premiumRubLine, premiumTON, premiumStars,
				rubCommands,
			),
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
	case "mgr_grant_pro":
		send("✅ <b>Выдать Pro</b>\n\nОтправьте:\n<code>/grantpro &lt;tg_id&gt; &lt;дней&gt;</code>")
	case "mgr_revoke_pro":
		send("❌ <b>Отозвать Pro</b>\n\nОтправьте:\n<code>/revokepro &lt;tg_id&gt;</code>")

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
		priceRub := h.getProPriceRub()
		usdt := h.getProUSDT()
		stars := h.getProStars()
		yooEnabled := h.isYooKassaConfigured()
		rubLine := ""
		rubCommands := ""
		if yooEnabled {
			rubLine = fmt.Sprintf("💳 ЮКасса: <b>%d ₽</b>\n", priceRub)
			rubCommands = "<code>/setprice_rub_pro &lt;₽&gt;</code>\n"
		}
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf(
				"⚙️ <b>Настройки Pro</b>\n\n"+
					"📅 Дней: <b>%d</b>\n"+
					"%s"+
					"💵 TON: <b>%.2f</b>\n"+
					"⭐ Stars: <b>%d</b>\n\n"+
					"Изменить:\n"+
					"%s"+
					"<code>/setpro_days &lt;дней&gt;</code>\n"+
					"<code>/setpro_price_usdt &lt;сумма&gt;</code>\n"+
					"<code>/setpro_price_stars &lt;звёзды&gt;</code>",
				days, rubLine, usdt, stars, rubCommands,
			),
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

	case "mgr_support_settings":
		if h.settingsRepo == nil {
			send("❌ Настройки недоступны.")
			return
		}
		preview := func(key string, max int) string {
			t, _ := h.settingsRepo.Get(key)
			t = strings.TrimSpace(t)
			if t == "" {
				return "(пусто)"
			}
			rs := []rune(t)
			if len(rs) > max {
				t = string(rs[:max]) + "…"
			}
			return html.EscapeString(t)
		}
		msg := fmt.Sprintf(
			"🛟 <b>Поддержка (тексты и ссылки)</b>\n\n"+
				"🔌 Не работает прокси:\n<code>%s</code>\n\n"+
				"💳 Оплата:\n<code>%s</code>\n\n"+
				"🤝 Ссылка сотрудничества:\n<code>%s</code>\n\n"+
				"✉️ Ссылка «другой вопрос»:\n<code>%s</code>\n\n"+
				"Выберите, что изменить — следующим сообщением пришлите новое значение (HTML для текстов, полный URL для ссылок). Отмена: /cancel",
			preview(domain.SettingSupportProxyNotWorkingText, 80),
			preview(domain.SettingSupportPaymentIssueText, 80),
			preview(domain.SettingSupportPartnershipLink, 120),
			preview(domain.SettingSupportOtherQuestionLink, 120),
		)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML, Text: msg,
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{{Text: "🔌 Текст: не работает прокси", CallbackData: "mgr_sup_edit_proxy"}},
					{{Text: "💳 Текст: оплата", CallbackData: "mgr_sup_edit_pay"}},
					{{Text: "🤝 Ссылка: сотрудничество", CallbackData: "mgr_sup_edit_partner"}},
					{{Text: "✉️ Ссылка: другой вопрос", CallbackData: "mgr_sup_edit_other"}},
					{{Text: "◀️ Назад", CallbackData: "mgr_back"}},
				},
			},
		})

	case "mgr_premium_reissue":
		send("🔄 <b>Перевыпуск Premium-прокси</b>\n\nДля выбранного пользователя с активным Premium будут удалены старый контейнер/FIP и выданы новые ключи (и при TimeWeb — новый IP).\n\nИспользование:\n<code>/reissue_premium &lt;tg_id&gt;</code>\n\nПример: <code>/reissue_premium 123456789</code>")

	case "mgr_sup_edit_proxy":
		h.setSupportAwaiting(chatID, domain.SettingSupportProxyNotWorkingText)
		send("Отправьте текст для «Не работает прокси» (HTML). Отмена: /cancel")

	case "mgr_sup_edit_pay":
		h.setSupportAwaiting(chatID, domain.SettingSupportPaymentIssueText)
		send("Отправьте текст для «Проблема с оплатой» (HTML). Отмена: /cancel")

	case "mgr_sup_edit_partner":
		h.setSupportAwaiting(chatID, domain.SettingSupportPartnershipLink)
		send("Отправьте ссылку (например <code>https://t.me/username</code>). Отмена: /cancel")

	case "mgr_sup_edit_other":
		h.setSupportAwaiting(chatID, domain.SettingSupportOtherQuestionLink)
		send("Отправьте ссылку для «Другой вопрос» (например <code>https://t.me/username</code>). Отмена: /cancel")

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

	port1 := proxy.Port
	port2 := proxy.Port + 10000
	if !isLegacy {
		port1 = domain.PremiumPortEE1
		port2 = domain.PremiumPortEE2
	}

	nameEE1 := fmt.Sprintf(docker.UserContainerNameEE1, tgID)
	nameEE2 := fmt.Sprintf(docker.UserContainerNameEE2, tgID)
	ee1Status := "⚪ неизвестен"
	ee2Status := "⚪ неизвестен"
	if h.proDockerMgr != nil {
		if running, err := h.proDockerMgr.IsContainerRunning(ctx, nameEE1); err == nil {
			if running {
				ee1Status = "🟢 запущен"
			} else {
				ee1Status = "🔴 остановлен"
			}
		}
		if running, err := h.proDockerMgr.IsContainerRunning(ctx, nameEE2); err == nil {
			if running {
				ee2Status = "🟢 запущен"
			} else {
				ee2Status = "🔴 остановлен"
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
			"🔌 EE порт 1: <code>%d</code>\n"+
			"🔑 Secret 1: <code>%s</code>\n"+
			"📦 <code>%s</code> — %s\n"+
			"🔌 EE порт 2: <code>%d</code>\n"+
			"🔑 Secret 2: <code>%s</code>\n"+
			"📦 <code>%s</code> — %s\n",
		tgID, until, port1, proxy.Secret, nameEE1, ee1Status, port2, proxy.SecretEE, nameEE2, ee2Status,
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
	if fip != "" && fip != "0" {
		if h.premiumProvisioner == nil {
			h.sendText(ctx, b, update, "❌ PremiumProvisioner не настроен (TimeWeb)")
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		defer cancel()
		if err := h.premiumProvisioner.RestartContainersForUser(rctx, user, proxy); err != nil {
			h.techReport(ctx, alert.Report{
				Type:     "admin_rebuild_timeweb",
				Source:   "handler/HandleAdminRebuild",
				UserTGID: tgID,
				Tariff:   "premium",
				ProxyID:  proxy.ID,
				IP:       proxy.IP,
				Extra:    "Timeweb FIP",
				ErrText:  err.Error(),
			})
			h.sendText(ctx, b, update, "❌ Ошибка перезапуска на VPS. Подробности — в служебном чате ошибок.")
			return
		}
		h.sendText(ctx, b, update, "✅ Контейнеры ee+ee на VPS перезапущены")
		return
	}

	if h.proDockerMgr == nil {
		h.sendText(ctx, b, update, "❌ Docker менеджер недоступен (нужен для legacy Premium)")
		return
	}

	subCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	h.proDockerMgr.RemoveUserPremiumEEContainers(subCtx, tgID)
	if err := h.proDockerMgr.CreateUserPremiumEEContainers(subCtx, tgID, proxy); err != nil {
		h.techReport(ctx, alert.Report{
			Type:     "admin_rebuild_legacy_docker",
			Source:   "handler/HandleAdminRebuild",
			UserTGID: tgID,
			Tariff:   "premium",
			ProxyID:  proxy.ID,
			IP:       proxy.IP,
			Port:     proxy.Port,
			ErrText:  err.Error(),
		})
		h.sendText(ctx, b, update, "❌ Ошибка создания контейнеров. Подробности — в служебном чате ошибок.")
		return
	}

	h.sendText(ctx, b, update, "✅ Контейнеры пересозданы (ee + ee)")
}

// HandleBroadcast обрабатывает команду /broadcast (только для админов): выбор аудитории, затем сообщение
func (h *BotHandler) HandleBroadcast(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := chatID(update)
	h.discardBroadcastSession(uid)
	h.sendBroadcastAudiencePrompt(ctx, b, uid)
}

// transitionBroadcastToPreview переводит в фазу preview и показывает кнопки подтверждения (reply к исходному сообщению).
func (h *BotHandler) transitionBroadcastToPreview(ctx context.Context, b *bot.Bot, adminID int64, replyToMsgID int, p *BroadcastPending) {
	if b == nil || p == nil || len(p.MessageIDs) == 0 {
		return
	}
	h.broadcastState.SetPreview(adminID, p)
	aud := "всем пользователям (без Pro/Premium в выдаче — как раньше)"
	if p.Audience == BroadcastAudienceFree {
		aud = "только бесплатным (как раньше: без активных Pro/Premium)"
	}
	text := fmt.Sprintf("👁 <b>Предпросмотр рассылки</b>\nАудитория: <b>%s</b>\n\nСообщение ниже будет разослано после подтверждения.", aud)
	kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{{Text: "✅ Подтвердить рассылку", CallbackData: "broadcast_confirm"}},
		{{Text: "❌ Отменить", CallbackData: "broadcast_cancel"}},
		{{Text: "◀️ К выбору аудитории", CallbackData: "broadcast_preview_back"}},
	}}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      adminID,
		Text:        text,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
		ReplyParameters: &models.ReplyParameters{
			MessageID: replyToMsgID,
			ChatID:    adminID,
		},
	})
}

func (h *BotHandler) handleBroadcastConfirm(ctx context.Context, b *bot.Bot, chatID int64) {
	p, ok := h.broadcastState.Pending(chatID)
	if !ok || p == nil || len(p.MessageIDs) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: "❌ Нет данных для рассылки. Откройте «📢 Рассылка» и пройдите шаги снова.",
		})
		return
	}
	pCopy := *p
	h.discardBroadcastSession(chatID)
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, ParseMode: models.ParseModeHTML,
		Text: "📢 Рассылка запущена, вы получите отчёт по завершении.",
	})
	go h.executeBroadcastFromPending(chatID, &pCopy)
}

// executeBroadcastFromPending рассылает копии сообщений с тем же ограничением скорости, что и раньше.
func (h *BotHandler) executeBroadcastFromPending(adminID int64, p *BroadcastPending) {
	botRef := h.botRef
	if botRef == nil || len(p.MessageIDs) == 0 {
		return
	}
	h.broadcastSem <- struct{}{}
	defer func() { <-h.broadcastSem }()

	sort.Ints(p.MessageIDs)
	ctx := context.Background()
	users, err := h.userRepo.GetAll()
	if err != nil {
		_, _ = botRef.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: adminID, Text: "❌ Ошибка получения списка пользователей", ParseMode: models.ParseModeHTML,
		})
		return
	}
	sent, failed := 0, 0
	var lastErr error
	single := len(p.MessageIDs) == 1
	for _, u := range users {
		if h.isPaidActive(u) {
			continue
		}
		var errCopy error
		if single {
			_, errCopy = botRef.CopyMessage(ctx, &bot.CopyMessageParams{
				ChatID:     u.TGID,
				FromChatID: p.FromChatID,
				MessageID:  p.MessageIDs[0],
			})
		} else {
			_, errCopy = botRef.CopyMessages(ctx, &bot.CopyMessagesParams{
				ChatID:     u.TGID,
				FromChatID: p.FromChatID,
				MessageIDs: p.MessageIDs,
			})
		}
		if errCopy != nil {
			failed++
			lastErr = errCopy
		} else {
			sent++
		}
		time.Sleep(time.Duration(broadcastDelayMs) * time.Millisecond)
	}
	resultMsg := fmt.Sprintf("✅ Рассылка завершена. Доставлено: %d, ошибок: %d", sent, failed)
	if !single {
		resultMsg = fmt.Sprintf("✅ Рассылка альбома завершена. Доставлено: %d, ошибок: %d", sent, failed)
	}
	if failed > 0 && lastErr != nil {
		resultMsg += fmt.Sprintf("\n\n⚠️ Пример ошибки: %v", lastErr)
	}
	_, _ = botRef.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: adminID, Text: resultMsg, ParseMode: models.ParseModeHTML,
	})
}

// flushBroadcastMediaGroup вызывается после сборки альбома: показ предпросмотра, без немедленной рассылки.
func (h *BotHandler) flushBroadcastMediaGroup(adminID int64, fromChatID int64, messageIDs []int, audience BroadcastAudience) {
	if len(messageIDs) == 0 {
		return
	}
	if !h.broadcastState.IsAwaitingMessage(adminID) {
		return
	}
	sort.Ints(messageIDs)
	p := &BroadcastPending{Audience: audience, FromChatID: fromChatID, MessageIDs: messageIDs}
	if h.botRef == nil {
		h.broadcastState.Clear(adminID)
		return
	}
	ctx := context.Background()
	h.transitionBroadcastToPreview(ctx, h.botRef, adminID, messageIDs[0], p)
}

// HandleBroadcastMessage принимает контент рассылки и переводит в предпросмотр (без немедленной отправки).
// Альбомы буферизуются; после таймера — предпросмотр.
func (h *BotHandler) HandleBroadcastMessage(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	adminID := chatID(update)
	if !h.broadcastState.IsAwaitingMessage(adminID) {
		return
	}

	mediaGroupID := update.Message.MediaGroupID
	if mediaGroupID != "" && h.broadcastMediaGroup != nil {
		aud := h.broadcastState.Audience(adminID)
		h.broadcastMediaGroup.Add(adminID, mediaGroupID, update.Message.Chat.ID, update.Message.ID, aud, func(aid int64, fromChat int64, ids []int, a BroadcastAudience) {
			if !h.broadcastState.IsAwaitingMessage(aid) {
				return
			}
			h.flushBroadcastMediaGroup(aid, fromChat, ids, a)
		})
		return
	}

	// Досрочный сброс незавершённого альбома: показать предпросмотр по первой группе; текущее сообщение в этом апдейте не обрабатываем.
	if h.broadcastMediaGroup != nil {
		pending := h.broadcastMediaGroup.FlushAllForAdmin(adminID)
		for _, g := range pending {
			if len(g.MessageIDs) > 0 {
				h.flushBroadcastMediaGroup(adminID, g.FromChatID, g.MessageIDs, g.Audience)
				return
			}
		}
	}

	text := update.Message.Text
	if text == "" && update.Message.Caption != "" {
		text = update.Message.Caption
	}
	hasMedia := len(update.Message.Photo) > 0 || update.Message.Video != nil || update.Message.Document != nil
	if !hasMedia && text == "" {
		h.sendText(ctx, b, update, "❌ Отправьте текст, фото, видео или документ. Отмена: /cancel")
		return
	}

	aud := h.broadcastState.Audience(adminID)
	fromChatID := update.Message.Chat.ID
	messageID := update.Message.ID
	p := &BroadcastPending{Audience: aud, FromChatID: fromChatID, MessageIDs: []int{messageID}}
	h.transitionBroadcastToPreview(ctx, b, adminID, messageID, p)
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
	if h.broadcastState.IsAwaitingMessage(cid) {
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
	if key := h.getSupportAwaitingKey(cid); key != "" {
		text := strings.TrimSpace(update.Message.Text)
		if text == "" {
			h.sendText(ctx, b, update, "❌ Значение не может быть пустым. Отправьте текст или /cancel.")
			return
		}
		if key == domain.SettingSupportProxyNotWorkingText || key == domain.SettingSupportPaymentIssueText {
			if utf8.RuneCountInString(text) > maxSupportHTMLRunes {
				h.sendText(ctx, b, update, fmt.Sprintf("❌ Текст слишком длинный (макс. %d символов). Сократите или разбейте на части.", maxSupportHTMLRunes))
				return
			}
			if err := validateSupportHTMLText(text); err != nil {
				h.sendText(ctx, b, update, fmt.Sprintf("❌ Некорректный support-текст: %v", err))
				return
			}
		}
		if key == domain.SettingSupportPartnershipLink || key == domain.SettingSupportOtherQuestionLink {
			if err := validateSupportLink(text); err != nil {
				h.sendText(ctx, b, update, fmt.Sprintf("❌ Некорректная ссылка: %v", err))
				return
			}
		}
		if h.settingsRepo == nil {
			h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
			return
		}
		if err := h.settingsRepo.Set(key, text); err != nil {
			h.sendText(ctx, b, update, "❌ Ошибка сохранения.")
			return
		}
		h.clearSupportAwaiting(cid)
		h.sendText(ctx, b, update, "✅ Значение сохранено.")
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

	hasFloat := usecase.IsTimewebFloatingIDSet(proxy.TimewebFloatingIPID)
	hasPremiumSrv := proxy.PremiumServerID != nil && *proxy.PremiumServerID != 0
	portEE1 := proxy.Port
	portEE2 := proxy.Port + 10000
	if hasFloat || hasPremiumSrv {
		portEE1 = domain.PremiumPortEE1
		portEE2 = domain.PremiumPortEE2
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
		n1 := fmt.Sprintf(docker.UserContainerNameEE1, user.TGID)
		n2 := fmt.Sprintf(docker.UserContainerNameEE2, user.TGID)
		st1 := "⚪ неизвестен (нет Docker)"
		st2 := st1
		if h.proDockerMgr != nil {
			if running, err := h.proDockerMgr.IsContainerRunning(ctx, n1); err == nil {
				if running {
					st1 = "🟢 запущен"
				} else {
					st1 = "🔴 остановлен"
				}
			}
			if running, err := h.proDockerMgr.IsContainerRunning(ctx, n2); err == nil {
				if running {
					st2 = "🟢 запущен"
				} else {
					st2 = "🔴 остановлен"
				}
			}
		}
		typeBlock = fmt.Sprintf(
			"🔶 <b>Тип: Legacy Premium</b>\n"+
				"🌐 IP: <code>%s</code> (Pro-сервер)\n"+
				"🔌 EE порт 1: <code>%d</code> 🔑 <code>%s</code>\n📦 <code>%s</code> — %s\n"+
				"🔌 EE порт 2: <code>%d</code> 🔑 <code>%s</code>\n📦 <code>%s</code> — %s",
			proxy.IP, proxy.Port, proxy.Secret, n1, st1,
			proxy.Port+10000, proxy.SecretEE, n2, st2,
		)
	}

	h.sendText(ctx, b, update, fmt.Sprintf(
		"💎 <b>Premium info</b>\n\n"+
			"👤 TG ID: <code>%d</code>\n"+
			"📅 Подписка до: %s\n\n"+
			"%s\n"+
			"🔌 EE порт 1: <code>%d</code>\n🔑 Secret 1: <code>%s</code>\n\n"+
			"🔌 EE порт 2: <code>%d</code>\n🔑 Secret 2: <code>%s</code>\n"+
			"%s",
		tgID, premUntil, typeBlock,
		portEE1, proxy.Secret,
		portEE2, proxy.SecretEE,
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

	isTimeweb := usecase.IsTimewebFloatingIDSet(proxy.TimewebFloatingIPID) || proxy.PremiumServerID != nil
	if proxy.PremiumServerID == nil {
		h.sendText(ctx, b, update, "❌ PremiumServerID отсутствует в proxy_nodes.")
		return
	}

	// Старые значения для обновления user_proxies.
	oldIP := proxy.IP
	port1 := proxy.Port
	port2 := proxy.Port + 10000
	if isTimeweb {
		port1 = domain.PremiumPortEE1
		port2 = domain.PremiumPortEE2
	}
	oldDDSecret := proxy.Secret
	oldEESecret := proxy.SecretEE

	h.sendText(ctx, b, update, "⏳ Замена floating IP и перезапуск контейнеров...")

	newIP, newFloatingID, err := h.premiumProvisioner.ReplaceFloatingIP(ctx, user, proxy)
	if err != nil {
		ex := ""
		if usecase.IsTimewebFloatingIDSet(proxy.TimewebFloatingIPID) {
			ex = "fip_id=" + proxy.TimewebFloatingIPID
		}
		h.techReport(ctx, alert.Report{
			Type:     "replace_floating_ip_failed",
			Source:   "handler/HandleReplaceIP",
			UserTGID: tgID,
			Username: user.Username,
			Tariff:   "premium",
			ProxyID:  proxy.ID,
			IP:       proxy.IP,
			Extra:    ex,
			ErrText:  err.Error(),
		})
		h.sendText(ctx, b, update, "❌ Замена IP не выполнена. Подробности — в служебном чате ошибок.")
		return
	}

	proxy.IP = newIP
	proxy.FloatingIP = newIP
	proxy.TimewebFloatingIPID = newFloatingID
	proxy.Status = domain.ProxyStatusActive
	_ = h.proxyRepo.Update(proxy)

	// Удаляем старые записи прокси в «Мои прокси».
	_ = h.userProxyRepo.DeleteByIPPortSecret(oldIP, port1, oldDDSecret)
	if oldEESecret != "" {
		_ = h.userProxyRepo.DeleteByIPPortSecret(oldIP, port2, oldEESecret)
	}

	// Отправляем пользователю два ee-прокси (и создаём новые user_proxies).
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

// HandleReissuePremium полностью перевыпускает Premium-прокси (только админ).
func (h *BotHandler) HandleReissuePremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 2 {
		h.sendText(ctx, b, update, "❌ Использование: /reissue_premium <tg_id>")
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
	if !user.IsPremiumActive() {
		h.sendText(ctx, b, update, "❌ У пользователя нет активного Premium")
		return
	}
	h.sendText(ctx, b, update, "⏳ Перевыпускаем Premium-прокси (может занять до нескольких минут)…")
	proxy, err := h.userUC.ReissuePremiumProxy(tgID)
	if err != nil {
		h.techReport(ctx, alert.Report{
			Type:     "premium_reissue_failed",
			Source:   "handler/HandleReissuePremium",
			UserTGID: tgID,
			Username: user.Username,
			Tariff:   "premium",
			ErrText:  err.Error(),
		})
		h.sendText(ctx, b, update, "❌ Перевыпуск не выполнен. Подробности — в служебном чате ошибок.")
		return
	}
	if proxy == nil {
		h.techReport(ctx, alert.Report{
			Type:     "premium_reissue_nil_proxy",
			Source:   "handler/HandleReissuePremium",
			UserTGID: tgID,
			Username: user.Username,
			Tariff:   "premium",
			ErrText:  "proxy is nil after ReissuePremiumProxy",
		})
		h.sendText(ctx, b, update, "❌ Прокси не получен после перевыпуска. Подробности — в служебном чате ошибок.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Premium-прокси перевыпущен для <code>%d</code>.\nIP: <code>%s</code>\nОтправьте пользователю новые ключи через «Получить Premium proxy» или вручную.", tgID, proxy.IP))
}

// HandleGrantPro выдать Pro вручную (админ)
func (h *BotHandler) HandleGrantPro(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.proUC == nil || h.userRepo == nil {
		h.sendText(ctx, b, update, "❌ Pro не настроен.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 3 {
		h.sendText(ctx, b, update, "❌ Использование: /grantpro <tg_id> <дней>")
		return
	}
	tgID, err1 := strconv.ParseInt(args[1], 10, 64)
	days, err2 := strconv.Atoi(args[2])
	if err1 != nil || err2 != nil || days < 1 {
		h.sendText(ctx, b, update, "❌ Неверные аргументы")
		return
	}
	user, err := h.userRepo.GetByTGID(tgID)
	if err != nil || user == nil {
		h.sendText(ctx, b, update, "❌ Пользователь не найден")
		return
	}
	cycle := h.getProDays()
	group, _, err := h.proUC.ActivateProSubscription(user, days, h.proServerIP, h.proDockerMgr, cycle)
	if err != nil {
		h.sendText(ctx, b, update, "❌ "+err.Error())
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Pro выдан пользователю %d на %d дн. (группа %d)", tgID, days, group.ID))
}

// HandleRevokePro отозвать Pro вручную (админ)
func (h *BotHandler) HandleRevokePro(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.proUC == nil || h.userRepo == nil {
		h.sendText(ctx, b, update, "❌ Pro не настроен.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) < 2 {
		h.sendText(ctx, b, update, "❌ Использование: /revokepro <tg_id>")
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
	if err := h.proUC.RevokeProSubscription(user); err != nil {
		h.sendText(ctx, b, update, "❌ "+err.Error())
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Pro отозван у %d", tgID))
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

// HandleSetPriceRubPro задаёт цену Pro в рублях: /setprice_rub_pro <сумма>
func (h *BotHandler) HandleSetPriceRubPro(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setprice_rub_pro <сумма>\nНапример: /setprice_rub_pro 299")
		return
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		h.sendText(ctx, b, update, "❌ Введите целое число рублей.")
		return
	}
	if err := h.settingsRepo.Set("pro_price_rub", fmt.Sprintf("%d", n)); err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка сохранения.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена Pro: %d ₽.", n))
}

// HandleSetPriceRubPremium задаёт цену Premium в рублях: /setprice_rub_premium <сумма>
func (h *BotHandler) HandleSetPriceRubPremium(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Хранилище настроек недоступно.")
		return
	}
	args := strings.Fields(update.Message.Text)
	if len(args) != 2 {
		h.sendText(ctx, b, update, "❌ Использование: /setprice_rub_premium <сумма>\nНапример: /setprice_rub_premium 499")
		return
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		h.sendText(ctx, b, update, "❌ Введите целое число рублей.")
		return
	}
	if err := h.settingsRepo.Set("premium_price_rub", fmt.Sprintf("%d", n)); err != nil {
		h.sendText(ctx, b, update, "❌ Ошибка сохранения.")
		return
	}
	h.sendText(ctx, b, update, fmt.Sprintf("✅ Цена Premium: %d ₽.", n))
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
				if h.isPaidActive(u) {
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
	if h.broadcastState.IsAwaitingMessage(adminID) || h.broadcastState.IsPreview(adminID) {
		h.discardBroadcastSession(adminID)
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
	if h.clearSupportAwaiting(adminID) {
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
		priceRub := h.getProPriceRub()
		msg := fmt.Sprintf("⚡ <b>Pro</b> — быстрые прокси без рекламы на %d дн.\n\n"+
			"• Максимальная скорость\n"+
			"• Без рекламы\n"+
			"• Два ee-прокси на разных портах\n"+
			"• Общий выделенный сервер (стабильно)\n\n"+
			"💰 Стоимость: <b>%d ₽</b>, <b>%.2f TON</b> или <b>%d ⭐ Stars</b>\n\nВыберите способ оплаты:",
			days, priceRub, usdt, stars)
		var rows [][]telegramx.InlineKeyboardButton
		if h.isYooKassaConfigured() {
			rows = append(rows, []telegramx.InlineKeyboardButton{{Text: fmt.Sprintf("💳 Банковская карта — %d ₽", priceRub), CallbackData: "buy_pro_rub"}})
		}
		rows = append(rows,
			[]telegramx.InlineKeyboardButton{{Text: fmt.Sprintf("💵 TON — %.2f", usdt), CallbackData: "buy_pro_usdt"}},
			[]telegramx.InlineKeyboardButton{{Text: fmt.Sprintf("⭐ Telegram Stars — %d ⭐", stars), CallbackData: "buy_pro_stars"}},
			[]telegramx.InlineKeyboardButton{h.cb("payment_back", h.txt("payment_back", "◀️ Назад"), "cancel_payment")},
		)
		kb := &telegramx.InlineKeyboardMarkup{InlineKeyboard: rows}
		h.sendOrEdit(ctx, b, chatID, msg, kb)
	case "buy_pro_rub":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleBuyProRub(ctx, b, update)
	case "buy_premium_rub":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		h.HandleBuyPremiumRub(ctx, b, update)
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
				{{Text: fmt.Sprintf("💵 Оплатить %.2f TON", usdt), URL: payURL}},
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
				{{Text: fmt.Sprintf("💵 Оплатить %.2f TON", usdt), URL: payURL}},
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
			h.sendOrEdit(ctx, b, chatID, "❌ Ошибка. Попробуйте позже.", nil)
			return
		}
		list, errList := h.userProxyRepo.ListByUserID(user.ID)
		if errList != nil || len(list) == 0 {
			kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "◀️ Назад", CallbackData: "back_to_main"}},
			}}
			h.sendOrEdit(ctx, b, chatID, "У вас пока нет сохранённых прокси. Получите прокси через «Получить proxy».", kb)
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
		rows = append(rows, []models.InlineKeyboardButton{{Text: "◀️ Назад", CallbackData: "back_to_main"}})
		h.sendOrEdit(ctx, b, chatID, text, &models.InlineKeyboardMarkup{InlineKeyboard: rows})
	case "get_premium_proxy":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Готовим ключи…",
		})
		user, err := h.userUC.GetOrCreateUser(chatID, h.getUsername(update))
		if err != nil || user == nil || !user.IsPremiumActive() {
			b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "❌ Доступно только для премиум-пользователей.", ParseMode: models.ParseModeHTML})
			return
		}
		if msg, errWait := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			ParseMode: models.ParseModeHTML,
			Text: "⏳ <b>Подождите</b>\n\nНастраиваем персональный Premium proxy. " +
				"Первый запуск часто занимает <b>1–3 минуты</b> — ключи придут в этот чат.",
		}); errWait == nil && msg != nil {
			h.trackWaitingMessage(chatID, msg.ID)
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
		if proxy == nil || proxy.Status != domain.ProxyStatusActive {
			// FIP ещё не применился на сервере или контейнеры не стартовали.
			// Ключи не выдаём, чтобы пользователь не получил недоступный tg://proxy.
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:    chatID,
				ParseMode: models.ParseModeHTML,
				Text:      "⏳ <b>Ещё выполняется настройка</b>.\n\nПовторите запрос через пару минут — как только контейнеры будут готовы, мы сразу пришлём два ee-прокси.",
			})
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
	case "support_menu":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		msg := "🛟 <b>Поддержка</b>\n\nВыберите тему:"
		h.sendOrEdit(ctx, b, chatID, msg, h.buildSupportMenuKeyboard())
	case "support_proxy_issue":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		txt := h.supportTextOrPlaceholder(domain.SettingSupportProxyNotWorkingText)
		kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "◀️ Назад", CallbackData: "support_menu"}}}}
		h.sendOrEdit(ctx, b, chatID, txt, kb)
	case "support_payment":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cqID})
		txt := h.supportTextOrPlaceholder(domain.SettingSupportPaymentIssueText)
		kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "◀️ Назад", CallbackData: "support_menu"}}}}
		h.sendOrEdit(ctx, b, chatID, txt, kb)
	case "support_partner_missing":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Ссылка не настроена администратором.",
			ShowAlert:       true,
		})
	case "support_other_missing":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cqID,
			Text:            "Ссылка не настроена администратором.",
			ShowAlert:       true,
		})
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
						rows := kb.InlineKeyboard
						rows = append(rows, []models.InlineKeyboardButton{{Text: "📋 Мои прокси", CallbackData: "my_proxies"}, {Text: "◀️ Назад", CallbackData: "back_to_main"}})
						h.sendOrEdit(ctx, b, chatID, msg, &models.InlineKeyboardMarkup{InlineKeyboard: rows})
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

func (h *BotHandler) createYooKassaSmartPayment(ctx context.Context, userID int64, tariffType string, amountRub int, days int, description string) (string, string, error) {
	if h.yooKassaShopID == "" || h.yooKassaSecretKey == "" || h.yooKassaReturnURL == "" {
		return "", "", fmt.Errorf("yookassa smart payment config is incomplete")
	}

	reqBody := map[string]interface{}{
		"amount": map[string]string{
			"value":    fmt.Sprintf("%d.00", amountRub),
			"currency": "RUB",
		},
		"capture": true,
		"confirmation": map[string]string{
			"type":       "redirect",
			"return_url": h.yooKassaReturnURL,
		},
		"description": description,
		"metadata": map[string]string{
			"tg_id":        fmt.Sprintf("%d", userID),
			"tariff_type":  tariffType,
			"days_granted": fmt.Sprintf("%d", days),
		},
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.yookassa.ru/v3/payments", bytes.NewBuffer(data))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotence-Key", fmt.Sprintf("tg-%s-%d-%d-%d", tariffType, userID, days, time.Now().UnixNano()))
	auth := base64.StdEncoding.EncodeToString([]byte(h.yooKassaShopID + ":" + h.yooKassaSecretKey))
	req.Header.Set("Authorization", "Basic "+auth)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("yookassa api status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		ID           string `json:"id"`
		Confirmation struct {
			ConfirmationURL string `json:"confirmation_url"`
		} `json:"confirmation"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", err
	}
	if out.ID == "" || out.Confirmation.ConfirmationURL == "" {
		return "", "", fmt.Errorf("yookassa response missing id or confirmation_url")
	}
	return out.ID, out.Confirmation.ConfirmationURL, nil
}

// HandleBuyProRub отправляет ссылку Smart Payment ЮКассы на оплату Pro.
func (h *BotHandler) HandleBuyProRub(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)
	if h.yooKassaShopID == "" || h.yooKassaSecretKey == "" || h.yooKassaReturnURL == "" {
		h.sendText(ctx, b, update, "❌ Оплата в рублях временно недоступна.")
		return
	}

	days := h.getProDays()
	priceRub := h.getProPriceRub()
	paymentID, link, err := h.createYooKassaSmartPayment(
		ctx, userID, "pro", priceRub, days,
		fmt.Sprintf("Pro подписка на %d дней", days),
	)
	if err != nil {
		log.Printf("[YooKassa] HandleBuyProRub create payment tg_id=%d: %v", userID, err)
		h.sendText(ctx, b, update, "❌ Не удалось создать счёт. Попробуйте позже.")
		return
	}

	msg := fmt.Sprintf("💳 Оплата Pro через ЮКассу\n\nСумма: <b>%d ₽</b>\nПериод: <b>%d дней</b>", priceRub, days)
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: fmt.Sprintf("💳 Оплатить %d ₽", priceRub), URL: link}},
		},
	}
	sent, errSend := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      userID,
		Text:        msg,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
	if errSend == nil && sent != nil && sent.ID != 0 {
		_ = h.paymentUC.CreateYooKassaInvoice(&domain.YooKassaInvoice{
			PaymentID:   paymentID,
			TGID:        userID,
			TariffType:  "pro",
			AmountRub:   priceRub,
			DaysGranted: days,
			Status:      "pending",
			ChatID:      userID,
			MessageID:   int64(sent.ID),
		})
	}
}

// HandleBuyPremiumRub отправляет ссылку Smart Payment ЮКассы на оплату Premium.
func (h *BotHandler) HandleBuyPremiumRub(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := chatID(update)
	if h.yooKassaShopID == "" || h.yooKassaSecretKey == "" || h.yooKassaReturnURL == "" {
		h.sendText(ctx, b, update, "❌ Оплата в рублях временно недоступна.")
		return
	}

	days := h.getPremiumDays()
	priceRub := h.getPremiumPriceRub()
	paymentID, link, err := h.createYooKassaSmartPayment(
		ctx, userID, "premium", priceRub, days,
		fmt.Sprintf("Premium подписка на %d дней", days),
	)
	if err != nil {
		log.Printf("[YooKassa] HandleBuyPremiumRub create payment tg_id=%d: %v", userID, err)
		h.sendText(ctx, b, update, "❌ Не удалось создать счёт. Попробуйте позже.")
		return
	}

	msg := fmt.Sprintf("💳 Оплата Premium через ЮКассу\n\nСумма: <b>%d ₽</b>\nПериод: <b>%d дней</b>", priceRub, days)
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: fmt.Sprintf("💳 Оплатить %d ₽", priceRub), URL: link}},
		},
	}
	sent, errSend := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      userID,
		Text:        msg,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
	if errSend == nil && sent != nil && sent.ID != 0 {
		_ = h.paymentUC.CreateYooKassaInvoice(&domain.YooKassaInvoice{
			PaymentID:   paymentID,
			TGID:        userID,
			TariffType:  "premium",
			AmountRub:   priceRub,
			DaysGranted: days,
			Status:      "pending",
			ChatID:      userID,
			MessageID:   int64(sent.ID),
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
	pc := update.PreCheckoutQuery
	kind, days, userID, ok := parsePaymentPayload(pc.InvoicePayload)
	if !ok || userID <= 0 || days <= 0 {
		_, _ = b.AnswerPreCheckoutQuery(ctx, &bot.AnswerPreCheckoutQueryParams{
			PreCheckoutQueryID: pc.ID,
			OK:                 false,
			ErrorMessage:       "Платеж не прошел проверку. Попробуйте создать новый счет.",
		})
		return
	}
	if !h.isValidPreCheckout(kind, days, pc.Currency, pc.TotalAmount) {
		_, _ = b.AnswerPreCheckoutQuery(ctx, &bot.AnswerPreCheckoutQueryParams{
			PreCheckoutQueryID: pc.ID,
			OK:                 false,
			ErrorMessage:       "Счет устарел или не прошел проверку. Создайте новый платеж.",
		})
		return
	}

	_, _ = b.AnswerPreCheckoutQuery(ctx, &bot.AnswerPreCheckoutQueryParams{
		PreCheckoutQueryID: pc.ID,
		OK:                 true,
	})
}

// HandleSuccessfulPayment выдача подписки после оплаты (Stars XTR / ЮKassa RUB).
func (h *BotHandler) HandleSuccessfulPayment(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.SuccessfulPayment == nil {
		return
	}
	sp := update.Message.SuccessfulPayment
	kind, days, userID, ok := parsePaymentPayload(sp.InvoicePayload)
	if !ok || userID <= 0 || days <= 0 {
		return
	}

	// Telegram Stars (XTR)
	if sp.Currency == "XTR" {
		expDays := h.getPremiumDays()
		expAmount := h.getPremiumStars()
		if kind == "pro" {
			expDays = h.getProDays()
			expAmount = h.getProStars()
		}
		if days != expDays || int(sp.TotalAmount) != expAmount {
			return
		}
		orchestrator := usecase.NewPaymentOrchestrator(
			h.paymentUC,
			func(tgID int64, d int) error { return h.userUC.ActivatePremium(tgID, d) },
			func(tgID int64, d int) error { return h.activateProAndSend(ctx, b, tgID, d) },
			func(in usecase.PaymentEventInput) error {
				return h.paymentUC.RecordStarPayment(in.TGID, int64(sp.TotalAmount), in.Currency, in.Days, sp.TelegramPaymentChargeID)
			},
			nil,
		)
		res, err := orchestrator.ProcessPaidEvent(usecase.PaymentEventInput{
			Provider:   "telegram",
			ExternalID: sp.TelegramPaymentChargeID,
			TGID:       userID,
			Tariff:     kind,
			Days:       days,
			Currency:   sp.Currency,
		})
		if err != nil {
			log.Printf("HandleSuccessfulPayment orchestration tg_id=%d days=%d: %v", userID, days, err)
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID, Text: "❌ Временная ошибка при активации. Попробуйте позже или обратитесь в поддержку.",
			})
			return
		}
		if res != nil && res.Status == usecase.PaymentAlreadyProcessed {
			return
		}
		if kind == "premium" {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   fmt.Sprintf("✅ Оплата получена! Премиум на %d дн. активирован. Когда прокси будет готов, вы получите отдельное сообщение.", days),
			})
		}
		return
	}

	// ЮKassa (RUB)
	if sp.Currency == "RUB" {
		expDays := h.getPremiumDays()
		expAmount := h.getPremiumPriceRub() * 100
		if kind == "pro" {
			expDays = h.getProDays()
			expAmount = h.getProPriceRub() * 100
		}
		if days != expDays || int(sp.TotalAmount) != expAmount {
			return
		}
		orchestrator := usecase.NewPaymentOrchestrator(
			h.paymentUC,
			func(tgID int64, d int) error { return h.userUC.ActivatePremium(tgID, d) },
			func(tgID int64, d int) error { return h.activateProAndSend(ctx, b, tgID, d) },
			func(in usecase.PaymentEventInput) error {
				return h.paymentUC.RecordYooKassaPayment(
					in.TGID, in.Tariff, in.AmountRub, in.Days, sp.TelegramPaymentChargeID, sp.ProviderPaymentChargeID,
				)
			},
			nil,
		)
		res, err := orchestrator.ProcessPaidEvent(usecase.PaymentEventInput{
			Provider:   "telegram_rub",
			ExternalID: sp.ProviderPaymentChargeID,
			TGID:       userID,
			Tariff:     kind,
			Days:       days,
			AmountRub:  sp.TotalAmount / 100,
			Currency:   sp.Currency,
		})
		if err != nil {
			log.Printf("[YooKassa] orchestration tg_id=%d days=%d: %v", userID, days, err)
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID, Text: "❌ Временная ошибка при активации. Попробуйте позже или обратитесь в поддержку.",
			})
			return
		}
		if res != nil && res.Status == usecase.PaymentAlreadyProcessed {
			return
		}
		if kind == "premium" {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:    update.Message.Chat.ID,
				ParseMode: models.ParseModeHTML,
				Text:      fmt.Sprintf("✅ Оплата получена! Премиум на %d дн. активирован. Когда прокси будет готов, вы получите отдельное сообщение.", days),
			})
		}
	}
}

func parsePaymentPayload(payload string) (kind string, days int, userID int64, ok bool) {
	s := strings.TrimSpace(payload)
	switch {
	case strings.HasPrefix(s, "premium_"):
		kind = "premium"
		s = strings.TrimPrefix(s, "premium_")
	case strings.HasPrefix(s, "pro_"):
		kind = "pro"
		s = strings.TrimPrefix(s, "pro_")
	default:
		return "", 0, 0, false
	}
	parts := strings.SplitN(s, "_", 2)
	if len(parts) != 2 {
		return "", 0, 0, false
	}
	d, errD := strconv.Atoi(parts[0])
	u, errU := strconv.ParseInt(parts[1], 10, 64)
	if errD != nil || errU != nil || d < 1 || u < 1 {
		return "", 0, 0, false
	}
	return kind, d, u, true
}

func (h *BotHandler) isValidPreCheckout(kind string, days int, currency string, totalAmount int) bool {
	switch currency {
	case "XTR":
		expDays := h.getPremiumDays()
		expAmount := h.getPremiumStars()
		if kind == "pro" {
			expDays = h.getProDays()
			expAmount = h.getProStars()
		}
		return days == expDays && totalAmount == expAmount
	case "RUB":
		expDays := h.getPremiumDays()
		expAmount := h.getPremiumPriceRub() * 100
		if kind == "pro" {
			expDays = h.getProDays()
			expAmount = h.getProPriceRub() * 100
		}
		return days == expDays && totalAmount == expAmount
	default:
		return false
	}
}

func validateSupportHTMLText(text string) error {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<script") {
		return fmt.Errorf("script-теги запрещены")
	}
	if strings.Contains(lower, "<a ") || strings.Contains(lower, "<a>") {
		return fmt.Errorf("HTML-ссылки запрещены, используйте отдельное поле ссылки")
	}
	return nil
}

func validateSupportLink(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("только http/https")
	}
	if strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("пустой host")
	}
	if u.User != nil {
		return fmt.Errorf("userinfo в URL запрещен")
	}
	return nil
}
