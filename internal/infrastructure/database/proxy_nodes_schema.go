package database

import (
	"fmt"

	"gorm.io/gorm"
)

// ensureProxyNodesSchema создаёт/дополняет proxy_nodes только через SQL — без Migrator.CreateTable/AddColumn
// и без db.AutoMigrate(&ProxyNode{}): иначе GORM на PostgreSQL может выполнить
// ALTER TABLE ... DROP CONSTRAINT "uni_proxy_nodes_owner_id" без IF EXISTS (SQLSTATE 42704).

// ProxyNodesSchemaVersion метка для логов при старте (проверка, что образ не старый).
const ProxyNodesSchemaVersion = "proxy_nodes_schema_v3_raw_sql"

func ensureProxyNodesSchema(db *gorm.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS proxy_nodes (
			id BIGSERIAL PRIMARY KEY,
			ip VARCHAR(255) NOT NULL,
			port INTEGER NOT NULL,
			secret VARCHAR(255) NOT NULL,
			secret_ee VARCHAR(255),
			type VARCHAR(20) NOT NULL,
			floating_ip VARCHAR(45),
			timeweb_floating_ip_id VARCHAR(64) DEFAULT '',
			premium_server_id BIGINT,
			owner_id BIGINT,
			container_name VARCHAR(255),
			status VARCHAR(20) NOT NULL DEFAULT 'active',
			load INTEGER NOT NULL DEFAULT 0,
			last_rtt_ms INTEGER,
			unreachable_since TIMESTAMPTZ,
			last_check TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		// Очень старые схемы: дозаполняем базовые колонки.
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS ip VARCHAR(255) NOT NULL DEFAULT ''`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS port INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS secret VARCHAR(255) NOT NULL DEFAULT ''`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS type VARCHAR(20) NOT NULL DEFAULT 'free'`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS status VARCHAR(20) NOT NULL DEFAULT 'active'`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS load INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`,
		// Старые БД без части колонок — добавляем безопасно.
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS secret_ee VARCHAR(255)`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS floating_ip VARCHAR(45)`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS timeweb_floating_ip_id VARCHAR(64) DEFAULT ''`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS premium_server_id BIGINT`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS owner_id BIGINT`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS container_name VARCHAR(255)`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS last_rtt_ms INTEGER`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS unreachable_since TIMESTAMPTZ`,
		`ALTER TABLE proxy_nodes ADD COLUMN IF NOT EXISTS last_check TIMESTAMPTZ`,
	}
	for _, q := range stmts {
		if err := db.Exec(q).Error; err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}
