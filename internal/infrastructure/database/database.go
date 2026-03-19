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
		&domain.ProGroup{},
		&domain.ProSubscription{},
		&domain.PremiumServer{},
		&domain.VPSProvisionRequest{},
		&domain.MaintenanceWaitUser{},
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

	// Защита от дубликатов в «Мои прокси» (user_proxies): уникальна комбинация (user_id, ip, port, secret).
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_user_proxies_unique
		ON user_proxies (user_id, ip, port, secret)
	`).Error; err != nil {
		return err
	}

	// Уникальный порт для Pro-групп (dd). Порт ee вычисляется как PortDD+10000.
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_pro_groups_port_dd_unique
		ON pro_groups (port_dd)
	`).Error; err != nil {
		return err
	}

	// Pro: инфраструктура по сроку; не более одной active-группы на календарный день (date = 00:00 UTC).
	if err := db.Exec(`
		ALTER TABLE pro_groups ADD COLUMN IF NOT EXISTS infrastructure_expires_at TIMESTAMPTZ
	`).Error; err != nil {
		return err
	}
	if err := db.Exec(`
		UPDATE pro_groups SET infrastructure_expires_at = created_at + interval '30 days'
		WHERE infrastructure_expires_at IS NULL
	`).Error; err != nil {
		return err
	}
	_ = db.Exec(`ALTER TABLE pro_groups ALTER COLUMN infrastructure_expires_at SET NOT NULL`).Error
	_ = db.Exec(`DROP INDEX IF EXISTS idx_pro_groups_date`)
	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_pro_groups_date ON pro_groups (date)`).Error; err != nil {
		return err
	}
	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_pro_groups_infra_exp ON pro_groups (infrastructure_expires_at)`).Error; err != nil {
		return err
	}
	_ = db.Exec(`DROP INDEX IF EXISTS idx_pro_groups_one_active_per_date`)
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_pro_groups_one_active_per_date
		ON pro_groups (date) WHERE status = 'active'
	`).Error; err != nil {
		return err
	}

	// TimeWeb Premium: индексы для поиска по floating IP и server_id.
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_proxy_nodes_timeweb_floating_ip_id
		ON proxy_nodes (timeweb_floating_ip_id)
		WHERE timeweb_floating_ip_id <> ''
	`).Error; err != nil {
		return err
	}
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_proxy_nodes_premium_server_id
		ON proxy_nodes (premium_server_id)
		WHERE premium_server_id IS NOT NULL
	`).Error; err != nil {
		return err
	}

	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_vps_provision_requests_status_created_at
		ON vps_provision_requests (status, created_at)
	`).Error; err != nil {
		return err
	}

	if err := db.Exec(`
		CREATE TABLE IF NOT EXISTS op_channel_clicks (
			id         BIGSERIAL PRIMARY KEY,
			channel    VARCHAR(255) NOT NULL,
			user_tg_id BIGINT NOT NULL,
			clicked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`).Error; err != nil {
		return err
	}
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_op_channel_clicks_channel ON op_channel_clicks(channel)
	`).Error; err != nil {
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
		"pro_days":        "30",
		"pro_price_usdt":  "3",
		"pro_price_stars": "50",
		"instruction_text": "📖 <b>Инструкция по использованию прокси</b>\n\n<b>1. Получите несколько прокси</b>\nНажмите «Получить прокси» 2-3 раза — вы получите разные прокси-серверы.\n\n<b>2. Добавьте все прокси в Telegram</b>\nНажмите «Подключиться» под каждым прокси и включите его.\n\n<b>3. Включите автопереключение</b>\nНастройки → Конфиденциальность и безопасность → Тип подключения → выберите все прокси.\n\nTelegram автоматически переключится на рабочий прокси при сбое!",
		"instruction_photo_id": "",
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
