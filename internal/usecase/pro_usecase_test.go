package usecase

import (
	"testing"
	"time"
)

func TestProInfraTTL_Default30DaysMinusOneMinute(t *testing.T) {
	got := proInfraTTL(30)
	want := (30 * 24 * time.Hour) - time.Minute
	if got != want {
		t.Fatalf("unexpected ttl: got=%s want=%s", got, want)
	}
}

func TestProInfraTTL_NonPositiveFallsBackToDefault(t *testing.T) {
	got := proInfraTTL(0)
	want := (30 * 24 * time.Hour) - time.Minute
	if got != want {
		t.Fatalf("unexpected ttl for zero: got=%s want=%s", got, want)
	}
}

