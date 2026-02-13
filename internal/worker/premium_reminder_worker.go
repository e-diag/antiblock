package worker

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

// PremiumReminderWorker отправляет напоминания за 7 дней до окончания подписки
type PremiumReminderWorker struct {
	bot          *bot.Bot
	userUC       usecase.UserUseCase
	paymentUC    usecase.PaymentUseCase
	settingsRepo repository.SettingsRepository
	config       config.WorkerConfig
	stop         chan struct{}
}

// NewPremiumReminderWorker создаёт воркер напоминаний о продлении премиума
func NewPremiumReminderWorker(
	b *bot.Bot,
	userUC usecase.UserUseCase,
	paymentUC usecase.PaymentUseCase,
	settingsRepo repository.SettingsRepository,
	cfg config.WorkerConfig,
) *PremiumReminderWorker {
	return &PremiumReminderWorker{
		bot:          b,
		userUC:       userUC,
		paymentUC:    paymentUC,
		settingsRepo: settingsRepo,
		config:       cfg,
		stop:         make(chan struct{}),
	}
}

// Start запускает воркер
func (w *PremiumReminderWorker) Start() {
	if !w.config.Enabled {
		log.Println("Premium reminder worker is disabled")
		return
	}
	interval := w.config.Interval()
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	log.Printf("Starting premium reminder worker (interval: %v)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	w.sendReminders()
	for {
		select {
		case <-ticker.C:
			w.sendReminders()
		case <-w.stop:
			log.Println("Premium reminder worker stopped")
			return
		}
	}
}

// Stop останавливает воркер
func (w *PremiumReminderWorker) Stop() {
	close(w.stop)
}

func (w *PremiumReminderWorker) getPremiumDays() int {
	if w.settingsRepo == nil {
		return 30
	}
	v, _ := w.settingsRepo.Get("premium_days")
	if v == "" {
		return 30
	}
	n, _ := strconv.Atoi(v)
	if n < 1 {
		return 30
	}
	return n
}

func (w *PremiumReminderWorker) getPremiumUSDT() float64 {
	if w.settingsRepo == nil {
		return 10
	}
	v, _ := w.settingsRepo.Get("premium_usdt")
	if v == "" {
		return 10
	}
	f, _ := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
	if f < 0.01 {
		return 10
	}
	return f
}

func (w *PremiumReminderWorker) getPremiumStars() int {
	if w.settingsRepo == nil {
		return 100
	}
	v, _ := w.settingsRepo.Get("premium_stars")
	if v == "" {
		return 100
	}
	n, _ := strconv.Atoi(v)
	if n < 1 {
		return 100
	}
	return n
}

const reminderRetryDelay = 5 * time.Second

func (w *PremiumReminderWorker) sendReminders() {
	var users []*domain.User
	for attempt := 0; attempt < 2; attempt++ {
		var err error
		users, err = w.userUC.GetUsersForPremiumReminder()
		if err != nil {
			if attempt == 0 {
				time.Sleep(reminderRetryDelay)
				continue
			}
			log.Printf("Premium reminder: GetUsersForPremiumReminder error: %v", err)
			return
		}
		break
	}
	if len(users) == 0 {
		return
	}
	log.Printf("Premium reminder: sending to %d user(s)", len(users))
	starsCount := w.getPremiumStars()
	ctx, cancel := context.WithTimeout(context.Background(), w.config.Timeout())
	defer cancel()
	for _, u := range users {
		// CryptoPay отключён — только Stars
		msg := fmt.Sprintf("⏰ Ваша Premium-подписка истекает через 7 дней.\n\nПродлить подписку и сохранить персональный proxy?\n\n💰 Стоимость: %d ⭐ Stars", starsCount)
		rows := [][]models.InlineKeyboardButton{
			{{Text: "⭐ Telegram Stars", CallbackData: "buy_stars"}},
			{{Text: "Позже", CallbackData: "reminder_later"}},
		}
		kb := &models.InlineKeyboardMarkup{InlineKeyboard: rows}
		_, errSend := w.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: u.TGID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
		})
		if errSend != nil {
			log.Printf("Premium reminder: send to %d: %v", u.TGID, errSend)
			continue
		}
		if errMark := w.userUC.MarkPremiumReminderSent(u.TGID); errMark != nil {
			log.Printf("Premium reminder: MarkPremiumReminderSent %d: %v", u.TGID, errMark)
		}
	}
}
