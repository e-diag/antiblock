package usecase

import (
	"strings"
	"testing"
)

func TestMigrationV2ProgressReportHTML(t *testing.T) {
	t.Parallel()
	st := &MigrationV2State{
		Phase:         "pro_groups",
		StartedAt:     "2026-04-06T12:00:00Z",
		OK:            3,
		Err:           1,
		ProGroupIDs:   []uint{10, 20, 30},
		ProIdx:        1,
		ErrUserSample: []uint{42, 99},
	}
	html := MigrationV2ProgressReportHTML(st, "prod-docker")
	if !strings.Contains(html, "Pro (Docker)") || !strings.Contains(html, "prod-docker") {
		t.Fatalf("unexpected report: %s", html)
	}
	if !strings.Contains(html, "42") {
		t.Fatalf("expected err sample in report: %s", html)
	}
}
