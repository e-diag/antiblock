package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/handler"
	"github.com/yourusername/antiblock/internal/handler/middleware"
	// "net/http"
	// "strconv"
	// "github.com/yourusername/antiblock/internal/handler/webhook" // CryptoPay отключён
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/database"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
	"github.com/yourusername/antiblock/internal/usecase"
	"github.com/yourusername/antiblock/internal/worker"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	db, err := database.New(&cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	userRepo := repository.NewUserRepository(db.DB)
	proxyRepo := repository.NewProxyRepository(db.DB)
	adRepo := repository.NewAdRepository(db.DB)
	adPinRepo := repository.NewAdPinRepository(db.DB)
	invoiceRepo := repository.NewInvoiceRepository(db.DB)
	settingsRepo := repository.NewSettingsRepository(db.DB)

	proxyUC := usecase.NewProxyUseCase(proxyRepo)
	var dockerMgr *docker.Manager
	pd := cfg.PremiumDocker
	if pd.Host != "" && pd.CertPath != "" {
		// Проверяем наличие файлов сертификатов (папка должна быть смонтирована через volume, DOCKER_CERTS_PATH на хосте).
		required := []string{"ca.pem", "cert.pem", "key.pem"}
		var missing []string
		for _, name := range required {
			if _, err := os.Stat(filepath.Join(pd.CertPath, name)); err != nil {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			log.Printf("Premium Docker disabled: cert path %q missing files: %v (on host: put ca.pem, cert.pem, key.pem in folder and set DOCKER_CERTS_PATH, mount volume to /antiblock/docker-certs)", pd.CertPath, missing)
		} else {
			port := pd.Port
			if port <= 0 {
				port = 2376
			}
			var errDocker error
			dockerMgr, errDocker = docker.NewManagerTLS(pd.Host, port, pd.CertPath)
			if errDocker != nil {
				log.Printf("Failed to init Docker TLS manager (premium): %v (premium containers will be disabled)", errDocker)
			}
		}
	} else {
		dockerMgr, _ = docker.NewManager()
	}
	premiumServerIP := pd.ServerIP

	userUC := usecase.NewUserUseCase(userRepo, proxyRepo, proxyUC, dockerMgr, premiumServerIP)
	paymentUC := usecase.NewPaymentUseCase(cfg.CryptoBot.APIToken, cfg.CryptoBot.APIURL, invoiceRepo)

	broadcastState := handler.NewBroadcastState()
	broadcastMediaGroup := handler.NewBroadcastMediaGroupBuffer()
	adComposeState := handler.NewAdComposeState()
	botHandler := handler.NewBotHandler(userUC, proxyUC, paymentUC, userRepo, adRepo, adPinRepo, settingsRepo, dockerMgr, cfg.Telegram.ForcedSubscriptionChannel, broadcastState, broadcastMediaGroup, adComposeState, cfg.Telegram.GetAdminIDs())
	adminMiddleware := middleware.AdminMiddleware(cfg.Telegram.GetAdminIDs())

	opts := []bot.Option{
		bot.WithMiddlewares(middleware.NewRateLimiter(
			cfg.RateLimit.RequestsPerSecond,
			cfg.RateLimit.BurstSize,
		).Middleware),
		bot.WithDefaultHandler(botHandler.DefaultHandler),
	}

	b, err := bot.New(cfg.Telegram.BotToken, opts...)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}
	botHandler.SetBot(b)

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
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setpricing", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetPricing))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setprice_usdt", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetPriceUSDT))
	b.RegisterHandler(bot.HandlerTypeMessageText, "/setprice_stars", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleSetPriceStars))

	// Callback-кнопки
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "mgr_", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleManagerCallback))
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "broadcast_audience_", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleManagerCallback))
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_premium", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_stars", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_premium_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "check_sub_forced", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "cancel_payment", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "reminder_later", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "ad_click_", bot.MatchTypePrefix, botHandler.HandleCallback)

	// Платежи: PreCheckoutQuery и Message с SuccessfulPayment
	b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.PreCheckoutQuery != nil
	}, botHandler.HandlePreCheckout)
	b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.Message != nil && update.Message.SuccessfulPayment != nil
	}, botHandler.HandleSuccessfulPayment)

	healthCheckWorker := worker.NewHealthCheckWorker(proxyUC, cfg.Workers.HealthCheck)
	subscriptionWorker := worker.NewSubscriptionWorker(userUC, cfg.Workers.SubscriptionChecker)
	premiumReminderWorker := worker.NewPremiumReminderWorker(b, userUC, paymentUC, settingsRepo, cfg.Workers.PremiumReminder)
	dockerMonitorWorker := worker.NewDockerMonitorWorker(b, cfg.Telegram.GetAdminIDs(), cfg.Workers.DockerMonitor)
	adRePinWorker := worker.NewAdRePinWorker(b, adRepo, adPinRepo, cfg.Workers.AdRePin)

	go healthCheckWorker.Start()
	go subscriptionWorker.Start()
	go premiumReminderWorker.Start()
	go dockerMonitorWorker.Start()
	go adRePinWorker.Start()

	// CryptoPay отключён — оплата только через Telegram Stars.
	// if cfg.CryptoBot.WebhookPort != "" {
	// 	port, _ := strconv.Atoi(cfg.CryptoBot.WebhookPort)
	// 	if port > 0 {
	// 		mux := http.NewServeMux()
	// 		getPremiumDays := func() int {
	// 			v, _ := settingsRepo.Get("premium_days")
	// 			if v == "" { return 30 }
	// 			n, _ := strconv.Atoi(v)
	// 			if n < 1 { return 30 }
	// 			return n
	// 		}
	// 		mux.HandleFunc("/webhook/cryptopay", webhook.CryptoPayWebhook(userUC, paymentUC, cfg.CryptoBot.WebhookSecret, getPremiumDays))
	// 		srv := &http.Server{Addr: ":" + cfg.CryptoBot.WebhookPort, Handler: mux}
	// 		go func() {
	// 			log.Printf("CryptoPay webhook listening on :%s", cfg.CryptoBot.WebhookPort)
	// 			_ = srv.ListenAndServe()
	// 		}()
	// 	}
	// }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		adRePinWorker.Stop()
		healthCheckWorker.Stop()
		subscriptionWorker.Stop()
		premiumReminderWorker.Stop()
		dockerMonitorWorker.Stop()
		cancel()
	}()

	log.Println("Bot started successfully!")
	b.Start(ctx)
}
