package handler

import "testing"

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
