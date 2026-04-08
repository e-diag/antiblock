package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/usecase"
)

const (
	envOpsContour = "OPS_CONTOUR"

	opsMigrateStepDelay      = 5 * time.Second
	opsMigrateReportEvery    = 10 * time.Minute
	opsCompensateNotifyDelay = 2 * time.Second
)

// SendManagerStartupNotification — после старта бота: одно сообщение в чат прогресса (TELEGRAM_MANAGER_PROGRESS_CHAT_ID) со списком /ops_*.
func (h *BotHandler) SendManagerStartupNotification(ctx context.Context, b *bot.Bot) {
	if b == nil {
		return
	}
	when := time.Now().UTC().Format(time.RFC3339)
	contour := strings.TrimSpace(os.Getenv(envOpsContour))
	if contour == "" {
		contour = "bot"
	}
	text := strings.TrimSpace(fmt.Sprintf(`✅ <b>Бот запущен</b> (UTC: <code>%s</code>, контур отчётов: <code>%s</code>)

<b>Тарифы (+14 дн. и рассылка компенсации)</b>
<code>/ops_tariff_apply</code> — начислить +14 дн. и очередь TG (один раз)
<code>/ops_tariff_notify</code> — разослать очередь уведомлений

<b>Пересоздание прокси (Pro + Premium + legacy)</b>
<code>/ops_proxy_migrate</code> — фон: миграция dd→ee (без писем пользователям о процессе)
<code>/ops_proxy_migrate_reset</code> — сброс v2 перед повторным полным прогоном

<code>/ops_help</code> — краткая справка

<i>Команды вводятся в личке с ботом под админским аккаунтом.</i>`, when, contour))
	if h.managerProgressChatID == 0 {
		log.Println("[ops] TELEGRAM_MANAGER_PROGRESS_CHAT_ID не задан — текст уходит в личку всем TELEGRAM_ADMIN_ID_*")
	} else {
		log.Printf("[ops] стартовое уведомление → manager progress chat_id=%d", h.managerProgressChatID)
	}
	h.sendOpsToManagers(ctx, b, text)
}

func (h *BotHandler) sendOpsToManagers(ctx context.Context, b *bot.Bot, html string) {
	if h.managerProgressChatID != 0 {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: h.managerProgressChatID, Text: html, ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			log.Printf("[ops] SendMessage manager progress chat_id=%d: %v", h.managerProgressChatID, err)
		}
		return
	}
	for _, id := range h.adminIDs {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: id, Text: html, ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			log.Printf("[ops] SendMessage admin tg_id=%d: %v", id, err)
		}
	}
}

func (h *BotHandler) opsReportChatID(fallback int64) int64 {
	if h.managerProgressChatID != 0 {
		return h.managerProgressChatID
	}
	return fallback
}

// HandleOpsHelp — /ops_help
func (h *BotHandler) HandleOpsHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	text := `📋 <b>Операции (админ)</b>

<b>Тарифы</b>
<code>/ops_tariff_apply</code> — +14 дн. в БД + очередь рассылки (один раз)
<code>/ops_tariff_notify</code> — отправить очередь компенсации пользователям

<b>Прокси</b>
<code>/ops_proxy_migrate</code> — фон: миграция v2 (без писем юзерам о ходе работ)
<code>/ops_proxy_migrate_reset</code> — сброс маркеров v2

Отчёты: чат <code>manager_progress_chat_id</code> или этот диалог. Контур: env <code>OPS_CONTOUR</code>.`
	h.sendText(ctx, b, update, text)
}

// HandleOpsTariffApply — /ops_tariff_apply
func (h *BotHandler) HandleOpsTariffApply(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || h.gormDB == nil || h.paidOps == nil {
		h.sendText(ctx, b, update, "❌ Операции недоступны (БД или paid ops).")
		return
	}
	if err := usecase.Compensate14DaysTransactional(h.gormDB, h.paidOps); err != nil {
		if errors.Is(err, usecase.ErrOpsLockBusy) {
			h.sendText(ctx, b, update, "⏳ Компенсация уже выполняется в другом процессе.")
			return
		}
		if err == usecase.ErrPaidCompensationAlreadyDone {
			h.sendText(ctx, b, update, "ℹ️ Компенсация +14 дн. уже была применена ранее.")
			return
		}
		h.sendText(ctx, b, update, fmt.Sprintf("❌ Ошибка: %v", err))
		return
	}
	h.sendText(ctx, b, update, "✅ +14 дн. начислено, очередь уведомлений записана. Запустите <code>/ops_tariff_notify</code> для рассылки.")
}

// HandleOpsTariffNotify — /ops_tariff_notify
func (h *BotHandler) HandleOpsTariffNotify(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || h.settingsRepo == nil || h.paidOps == nil {
		h.sendText(ctx, b, update, "❌ Операции недоступны.")
		return
	}
	if !h.opsTariffRunning.CompareAndSwap(false, true) {
		h.sendText(ctx, b, update, "⏳ Рассылка компенсации уже выполняется.")
		return
	}
	chatID := chatID(update)
	go func() {
		defer h.opsTariffRunning.Store(false)
		h.runTariffNotifyDrain(b, chatID)
	}()
}

func (h *BotHandler) runTariffNotifyDrain(b *bot.Bot, replyChatID int64) {
	ctx := context.Background()
	dest := h.opsReportChatID(replyChatID)
	send := func(tgID int64, text string) error {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: tgID, Text: text, ParseMode: models.ParseModeHTML,
		})
		return err
	}
	total, err := usecase.RunCompensationNotifyDrain(h.settingsRepo, send, opsCompensateNotifyDelay)
	if err != nil {
		if err == usecase.ErrCompensationNoticeQueueEmpty {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: dest, ParseMode: models.ParseModeHTML,
				Text:   "ℹ️ Очередь уведомлений о компенсации пуста (уже разослано или не было <code>/ops_tariff_apply</code>).",
			})
			return
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: dest, ParseMode: models.ParseModeHTML,
			Text:   fmt.Sprintf("❌ Ошибка рассылки компенсации: <code>%v</code>", err),
		})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: dest, ParseMode: models.ParseModeHTML,
		Text:   fmt.Sprintf("✅ Рассылка компенсации завершена. Отправлено сообщений: <b>%d</b>", total),
	})
}

// HandleOpsProxyMigrate — /ops_proxy_migrate
func (h *BotHandler) HandleOpsProxyMigrate(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || h.paidOps == nil {
		h.sendText(ctx, b, update, "❌ Миграция недоступна (paid ops не настроен).")
		return
	}
	if h.proDockerMgr == nil && h.premiumProvisioner == nil {
		h.sendText(ctx, b, update, "❌ Нужен Pro Docker TLS или TimeWeb PremiumProvisioner в конфиге.")
		return
	}
	if !h.opsMigrateRunning.CompareAndSwap(false, true) {
		h.sendText(ctx, b, update, "⏳ Миграция прокси уже выполняется в фоне.")
		return
	}
	contour := strings.TrimSpace(os.Getenv(envOpsContour))
	if contour == "" {
		contour = "bot"
	}
	replyChat := chatID(update)
	go func() {
		defer h.opsMigrateRunning.Store(false)
		h.runProxyMigrateDaemon(b, replyChat, contour)
	}()
	h.sendText(ctx, b, update, fmt.Sprintf("🔄 Миграция прокси запущена в фоне. Отчёты: чат прогресса или этот диалог. Контур: <code>%s</code>", contour))
}

func (h *BotHandler) runProxyMigrateDaemon(b *bot.Bot, triggerChatID int64, contour string) {
	ctx := context.Background()
	dest := h.opsReportChatID(triggerChatID)
	lastReport := time.Now()
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: dest, ParseMode: models.ParseModeHTML,
		Text:   fmt.Sprintf("🔄 <b>Миграция v2</b> стартовала из бота. Контур: <b>%s</b>", contour),
	})
	for {
		st, cont, err := h.paidOps.MigrationV2OneStep(ctx)
		if err != nil {
			if errors.Is(err, usecase.ErrOpsLockBusy) {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: dest, ParseMode: models.ParseModeHTML,
					Text:   "⏳ Миграция уже выполняется в другом процессе.",
				})
				return
			}
			if err == usecase.ErrMigrationV2AlreadyDone {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: dest, ParseMode: models.ParseModeHTML,
					Text:   "ℹ️ Миграция v2 уже отмечена как завершённая. Нужен <code>/ops_proxy_migrate_reset</code> для повтора.",
				})
				return
			}
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: dest, ParseMode: models.ParseModeHTML,
				Text:   fmt.Sprintf("❌ <b>Ошибка шага миграции</b>\n<code>%v</code>", err),
			})
			return
		}
		if !cont {
			html := usecase.MigrationV2ProgressReportHTML(st, contour)
			if html != "" {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: dest, Text: html, ParseMode: models.ParseModeHTML})
			}
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: dest, ParseMode: models.ParseModeHTML,
				Text:   "✅ <b>Миграция v2 завершена.</b>",
			})
			return
		}
		if time.Since(lastReport) >= opsMigrateReportEvery {
			html := usecase.MigrationV2ProgressReportHTML(st, contour)
			if html != "" {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: dest, Text: html, ParseMode: models.ParseModeHTML})
			}
			lastReport = time.Now()
		}
		time.Sleep(opsMigrateStepDelay)
	}
}

// HandleOpsProxyMigrateReset — /ops_proxy_migrate_reset
func (h *BotHandler) HandleOpsProxyMigrateReset(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || h.settingsRepo == nil {
		h.sendText(ctx, b, update, "❌ Недоступно.")
		return
	}
	if err := usecase.ResetPaidMigrationV2Markers(h.settingsRepo); err != nil {
		h.sendText(ctx, b, update, fmt.Sprintf("❌ %v", err))
		return
	}
	h.sendText(ctx, b, update, "✅ Маркеры миграции v2 сброшены. Запустите <code>/ops_proxy_migrate</code>.")
}
