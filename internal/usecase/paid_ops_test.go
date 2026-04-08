package usecase

import (
	"context"
	"testing"

	"github.com/yourusername/antiblock/internal/domain"
)

func TestIsLegacyPremiumProxy(t *testing.T) {
	t.Parallel()
	if !isLegacyPremiumProxy(&domain.ProxyNode{}) {
		t.Fatal("empty fip and server => legacy")
	}
	if isLegacyPremiumProxy(&domain.ProxyNode{TimewebFloatingIPID: "abc"}) {
		t.Fatal("with fip => not legacy")
	}
	ps := uint(1)
	if isLegacyPremiumProxy(&domain.ProxyNode{PremiumServerID: &ps, Port: domain.PremiumPortEE1}) {
		t.Fatal("timeweb port + premium server => not legacy")
	}
	if !isLegacyPremiumProxy(&domain.ProxyNode{PremiumServerID: &ps, Port: 20011}) {
		t.Fatal("legacy port 20000+ should be treated as legacy even with stale premium server id")
	}
}

func TestMigratePaidProxiesAlreadyDone(t *testing.T) {
	t.Parallel()
	s := &memSettingsStore{vals: map[string]string{SettingPaidMigrationDDToEEV1: "done"}}
	p := &PaidOps{Settings: s}
	err := p.MigratePaidProxiesToEE(context.Background())
	if err != ErrPaidMigrationAlreadyDone {
		t.Fatalf("expected ErrPaidMigrationAlreadyDone, got %v", err)
	}
}

type memSettingsStore struct {
	vals map[string]string
}

func (m *memSettingsStore) Get(key string) (string, error) {
	if m.vals == nil {
		return "", nil
	}
	return m.vals[key], nil
}

func (m *memSettingsStore) Set(key, value string) error {
	if m.vals == nil {
		m.vals = map[string]string{}
	}
	m.vals[key] = value
	return nil
}

func (m *memSettingsStore) Increment(key string, delta int) error {
	return nil
}
