package database

import (
	"fmt"
	"log"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
)

const dbRetryAttempts = 5
const dbRetryInterval = 2 * time.Second

// DB представляет подключение к базе данных
type DB struct {
	*gorm.DB
}

// New создает новое подключение к базе данных (с повтором при временной недоступности).
func New(cfg *config.DatabaseConfig) (*DB, error) {
	logLevel := logger.Warn
	if cfg.Debug {
		logLevel = logger.Info
	}

	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	}

	var db *gorm.DB
	var err error
	for attempt := 1; attempt <= dbRetryAttempts; attempt++ {
		db, err = gorm.Open(postgres.Open(cfg.DSN()), gormConfig)
		if err == nil {
			break
		}
		if attempt < dbRetryAttempts {
			log.Printf("Database connect attempt %d/%d failed: %v; retrying in %v", attempt, dbRetryAttempts, err, dbRetryInterval)
			time.Sleep(dbRetryInterval)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database after %d attempts: %w", dbRetryAttempts, err)
	}

	// Автомиграция моделей (без потери данных)
	if err := db.AutoMigrate(
		&domain.User{},
		&domain.ProxyNode{},
		&domain.UserProxy{},
		&domain.Ad{},
		&domain.AdPin{},
		&domain.Invoice{},
		&domain.StarPayment{},
		&domain.AppSetting{},
	); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	// Дополнительные миграции данных (обновление значений type/status в proxy_nodes и т.п.)
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("failed to run data migrations: %w", err)
	}

	return &DB{DB: db}, nil
}

// runMigrations выполняет безопасные миграции данных поверх AutoMigrate.
func runMigrations(db *gorm.DB) error {
	// Удаляем старый уникальный индекс по порту: у free-прокси порты могут повторяться, уникальна комбинация (ip, port, secret).
	if err := db.Exec("DROP INDEX IF EXISTS idx_proxy_nodes_port").Error; err != nil {
		return err
	}

	// Приводим типы прокси к нижнему регистру (Free/Premium -> free/premium)
	if err := db.Exec(
		"UPDATE proxy_nodes SET type = LOWER(type) WHERE type IN ('Free', 'Premium')",
	).Error; err != nil {
		return err
	}

	// Приводим статусы к нижнему регистру (Active/Inactive/Blocked -> active/inactive/blocked)
	if err := db.Exec(
		"UPDATE proxy_nodes SET status = LOWER(status) WHERE status IN ('Active', 'Inactive', 'Blocked')",
	).Error; err != nil {
		return err
	}

	// Индекс для ускорения выборок/очистки по premium_until
	if err := db.Exec(
		"CREATE INDEX IF NOT EXISTS idx_users_premium_until ON users (premium_until) WHERE premium_until IS NOT NULL",
	).Error; err != nil {
		return err
	}

	if err := seedAppSettings(db); err != nil {
		return err
	}

	return nil
}

// seedAppSettings создаёт записи настроек по умолчанию, если их ещё нет (избегает "record not found" в логах).
func seedAppSettings(db *gorm.DB) error {
	defaults := map[string]string{
		"premium_days": "30",
		"premium_usdt": "10", // сумма в TON (ключ оставлен для совместимости)
		"premium_stars": "100",
	}
	for key, value := range defaults {
		var exists int64
		if err := db.Model(&domain.AppSetting{}).Where("key = ?", key).Count(&exists).Error; err != nil {
			return err
		}
		if exists == 0 {
			if err := db.Create(&domain.AppSetting{Key: key, Value: value}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// Close закрывает подключение к базе данных
func (d *DB) Close() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
