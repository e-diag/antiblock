package database

import (
	"fmt"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
)

// DB представляет подключение к базе данных
type DB struct {
	*gorm.DB
}

// New создает новое подключение к базе данных
func New(cfg *config.DatabaseConfig) (*DB, error) {
	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	}

	db, err := gorm.Open(postgres.Open(cfg.DSN()), gormConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Автомиграция моделей (без потери данных)
	if err := db.AutoMigrate(
		&domain.User{},
		&domain.ProxyNode{},
		&domain.Ad{},
		&domain.Invoice{},
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
// Здесь мы, в частности, нормализуем значения type/status для существующих записей proxy_nodes.
func runMigrations(db *gorm.DB) error {
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
