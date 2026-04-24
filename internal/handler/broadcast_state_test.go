package handler

import (
	"testing"
)

func TestBroadcastAudienceMatchesUser(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		aud           BroadcastAudience
		prem, pro     bool
		want          bool
	}{
		{"all free only", BroadcastAudienceAll, false, false, true},
		{"all skip premium", BroadcastAudienceAll, true, false, false},
		{"all skip pro", BroadcastAudienceAll, false, true, false},
		{"free same as all for prem+pro", BroadcastAudienceFree, true, true, false},
		{"pro only", BroadcastAudiencePro, false, true, true},
		{"pro not if premium", BroadcastAudiencePro, true, true, false},
		{"pro not if no pro", BroadcastAudiencePro, false, false, false},
		{"premium", BroadcastAudiencePremium, true, false, true},
		{"premium not free", BroadcastAudiencePremium, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BroadcastAudienceMatchesUser(tc.aud, tc.prem, tc.pro)
			if got != tc.want {
				t.Fatalf("aud=%s prem=%v pro=%v: got %v want %v", tc.aud, tc.prem, tc.pro, got, tc.want)
			}
		})
	}
}

func TestBroadcastStatePhases(t *testing.T) {
	t.Parallel()
	s := NewBroadcastState()
	const admin int64 = 42

	if s.Phase(admin) != BroadcastPhaseIdle {
		t.Fatal("expected idle")
	}
	s.SetAwaitingMessage(admin, BroadcastAudienceAll)
	if !s.IsAwaitingMessage(admin) || s.IsPreview(admin) {
		t.Fatal("expected awaiting message")
	}
	s.SetPreview(admin, &BroadcastPending{
		Audience: BroadcastAudienceAll, FromChatID: 1, MessageIDs: []int{5},
	})
	if !s.IsPreview(admin) || s.IsAwaitingMessage(admin) {
		t.Fatal("expected preview")
	}
	s.Clear(admin)
	if s.Phase(admin) != BroadcastPhaseIdle {
		t.Fatal("expected idle after clear")
	}
}

func TestBroadcastSetAwaitingClearsPreview(t *testing.T) {
	t.Parallel()
	s := NewBroadcastState()
	const admin int64 = 7
	s.SetPreview(admin, &BroadcastPending{Audience: BroadcastAudienceFree, FromChatID: 1, MessageIDs: []int{1}})
	s.SetAwaitingMessage(admin, BroadcastAudienceAll)
	if _, ok := s.Pending(admin); ok {
		t.Fatal("preview should be cleared when re-entering awaiting")
	}
	if !s.IsAwaitingMessage(admin) {
		t.Fatal("awaiting expected")
	}
}
