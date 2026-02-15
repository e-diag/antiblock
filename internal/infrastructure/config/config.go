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
	App           AppConfig           `yaml:"app"`
	Telegram      TelegramConfig      `yaml:"telegram"`
	Database      DatabaseConfig      `yaml:"database"`
	CryptoBot     CryptoBotConfig     `yaml:"cryptobot"`
	RateLimit     RateLimitConfig     `yaml:"rate_limit"`
	Workers       WorkersConfig       `yaml:"workers"`
	Proxy         ProxyConfig         `yaml:"proxy"`
	PremiumDocker PremiumDockerConfig `yaml:"premium_docker"`
}

// PremiumDockerConfig — подключение к премиум-серверу по TLS для создания персональных mtg-контейнеров.
type PremiumDockerConfig struct {
	Host     string `yaml:"host"`      // хост премиум-сервера (Docker daemon)
	Port     int    `yaml:"port"`      // порт TLS, обычно 2376
	CertPath string `yaml:"cert_path"` // путь к сертификатам, например /antiblock/docker-certs/
	ServerIP string `yaml:"server_ip"` // IP сервера для записи в proxy_nodes (выдача пользователю)
}

type AppConfig struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Debug   bool   `yaml:"debug"`
}

type TelegramConfig struct {
	BotToken                  string   `yaml:"bot_token"`
	AdminIDs                  []string `yaml:"admin_ids"`
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

type RateLimitConfig struct {
	RequestsPerSecond int `yaml:"requests_per_second"`
	BurstSize         int `yaml:"burst_size"`
}

type WorkersConfig struct {
	HealthCheck         WorkerConfig              `yaml:"health_check"`
	SubscriptionChecker WorkerConfig              `yaml:"subscription_checker"`
	DockerMonitor       WorkerConfig              `yaml:"docker_monitor"`
	PremiumReminder     WorkerConfig              `yaml:"premium_reminder"`
	AdRePin             WorkerConfig              `yaml:"ad_repin"`
	PremiumHealthCheck  PremiumHealthCheckConfig  `yaml:"premium_health_check"`
}

// PremiumHealthCheckConfig — интервалы проверки премиум-прокси: полная проверка и перепроверка недоступных.
type PremiumHealthCheckConfig struct {
	Enabled                   bool `yaml:"enabled"`
	IntervalSeconds           int  `yaml:"interval_seconds"`            // полная проверка активных премиум (например 900 = 15 мин)
	UnreachableRecheckSeconds int  `yaml:"unreachable_recheck_seconds"` // перепроверка недоступных (например 300 = 5 мин)
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

	return &cfg, nil
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
