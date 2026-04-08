package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config представляет конфигурацию приложения
type Config struct {
	App       AppConfig       `yaml:"app"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	Database  DatabaseConfig  `yaml:"database"`
	CryptoBot CryptoBotConfig `yaml:"cryptobot"` // устаревший блок (CryptoPay), можно не заполнять
	XRocket   XRocketConfig   `yaml:"xrocket"`
	YooKassa  YooKassaConfig  `yaml:"yookassa"`
	Timeweb   TimewebConfig   `yaml:"timeweb"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Workers   WorkersConfig   `yaml:"workers"`
	Proxy     ProxyConfig     `yaml:"proxy"`
	ProDocker ProDockerConfig `yaml:"pro_docker"`
}

// ProDockerConfig — Pro-сервер (Free + Pro-группы + legacy Premium cleanup) по Docker TLS.
type ProDockerConfig struct {
	Host     string `yaml:"host"`      // хост премиум-сервера (Docker daemon)
	Port     int    `yaml:"port"`      // порт TLS, обычно 2376
	CertPath string `yaml:"cert_path"` // путь к сертификатам, например /antiblock/docker-certs/
	ServerIP string `yaml:"server_ip"` // IP сервера для записи в proxy_nodes (выдача пользователю)
}

// TimewebConfig — настройки TimeWeb Cloud API для Premium provisioning.
type TimewebConfig struct {
	APIToken          string `yaml:"api_token"`
	AvailabilityZone  string `yaml:"availability_zone"`
	SSHUser           string `yaml:"ssh_user"`
	SSHKeyID          int    `yaml:"ssh_key_id"`
	SSHKeyPath        string `yaml:"ssh_key_path"`
	PremiumServerOSID int    `yaml:"premium_server_os_id"` // 0 = авто Ubuntu 24.04 из /os/servers
}

type AppConfig struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Debug   bool   `yaml:"debug"`
}

type TelegramConfig struct {
	BotToken                  string   `yaml:"bot_token"`
	AdminIDs                  []string `yaml:"admin_ids"`
	// ErrorLogChatID — id чата/группы/канала для технических алертов (см. TELEGRAM_ERROR_LOG_CHAT_ID). Пусто = только лог.
	ErrorLogChatID string `yaml:"error_log_chat_id"`
	// ManagerProgressChatID — прогресс paidops (миграция dd→ee, компенсации). См. TELEGRAM_MANAGER_PROGRESS_CHAT_ID.
	ManagerProgressChatID string `yaml:"manager_progress_chat_id"`
	ForcedSubscriptionChannel string   `yaml:"forced_subscription_channel"` // @channel или username, пусто = отключено
}

func (t *TelegramConfig) GetAdminIDs() []int64 {
	ids := make([]int64, 0, len(t.AdminIDs))
	for _, idStr := range t.AdminIDs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// GetErrorLogChatID возвращает id служебного чата для технических ошибок; 0 = не настроено.
func (t *TelegramConfig) GetErrorLogChatID() int64 {
	if t == nil {
		return 0
	}
	s := strings.TrimSpace(t.ErrorLogChatID)
	if s == "" || strings.HasPrefix(s, "${") {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// GetManagerProgressChatID — чат для отчётов paidops (миграция, рассылки компенсации); 0 = не настроено.
func (t *TelegramConfig) GetManagerProgressChatID() int64 {
	if t == nil {
		return 0
	}
	s := strings.TrimSpace(t.ManagerProgressChatID)
	if s == "" || strings.HasPrefix(s, "${") {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
	Timezone string `yaml:"timezone"`
	// Debug включает подробные SQL-логи GORM (использовать только при отладке)
	Debug bool `yaml:"debug"`
}

func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode, d.Timezone)
}

type CryptoBotConfig struct {
	APIToken      string `yaml:"api_token"`
	APIURL        string `yaml:"api_url"`
	WebhookPort   string `yaml:"webhook_port"`   // порт для приёма webhook CryptoPay (например 8080)
	WebhookSecret string `yaml:"webhook_secret"` // секрет для проверки подписи CryptoPay webhook
}

// YooKassaConfig — настройки API ЮКассы (Smart Payment + webhook) и Telegram Payments.
type YooKassaConfig struct {
	ProviderToken string `yaml:"provider_token"`
	ShopID        string `yaml:"shop_id"`
	SecretKey     string `yaml:"secret_key"`
	ReturnURL     string `yaml:"return_url"`
	WebhookPort   string `yaml:"webhook_port"`
	WebhookToken  string `yaml:"webhook_token"`
}

// XRocketConfig — настройки интеграции с xRocket Pay API (TON).
type XRocketConfig struct {
	APIToken      string `yaml:"api_token"`      // API key из @xrocket_bot / @xrocket_testnet_bot
	APIURL        string `yaml:"api_url"`        // базовый URL API, по умолчанию https://pay.xrocket.tg/api
	WebhookPort   string `yaml:"webhook_port"`   // порт HTTP-сервера для приёма webhook xRocket (например 8081)
	WebhookSecret string `yaml:"webhook_secret"` // секрет для проверки подписи webhook (если включено в xRocket)
}

type RateLimitConfig struct {
	RequestsPerSecond int `yaml:"requests_per_second"`
	BurstSize         int `yaml:"burst_size"`
}

type WorkersConfig struct {
	HealthCheck         WorkerConfig             `yaml:"health_check"`
	SubscriptionChecker WorkerConfig             `yaml:"subscription_checker"`
	DockerMonitor       WorkerConfig             `yaml:"docker_monitor"`
	PremiumReminder     WorkerConfig             `yaml:"premium_reminder"`
	AdRePin             WorkerConfig             `yaml:"ad_repin"`
	InvoiceCleanup      WorkerConfig             `yaml:"invoice_cleanup"`
	PremiumHealthCheck  PremiumHealthCheckConfig `yaml:"premium_health_check"`
}

// PremiumHealthCheckConfig — интервалы проверки премиум-прокси: полная проверка и перепроверка недоступных.
type PremiumHealthCheckConfig struct {
	Enabled                   bool `yaml:"enabled"`
	IntervalSeconds           int  `yaml:"interval_seconds"`            // полная проверка активных премиум (например 900 = 15 мин)
	UnreachableRecheckSeconds int  `yaml:"unreachable_recheck_seconds"` // перепроверка недоступных (например 300 = 5 мин)
	// UnreachableAlertCooldownSeconds — не чаще одного алерта «прокси недоступен» на один proxy_id, пока не восстановится (0 = 4 ч).
	UnreachableAlertCooldownSeconds int `yaml:"unreachable_alert_cooldown_seconds"`
}

type WorkerConfig struct {
	Enabled         bool `yaml:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds"`
	TimeoutSeconds  int  `yaml:"timeout_seconds"`
}

func (w *WorkerConfig) Interval() time.Duration {
	return time.Duration(w.IntervalSeconds) * time.Second
}

func (w *WorkerConfig) Timeout() time.Duration {
	return time.Duration(w.TimeoutSeconds) * time.Second
}

type ProxyConfig struct {
	DefaultSecretLength int `yaml:"default_secret_length"`
	FreeTrialDays       int `yaml:"free_trial_days"`
}

// Load загружает конфигурацию из файла и переменных окружения
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Заменяем переменные окружения (только ${VAR}, без синтаксиса :default)
	content := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	applyDatabaseDefaults(&cfg.Database)
	trimDatabaseStrings(&cfg.Database)
	if cfg.CryptoBot.APIToken == "" {
		cfg.CryptoBot.APIToken = getEnv("CRYPTOBOT_API_TOKEN", "")
	}
	if cfg.CryptoBot.APIURL == "" {
		cfg.CryptoBot.APIURL = getEnv("CRYPTOBOT_API_URL", "https://pay.crypt.bot/api")
	}
	if cfg.CryptoBot.WebhookPort == "" {
		cfg.CryptoBot.WebhookPort = getEnv("CRYPTOBOT_WEBHOOK_PORT", "8080")
	}
	if cfg.CryptoBot.WebhookSecret == "" {
		cfg.CryptoBot.WebhookSecret = getEnv("CRYPTOBOT_WEBHOOK_SECRET", "")
	}

	// XRocket: заполняем значения по умолчанию и из ENV.
	if cfg.XRocket.APIToken == "" {
		cfg.XRocket.APIToken = getEnv("XROCKET_API_TOKEN", "")
	}
	if cfg.XRocket.APIURL == "" {
		// Базовый URL xRocket без /api; эндпоинты начинаются с /tg-invoices и т.п.
		cfg.XRocket.APIURL = getEnv("XROCKET_API_URL", "https://pay.xrocket.tg")
	}
	if cfg.XRocket.WebhookPort == "" {
		cfg.XRocket.WebhookPort = getEnv("XROCKET_WEBHOOK_PORT", "8081")
	}
	if cfg.XRocket.WebhookSecret == "" {
		cfg.XRocket.WebhookSecret = getEnv("XROCKET_WEBHOOK_SECRET", "")
	}

	if cfg.YooKassa.ProviderToken == "" {
		cfg.YooKassa.ProviderToken = getEnv("YOOKASSA_PROVIDER_TOKEN", "")
	}
	if cfg.YooKassa.ShopID == "" {
		cfg.YooKassa.ShopID = getEnv("YOOKASSA_SHOP_ID", "")
	}
	if cfg.YooKassa.SecretKey == "" {
		cfg.YooKassa.SecretKey = getEnv("YOOKASSA_SECRET_KEY", "")
	}
	if cfg.YooKassa.ReturnURL == "" {
		cfg.YooKassa.ReturnURL = getEnv("YOOKASSA_RETURN_URL", "")
	}
	if cfg.YooKassa.WebhookPort == "" {
		cfg.YooKassa.WebhookPort = getEnv("YOOKASSA_WEBHOOK_PORT", "8082")
	}
	if cfg.YooKassa.WebhookToken == "" {
		cfg.YooKassa.WebhookToken = getEnv("YOOKASSA_WEBHOOK_TOKEN", "")
	}

	// TimeWeb Premium (условно включаем по TIMEWEB_API_TOKEN).
	if cfg.Timeweb.APIToken == "" {
		cfg.Timeweb.APIToken = getEnv("TIMEWEB_API_TOKEN", "")
	}
	if cfg.Timeweb.AvailabilityZone == "" {
		cfg.Timeweb.AvailabilityZone = getEnv("TIMEWEB_AVAILABILITY_ZONE", "spb-3")
	}
	if cfg.Timeweb.SSHUser == "" {
		cfg.Timeweb.SSHUser = getEnv("TIMEWEB_SSH_USER", "root")
	}
	if cfg.Timeweb.SSHKeyID == 0 {
		if v := getEnv("TIMEWEB_SSH_KEY_ID", ""); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				cfg.Timeweb.SSHKeyID = n
			}
		}
	}
	if cfg.Timeweb.SSHKeyPath == "" {
		cfg.Timeweb.SSHKeyPath = getEnv("TIMEWEB_SSH_KEY_PATH", "/antiblock/premium-keys/premium_bot_key")
	}
	if cfg.Timeweb.PremiumServerOSID == 0 {
		if v := getEnv("TIMEWEB_PREMIUM_OS_ID", ""); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.Timeweb.PremiumServerOSID = n
			}
		}
	}
	cfg.Telegram.AdminIDs = mergeAdminIDs(cfg.Telegram.AdminIDs)
	cfg.Telegram.ErrorLogChatID = mergeErrorLogChatID(cfg.Telegram.ErrorLogChatID)
	cfg.Telegram.ManagerProgressChatID = mergeManagerProgressChatID(cfg.Telegram.ManagerProgressChatID)

	return &cfg, nil
}

// mergeErrorLogChatID: TELEGRAM_ERROR_LOG_CHAT_ID перекрывает значение из yaml.
func mergeErrorLogChatID(base string) string {
	if v := os.Getenv("TELEGRAM_ERROR_LOG_CHAT_ID"); v != "" {
		return trimQuotes(v)
	}
	return base
}

// mergeManagerProgressChatID: TELEGRAM_MANAGER_PROGRESS_CHAT_ID перекрывает yaml.
func mergeManagerProgressChatID(base string) string {
	if v := os.Getenv("TELEGRAM_MANAGER_PROGRESS_CHAT_ID"); v != "" {
		return trimQuotes(v)
	}
	return base
}

// applyDatabaseDefaults заполняет пустые поля Database из env с дефолтами
func applyDatabaseDefaults(d *DatabaseConfig) {
	if d.Host == "" {
		d.Host = getEnv("DB_HOST", "localhost")
	}
	if d.Port == "" {
		d.Port = getEnv("DB_PORT", "5432")
	}
	if d.User == "" {
		d.User = getEnv("DB_USER", "postgres")
	}
	if d.Password == "" {
		d.Password = getEnv("DB_PASSWORD", "postgres")
	}
	if d.DBName == "" {
		d.DBName = getEnv("DB_NAME", "antiblock")
	}
	if d.SSLMode == "" {
		d.SSLMode = getEnv("DB_SSLMODE", "disable")
	}
	if d.Timezone == "" {
		d.Timezone = getEnv("DB_TIMEZONE", "UTC")
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return trimQuotes(v)
	}
	return defaultVal
}

// trimQuotes убирает обрамляющие двойные/одинарные кавычки и пробелы
func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}

// trimDatabaseStrings применяет trimQuotes ко всем полям DatabaseConfig
func trimDatabaseStrings(d *DatabaseConfig) {
	d.Host = trimQuotes(d.Host)
	d.Port = trimQuotes(d.Port)
	d.User = trimQuotes(d.User)
	d.Password = trimQuotes(d.Password)
	d.DBName = trimQuotes(d.DBName)
	d.SSLMode = trimQuotes(d.SSLMode)
	d.Timezone = trimQuotes(d.Timezone)
}

// mergeAdminIDs объединяет admin_ids из config.yaml и окружения:
// - TELEGRAM_ADMIN_IDS="id1,id2,..."
// - TELEGRAM_ADMIN_ID_* (например TELEGRAM_ADMIN_ID_1, TELEGRAM_ADMIN_ID_2, ...)
func mergeAdminIDs(base []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(base)+4)
	add := func(v string) {
		v = strings.TrimSpace(trimQuotes(v))
		if v == "" || strings.HasPrefix(v, "${") {
			return
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	for _, v := range base {
		add(v)
	}

	if raw := os.Getenv("TELEGRAM_ADMIN_IDS"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			add(part)
		}
	}

	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TELEGRAM_ADMIN_ID_") {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		add(parts[1])
	}

	return out
}
