// Command paidops — одноразовые операции: пошаговая миграция платных прокси dd→ee (v2), компенсация +14 дней.
// Запускать на сервере с доступом к БД и к нужным контурам (Docker Pro / TimeWeb Premium).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/database"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
)

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Overload(".env.test")

	migrateStep := flag.Bool("migrate-paid-ee-v2-step", false, "один шаг миграции v2 (resume-safe)")
	migrateDaemon := flag.Bool("migrate-paid-ee-v2-daemon", false, "цикл: шаг + пауза; отчёт в чат менеджеров раз в 10 мин")
	stepDelay := flag.Duration("migrate-step-delay", 5*time.Second, "пауза между шагами в daemon")
	compensate := flag.Bool("compensate-14d", false, "атомарно начислить +14 дн. и поставить маркер + очередь TG")
	compensateNotify := flag.Bool("compensate-notify-drain", false, "постепенно отправить очередь уведомлений о компенсации")
	notifyDelay := flag.Duration("compensate-notify-delay", 2*time.Second, "пауза между сообщениями Telegram")
	contour := flag.String("contour", "default", "метка контура в отчёте (например prod-docker / prod-timeweb)")
	flag.Parse()

	if !*migrateStep && !*migrateDaemon && !*compensate && !*compensateNotify {
		fmt.Fprintln(os.Stderr, "Укажите флаг: -migrate-paid-ee-v2-step | -migrate-paid-ee-v2-daemon | -compensate-14d | -compensate-notify-drain")
		os.Exit(2)
	}
	if *migrateStep && *migrateDaemon {
		log.Fatal("выберите один режим миграции: -migrate-paid-ee-v2-step или -migrate-paid-ee-v2-daemon")
	}

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Database.Host == "" {
		log.Fatalf("DB_HOST required")
	}

	db, err := database.New(&cfg.Database)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	settingsRepo := repository.NewSettingsRepository(db.DB)
	opsLockRepo := repository.NewOpsLockRepository(db.DB)
	userRepo := repository.NewUserRepository(db.DB)
	proxyRepo := repository.NewProxyRepository(db.DB)
	proGroupRepo := repository.NewProGroupRepository(db.DB)
	proSubRepo := repository.NewProSubscriptionRepository(db.DB)
	proxyUC := usecase.NewProxyUseCase(proxyRepo, repository.NewUserProxyRepository(db.DB))
	proUC := usecase.NewProUseCase(proGroupRepo, proSubRepo, proxyRepo, userRepo)

	pd := cfg.ProDocker
	var proDocker *docker.Manager
	if pd.Host != "" && pd.CertPath != "" {
		port := pd.Port
		if port <= 0 {
			port = 2376
		}
		proDocker, err = docker.NewManagerTLS(pd.Host, port, pd.CertPath)
		if err != nil {
			log.Fatalf("docker TLS: %v", err)
		}
	} else {
		log.Printf("[paidops] Pro Docker TLS не настроен — фазы Pro и legacy Premium на этом хосте недоступны")
	}

	premiumServerIP := pd.ServerIP

	var premiumProvisioner *usecase.PremiumProvisioner
	if cfg.Timeweb.APIToken != "" {
		twClient := timeweb.NewClient(cfg.Timeweb.APIToken)
		vpsReqRepo := repository.NewVPSProvisionRequestRepository(db.DB)
		premiumServerRepo := repository.NewPremiumServerRepository(db.DB)
		premiumProvisioner = usecase.NewPremiumProvisioner(
			twClient,
			premiumServerRepo,
			vpsReqRepo,
			cfg.Timeweb.SSHUser,
			cfg.Timeweb.SSHKeyPath,
			cfg.Timeweb.SSHKeyID,
			cfg.Timeweb.AvailabilityZone,
		)
		log.Println("[paidops] TimeWeb PremiumProvisioner инициализирован")
	} else {
		log.Println("[paidops] TIMEWEB_API_TOKEN пуст — фаза TimeWeb Premium пропускается, пока не настроен provisioner")
	}

	ops := &usecase.PaidOps{
		Settings:        settingsRepo,
		Users:           userRepo,
		Proxies:         proxyRepo,
		Subs:            proSubRepo,
		ProxyUC:         proxyUC,
		ProUC:           proUC,
		Docker:          proDocker,
		PremiumServerIP: premiumServerIP,
		Provisioner:     premiumProvisioner,
		Locker:          usecase.NewOpsLocker(opsLockRepo),
		LockOwner:       "paidops-cli",
	}

	token := cfg.Telegram.BotToken
	managerChatID := cfg.Telegram.GetManagerProgressChatID()
	sendManagerHTML := func(html string) {
		if token == "" || managerChatID == 0 {
			log.Printf("[paidops] manager report (no TG chat): %s", stripHTMLForLog(html))
			return
		}
		if err := sendTelegramMessage(token, managerChatID, html); err != nil {
			log.Printf("[paidops] manager report send: %v", err)
		}
	}

	ctx := context.Background()

	if *compensate {
		if err := usecase.Compensate14DaysTransactional(db.DB, ops); err != nil {
			if errors.Is(err, usecase.ErrOpsLockBusy) {
				log.Println("компенсация уже выполняется в другом процессе")
				os.Exit(0)
			}
			if err == usecase.ErrPaidCompensationAlreadyDone {
				log.Println("компенсация уже была применена (маркер в app_settings)")
				os.Exit(0)
			}
			log.Fatalf("compensate: %v", err)
		}
		log.Println("OK: +14 дней начислено в транзакции, маркер и очередь уведомлений записаны")
	}

	if *compensateNotify {
		if token == "" {
			log.Fatal("для -compensate-notify-drain нужен telegram.bot_token")
		}
		send := func(tgID int64, text string) error {
			return sendTelegramMessage(token, tgID, text)
		}
		n, err := usecase.RunCompensationNotifyDrain(settingsRepo, send, *notifyDelay)
		if err != nil {
			if err == usecase.ErrCompensationNoticeQueueEmpty {
				log.Println("очередь уведомлений пуста или уже отправлена")
				os.Exit(0)
			}
			log.Fatalf("notify drain: %v", err)
		}
		log.Printf("OK: отправлено уведомлений: %d", n)
	}

	if *migrateStep {
		if proDocker == nil && premiumProvisioner == nil {
			log.Fatal("нужен хотя бы Pro Docker или TimeWeb PremiumProvisioner")
		}
		st, cont, err := ops.MigrationV2OneStep(ctx)
		if err != nil {
			if errors.Is(err, usecase.ErrOpsLockBusy) {
				log.Println("миграция уже выполняется в другом процессе")
				os.Exit(0)
			}
			if err == usecase.ErrMigrationV2AlreadyDone {
				log.Println("миграция v2 уже завершена (маркер paid_migration_v2_done)")
				os.Exit(0)
			}
			log.Fatalf("migrate step: %v", err)
		}
		sendManagerHTML(usecase.MigrationV2ProgressReportHTML(st, *contour))
		if !cont {
			log.Println("OK: миграция v2 завершена")
			os.Exit(0)
		}
		log.Printf("шаг выполнен, continue=%v OK=%d Err=%d phase=%s", cont, st.OK, st.Err, st.Phase)
	}

	if *migrateDaemon {
		if proDocker == nil && premiumProvisioner == nil {
			log.Fatal("нужен хотя бы Pro Docker или TimeWeb PremiumProvisioner")
		}
		var lastReport time.Time
		for {
			st, cont, err := ops.MigrationV2OneStep(ctx)
			if err != nil {
				if errors.Is(err, usecase.ErrOpsLockBusy) {
					log.Println("миграция уже выполняется в другом процессе")
					time.Sleep(*stepDelay)
					continue
				}
				if err == usecase.ErrMigrationV2AlreadyDone {
					log.Println("миграция v2 уже завершена")
					os.Exit(0)
				}
				sendManagerHTML(fmt.Sprintf("❌ <b>paidops daemon</b> ошибка шага: <code>%v</code>\nКонтур: <b>%s</b>", err, *contour))
				log.Fatalf("migrate step: %v", err)
			}
			if !cont {
				sendManagerHTML(usecase.MigrationV2ProgressReportHTML(st, *contour))
				log.Println("OK: миграция v2 завершена")
				os.Exit(0)
			}
			if lastReport.IsZero() {
				lastReport = time.Now()
			}
			if time.Since(lastReport) >= 10*time.Minute {
				sendManagerHTML(usecase.MigrationV2ProgressReportHTML(st, *contour))
				lastReport = time.Now()
			}
			time.Sleep(*stepDelay)
		}
	}
}

func stripHTMLForLog(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

func sendTelegramMessage(botToken string, chatID int64, text string) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	body := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(u, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram HTTP %d", resp.StatusCode)
	}
	return nil
}
