package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config представляет конфигурацию приложения
type Config struct {
	App        AppConfig        `yaml:"app"`
	Telegram   TelegramConfig   `yaml:"telegram"`
	Database   DatabaseConfig   `yaml:"database"`
	CryptoBot  CryptoBotConfig  `yaml:"cryptobot"`
	RateLimit  RateLimitConfig  `yaml:"rate_limit"`
	Workers    WorkersConfig    `yaml:"workers"`
	Proxy      ProxyConfig      `yaml:"proxy"`
}

type AppConfig struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Debug   bool   `yaml:"debug"`
}

type TelegramConfig struct {
	BotToken string   `yaml:"bot_token"`
	AdminIDs []string `yaml:"admin_ids"`
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
}

func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode, d.Timezone)
}

type CryptoBotConfig struct {
	APIToken string `yaml:"api_token"`
	APIURL   string `yaml:"api_url"`
}

type RateLimitConfig struct {
	RequestsPerSecond int `yaml:"requests_per_second"`
	BurstSize         int `yaml:"burst_size"`
}

type WorkersConfig struct {
	HealthCheck       WorkerConfig `yaml:"health_check"`
	SubscriptionChecker WorkerConfig `yaml:"subscription_checker"`
}

type WorkerConfig struct {
	Enabled       bool `yaml:"enabled"`
	IntervalSeconds int `yaml:"interval_seconds"`
	TimeoutSeconds  int `yaml:"timeout_seconds"`
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

	// Заменяем переменные окружения
	content := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}
