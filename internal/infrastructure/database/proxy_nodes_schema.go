package database

import (
	"fmt"
	"reflect"

	"github.com/yourusername/antiblock/internal/domain"
	"gorm.io/gorm"
)

// ensureProxyNodesSchema поддерживает таблицу proxy_nodes без db.AutoMigrate(&ProxyNode{}):
// GORM при AutoMigrate генерирует ALTER TABLE ... DROP CONSTRAINT "uni_proxy_nodes_owner_id"
// без IF EXISTS и падает (SQLSTATE 42704), если такого ограничения в БД никогда не было.
func ensureProxyNodesSchema(db *gorm.DB) error {
	probe := &domain.ProxyNode{}
	if !db.Migrator().HasTable(probe) {
		return db.Migrator().CreateTable(probe)
	}
	t := reflect.TypeOf(domain.ProxyNode{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("gorm") == "-" {
			continue
		}
		if !db.Migrator().HasColumn(probe, f.Name) {
			if err := db.Migrator().AddColumn(probe, f.Name); err != nil {
				return fmt.Errorf("proxy_nodes add column %s: %w", f.Name, err)
			}
		}
	}
	return nil
}
