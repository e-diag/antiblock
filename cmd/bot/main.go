package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/handler"
	"github.com/yourusername/antiblock/internal/handler/middleware"
	"github.com/yourusername/antiblock/internal/handler/webhook"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/database"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	appmetrics "github.com/yourusername/antiblock/internal/infrastructure/metrics"
	"github.com/yourusername/antiblock/internal/infrastructure/timeweb"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
	"github.com/yourusername/antiblock/internal/worker"
)

func main() {
	// Подхватываем переменные из файлов:
	// 1) общий .env, 2) .env.test с приоритетом (перекрывает совпадающие ключи).
	_ = godotenv.Load(".env")
	_ = godotenv.Overload(".env.test")

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if cfg.Database.Debug {
		log.Println("[WARN] database.debug=true — SQL queries (including proxy secrets) will be logged. Disable in production!")
	}

	if cfg.Telegram.BotToken == "" || strings.HasPrefix(cfg.Telegram.BotToken, "${") {
		log.Fatalf("Invalid config: TELEGRAM_BOT_TOKEN is required and must be set (e.g. in .env or environment)")
	}
	adminIDs := cfg.Telegram.GetAdminIDs()
	if len(adminIDs) == 0 {
		log.Fatalf("Invalid config: at least one Telegram admin ID is required (TELEGRAM_ADMIN_ID_1)")
	}
	log.Printf("Telegram: %d admin user(s) configured (maintenance bypass + /manager)", len(adminIDs))
	if cfg.Database.Host == "" {
		log.Fatalf("Invalid config: database host is required (DB_HOST)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := database.New(&cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	userRepo := repository.NewUserRepository(db.DB)
	userProxyRepo := repository.NewUserProxyRepository(db.DB)
	proxyRepo := repository.NewProxyRepository(db.DB)
	adRepo := repository.NewAdRepository(db.DB)
	adPinRepo := repository.NewAdPinRepository(db.DB)
	invoiceRepo := repository.NewInvoiceRepository(db.DB)
	starPaymentRepo := repository.NewStarPaymentRepository(db.DB)
	settingsRepo := repository.NewSettingsRepository(db.DB)
	maintenanceWaitRepo := repository.NewMaintenanceWaitRepository(db.DB)
	opStatsRepo := repository.NewOPStatsRepository(db.DB)

	proxyUC := usecase.NewProxyUseCase(proxyRepo, userProxyRepo)
	proGroupRepo := repository.NewProGroupRepository(db.DB)
	proSubRepo := repository.NewProSubscriptionRepository(db.DB)
	proUC := usecase.NewProUseCase(proGroupRepo, proSubRepo, proxyRepo, userRepo)

	pd := cfg.ProDocker
	var proDockerMgr *docker.Manager
	if pd.Host != "" && pd.CertPath != "" {
		required := []string{"ca.pem", "cert.pem", "key.pem"}
		var missing []string
		for _, name := range required {
			if _, err := os.Stat(filepath.Join(pd.CertPath, name)); err != nil {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			log.Printf("Pro Docker disabled: missing certs in %q: %v", pd.CertPath, missing)
		} else {
			port := pd.Port
			if port <= 0 {
				port = 2376
			}
			var errDocker error
			proDockerMgr, errDocker = docker.NewManagerTLS(pd.Host, port, pd.CertPath)
			if errDocker != nil {
				log.Printf("Failed to init Pro Docker manager: %v", errDocker)
			} else {
				log.Printf("Pro Docker manager initialized: %s:%d", pd.Host, port)
			}
		}
	}
	proServerIP := pd.ServerIP

	var twClient *timeweb.Client
	var premiumProvisioner *usecase.PremiumProvisioner
	var vpsReqRepo repository.VPSProvisionRequestRepository
	var premiumServerRepo repository.PremiumServerRepository
	if cfg.Timeweb.APIToken != "" {
		twClient = timeweb.NewClient(cfg.Timeweb.APIToken)
		vpsReqRepo = repository.NewVPSProvisionRequestRepository(db.DB)
		premiumServerRepo = repository.NewPremiumServerRepository(db.DB)
		premiumProvisioner = usecase.NewPremiumProvisioner(
			twClient,
			premiumServerRepo,
			vpsReqRepo,
			cfg.Timeweb.SSHUser,
			cfg.Timeweb.SSHKeyPath,
			cfg.Timeweb.SSHKeyID,
			cfg.Timeweb.AvailabilityZone,
		)
		log.Println("TimeWeb Premium provisioner initialized")
	} else {
		log.Println("TIMEWEB_API_TOKEN not set — новый Premium через TimeWeb недоступен до настройки токена")
	}

	userUC := usecase.NewUserUseCase(userRepo, proxyRepo, proxyUC, proDockerMgr, proServerIP, userProxyRepo, premiumProvisioner, ctx)
	// Платежи TON через xRocket Pay API.
	paymentUC := usecase.NewPaymentUseCase(cfg.XRocket.APIToken, cfg.XRocket.APIURL, invoiceRepo, starPaymentRepo)

	broadcastState := handler.NewBroadcastState()
	broadcastMediaGroup := handler.NewBroadcastMediaGroupBuffer()
	adComposeState := handler.NewAdComposeState()
	botHandler := handler.NewBotHandler(
		userUC, proxyUC, proUC, paymentUC,
		userRepo, userProxyRepo,
		proxyRepo,
		adRepo, adPinRepo,
		settingsRepo, opStatsRepo,
		maintenanceWaitRepo,
		proDockerMgr,
		proServerIP,
		cfg.Telegram.ForcedSubscriptionChannel,
		broadcastState, broadcastMediaGroup, adComposeState,
		twClient,
		premiumProvisioner,
		vpsReqRepo,
		premiumServerRepo,
		cfg.Timeweb.SSHKeyPath,
		cfg.Telegram.GetAdminIDs(),
	)
	adminMiddleware := middleware.AdminMiddleware(cfg.Telegram.GetAdminIDs())
	rateMW := middleware.NewRateLimiter(cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.BurstSize).Middleware
	maintMW := middleware.MaintenanceMiddleware(settingsRepo, maintenanceWaitRepo, cfg.Telegram.GetAdminIDs())
	chainMW := func(next bot.HandlerFunc) bot.HandlerFunc {
		return maintMW(rateMW(next))
	}

	opts := []bot.Option{
		bot.WithMiddlewares(chainMW),
		bot.WithDefaultHandler(botHandler.DefaultHandler),
	}

	b, err := bot.New(cfg.Telegram.BotToken, opts...)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}
	botHandler.SetBot(b)

	proUC.SetOnProRotated(func(tgID int64, g *domain.ProGroup) {
		if g == nil {
			return
		}
		bg := context.Background()
		_, _ = b.SendMessage(bg, &bot.SendMessageParams{
			ChatID: tgID, ParseMode: models.ParseModeHTML,
			Text: "🔄 <b>Pro: ключи обновлены</b> (плановая ротация на сервере)\n\nВот ваши новые прокси:",
		})
		botHandler.SendProGroupProxiesToUser(bg, b, tgID, g)
	})

	// После успешного асинхронного создания премиум-прокси отправляем dd+ee (через общий helper в handler).
	usecase.SetOnPremiumProxyReady(userUC, func(tgID int64, proxy *domain.ProxyNode) {
		if proxy == nil {
			return
		}
		user, err := userRepo.GetByTGID(tgID)
		if err != nil || user == nil {
			return
		}
		botHandler.SendPremiumProxyToUser(context.Background(), b, tgID, user, proxy)
	})

	// При достижении daily floating IP лимита пользователь попадает в очередь и не должен получать «не удалось».
	usecase.SetOnPremiumProxyFailed(userUC, func(tgID int64, err error) {
		if err == nil {
			return
		}
		var msg string
		if errors.Is(err, usecase.ErrFloatingIPDailyLimit) ||
			errors.Is(err, usecase.ErrProvisionerNotConfigured) {
			msg = "⏳ Ваш персональный прокси будет создан в ближайшее время — мы уведомим вас, как только он будет готов."
		} else {
			msg = "⚠️ Премиум активирован, но создание прокси не удалось. Нажмите «Получить Premium proxy» в меню для повторной попытки."
		}
		_, _ = b.SendMessage(context.Background(), &bot.SendMessageParams{
			ChatID: tgID, Text: msg, ParseMode: models.ParseModeHTML,
		})
	})

	// Если исчерпан лимит floating IP — уведомляем всех админов кнопкой создания нового Premium VPS.
	usecase.SetOnPremiumVPSRequested(userUC, func(req *domain.VPSProvisionRequest) {
		if req == nil || req.ID == 0 {
			return
		}
		var tgIDs []int64
		_ = json.Unmarshal([]byte(req.PendingUserIDs), &tgIDs)

		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "🛠 Создать Premium VPS", CallbackData: fmt.Sprintf("mgr_vps_create_%d", req.ID)}},
			},
		}
		msg := fmt.Sprintf(
			"💎 TimeWeb Premium: исчерпан лимит floating IP.\nВ очереди: <b>%d</b> пользователей.\n\nНажмите кнопку, чтобы создать новый VPS (request ID: <code>%d</code>).",
			len(tgIDs), req.ID,
		)

		for _, adminID := range cfg.Telegram.GetAdminIDs() {
			_, _ = b.SendMessage(context.Background(), &bot.SendMessageParams{
				ChatID: adminID, Text: msg, ParseMode: models.ParseModeHTML, ReplyMarkup: kb,
			})
		}
	})

	// Пользовательские команды
	b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeExact, botHandler.HandleStart)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/getproxy", bot.MatchTypeExact, botHandler.HandleGetProxy)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/buy", bot.MatchTypeExact, botHandler.HandleBuyPremium)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/cancel", bot.MatchTypeExact, botHandler.HandleCancel)

	// Админ-команды (панель менеджера + текстовые команды)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/manager", bot.MatchTypeExact, adminMiddleware(botHandler.HandleManager))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/addproxy", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleAddProxy))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/delproxy", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleDelProxy))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/proxies", bot.MatchTypeExact, adminMiddleware(botHandler.HandleProxies))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/stats", bot.MatchTypeExact, adminMiddleware(botHandler.HandleStats))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/admin_stats", bot.MatchTypeExact, adminMiddleware(botHandler.HandleAdminStats))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/admin_info", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleAdminInfo))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/admin_rebuild", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleAdminRebuild))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/broadcast", bot.MatchTypeExact, adminMiddleware(botHandler.HandleBroadcast))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/sendad", bot.MatchTypeExact, adminMiddleware(botHandler.HandleSendAd))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/subs", bot.MatchTypeExact, adminMiddleware(botHandler.HandleSubs))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/grantpremium", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleGrantPremium))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/revokepremium", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleRevokePremium))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/premium_info", bot.MatchTypePrefix, adminMiddleware(botHandler.HandlePremiumInfo))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/replace_ip", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleReplaceIP))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setup_ssh_key", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetupSSHKey))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setpricing", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetPricing))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setprice_usdt", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetPriceUSDT))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setprice_stars", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetPriceStars))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setpro_days", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetProDays))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setpro_price_usdt", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetProPriceUSDT))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setpro_price_stars", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetProPriceStars))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/set_instruction_text", bot.MatchTypeExact, adminMiddleware(botHandler.HandleSetInstructionText))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/set_instruction_photo", bot.MatchTypeExact, adminMiddleware(botHandler.HandleSetInstructionPhoto))

	// Callback-кнопки
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "mgr_", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleManagerCallback))
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "broadcast_audience_", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleManagerCallback))
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_pro", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_pro_usdt", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_pro_stars", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_premium", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_premium_usdt", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_stars", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_extra_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_pro_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "my_proxies", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "my_proxy_", bot.MatchTypePrefix, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_premium_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "check_sub_forced", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "cancel_payment", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "show_instructions", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "back_to_main", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "reminder_later", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "ad_click_", bot.MatchTypePrefix, botHandler.HandleCallback)

	// Платежи: PreCheckoutQuery и Message с SuccessfulPayment
	b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.PreCheckoutQuery != nil
	}, botHandler.HandlePreCheckout)
	b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.Message != nil && update.Message.SuccessfulPayment != nil
	}, botHandler.HandleSuccessfulPayment)

	healthCheckWorker := worker.NewHealthCheckWorker(proxyUC, b, cfg.Telegram.GetAdminIDs(), cfg.Workers.HealthCheck)
	premiumHealthCheckWorker := worker.NewPremiumHealthCheckWorker(b, proxyUC, cfg.Telegram.GetAdminIDs(), cfg.Workers.PremiumHealthCheck)
	subscriptionWorker := worker.NewSubscriptionWorker(userUC, cfg.Workers.SubscriptionChecker)
	premiumReminderWorker := worker.NewPremiumReminderWorker(b, userUC, paymentUC, settingsRepo, cfg.Workers.PremiumReminder)
	dockerMonitorWorker := worker.NewDockerMonitorWorker(b, cfg.Telegram.GetAdminIDs(), cfg.Workers.DockerMonitor, proDockerMgr)
	adRePinWorker := worker.NewAdRePinWorker(b, adRepo, adPinRepo, cfg.Workers.AdRePin)
	invoiceCleanupWorker := worker.NewInvoiceCleanupWorker(b, invoiceRepo, paymentUC, cfg.Workers.InvoiceCleanup)

	go healthCheckWorker.Start()
	go premiumHealthCheckWorker.Start()
	go subscriptionWorker.Start()
	go premiumReminderWorker.Start()
	go dockerMonitorWorker.Start()
	go adRePinWorker.Start()
	go invoiceCleanupWorker.Start()

	// Webhook xRocket Pay для подтверждения успешной оплаты TON.
	if cfg.XRocket.WebhookPort != "" {
		port, _ := strconv.Atoi(cfg.XRocket.WebhookPort)
		if port > 0 {
			mux := http.NewServeMux()
			getPremiumDays := func() int {
				v, _ := settingsRepo.Get("premium_days")
				if v == "" {
					return 30
				}
				n, _ := strconv.Atoi(v)
				if n < 1 {
					return 30
				}
				return n
			}
			getProDays := func() int {
				v, _ := settingsRepo.Get("pro_days")
				if v == "" {
					return 30
				}
				n, _ := strconv.Atoi(v)
				if n < 1 {
					return 30
				}
				return n
			}
			activatePremium := func(tgID int64, days int) error {
				return userUC.ActivatePremium(tgID, days)
			}
			activatePro := func(tgID int64, days int) (*domain.ProGroup, bool, error) {
				if proServerIP == "" {
					return nil, false, fmt.Errorf("pro server ip is empty")
				}
				u, err := userRepo.GetByTGID(tgID)
				if err != nil || u == nil {
					return nil, false, fmt.Errorf("user not found")
				}
				return proUC.ActivateProSubscription(u, days, proServerIP, proDockerMgr, getProDays())
			}
			mux.HandleFunc("/webhook/xrocket", webhook.XRocketWebhook(activatePremium, activatePro, paymentUC, cfg.XRocket.APIToken, getPremiumDays, getProDays, b))
			srv := &http.Server{Addr: ":" + cfg.XRocket.WebhookPort, Handler: mux}
			go func() {
				log.Printf("xRocket webhook listening on :%s", cfg.XRocket.WebhookPort)
				_ = srv.ListenAndServe()
			}()
		}
	}

	// Ротация Pro-групп: через pro_days снимаем контейнеры, активных подписчиков переносим в новую группу.
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		proCycle := func() int {
			v, _ := settingsRepo.Get("pro_days")
			if v == "" {
				return 30
			}
			n, _ := strconv.Atoi(v)
			if n < 1 {
				return 30
			}
			return n
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = proUC.CleanupExpiredGroups(proDockerMgr, proCycle())
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		adRePinWorker.Stop()
		invoiceCleanupWorker.Stop()
		healthCheckWorker.Stop()
		premiumHealthCheckWorker.Stop()
		subscriptionWorker.Stop()
		premiumReminderWorker.Stop()
		dockerMonitorWorker.Stop()
		cancel()
	}()

	// Горутина обновления метрик каждые 30 сек
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if stats, err := proxyUC.GetStats(); err == nil {
					appmetrics.ActivePremiumProxies.Set(float64(stats.PremiumProxies))
					appmetrics.UnreachablePremiumProxies.Set(float64(stats.UnreachablePremiumCount))
				}
				if allProxies, err := proxyUC.GetAllFreeProxies(); err == nil {
					activeCount, inactiveCount := 0, 0
					for _, p := range allProxies {
						if p.Status == domain.ProxyStatusActive {
							activeCount++
						} else {
							inactiveCount++
						}
						portStr := strconv.Itoa(p.Port)
						appmetrics.FreeProxyLoad.WithLabelValues(p.IP, portStr).Set(float64(p.Load))
					}
					appmetrics.ActiveFreeProxies.Set(float64(activeCount))
					appmetrics.InactiveFreeProxies.Set(float64(inactiveCount))
				}
				if count, err := userRepo.Count(); err == nil {
					appmetrics.TotalUsers.Set(float64(count))
				}
				if premiumUsers, err := userRepo.GetPremiumUsers(); err == nil {
					appmetrics.PremiumUsers.Set(float64(len(premiumUsers)))
				}
			}
		}
	}()

	// HTTP-сервер метрик с поддержкой graceful shutdown по контексту.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		srv := &http.Server{Addr: ":9090", Handler: mux}
		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				log.Printf("Metrics server shutdown error: %v", err)
			}
		}()
		log.Println("Prometheus metrics at :9090/metrics")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Metrics server error: %v", err)
		}
	}()

	log.Println("Bot started successfully!")
	b.Start(ctx)
}
