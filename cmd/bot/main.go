package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yourusername/antiblock/internal/handler"
	"github.com/yourusername/antiblock/internal/handler/middleware"
	"github.com/yourusername/antiblock/internal/handler/webhook"
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
	invoiceRepo := repository.NewInvoiceRepository(db.DB)

	dockerMgr, err := docker.NewManager()
	if err != nil {
		log.Printf("Failed to init Docker manager: %v (premium containers will be disabled)", err)
	}

	userUC := usecase.NewUserUseCase(userRepo, proxyRepo, dockerMgr)
	proxyUC := usecase.NewProxyUseCase(proxyRepo)
	paymentUC := usecase.NewPaymentUseCase(cfg.CryptoBot.APIToken, cfg.CryptoBot.APIURL, invoiceRepo)

	broadcastState := handler.NewBroadcastState()
	botHandler := handler.NewBotHandler(userUC, proxyUC, paymentUC, userRepo, adRepo, dockerMgr, broadcastState, cfg.Telegram.GetAdminIDs())
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

	// Callback-кнопки
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "mgr_", bot.MatchTypePrefix, adminMiddleware(botHandler.HandleManagerCallback))
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_premium", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "buy_stars", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "get_proxy", bot.MatchTypeExact, botHandler.HandleCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "cancel_payment", bot.MatchTypeExact, botHandler.HandleCallback)

	// Платежи: PreCheckoutQuery и Message с SuccessfulPayment
	b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.PreCheckoutQuery != nil
	}, botHandler.HandlePreCheckout)
	b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.Message != nil && update.Message.SuccessfulPayment != nil
	}, botHandler.HandleSuccessfulPayment)

	healthCheckWorker := worker.NewHealthCheckWorker(proxyUC, cfg.Workers.HealthCheck)
	subscriptionWorker := worker.NewSubscriptionWorker(userUC, cfg.Workers.SubscriptionChecker)
	dockerMonitorWorker := worker.NewDockerMonitorWorker(b, cfg.Telegram.GetAdminIDs(), cfg.Workers.DockerMonitor)

	go healthCheckWorker.Start()
	go subscriptionWorker.Start()
	go dockerMonitorWorker.Start()

	// Webhook для CryptoPay (оплата -> выдача премиума)
	if cfg.CryptoBot.WebhookPort != "" {
		port, _ := strconv.Atoi(cfg.CryptoBot.WebhookPort)
		if port > 0 {
			mux := http.NewServeMux()
			mux.HandleFunc("/webhook/cryptopay", webhook.CryptoPayWebhook(userUC, paymentUC, cfg.CryptoBot.WebhookSecret))
			srv := &http.Server{Addr: ":" + cfg.CryptoBot.WebhookPort, Handler: mux}
			go func() {
				log.Printf("CryptoPay webhook listening on :%s", cfg.CryptoBot.WebhookPort)
				_ = srv.ListenAndServe()
			}()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		healthCheckWorker.Stop()
		subscriptionWorker.Stop()
		dockerMonitorWorker.Stop()
		cancel()
	}()

	log.Println("Bot started successfully!")
	b.Start(ctx)
}
