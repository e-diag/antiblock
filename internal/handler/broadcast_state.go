package handler

import "sync"

// BroadcastAudience определяет аудиторию рассылки
type BroadcastAudience string

const (
	BroadcastAudienceAll  BroadcastAudience = "all"
	BroadcastAudienceFree BroadcastAudience = "free"
)

// BroadcastPhase — этап сценария рассылки.
type BroadcastPhase int

const (
	BroadcastPhaseIdle BroadcastPhase = iota
	BroadcastPhaseAwaitingMessage // выбрана аудитория, ждём контент
	BroadcastPhasePreview         // контент сохранён, ждём подтверждения
)

// BroadcastPending — что разослать после подтверждения (одно сообщение или альбом).
type BroadcastPending struct {
	Audience   BroadcastAudience
	FromChatID int64
	MessageIDs []int
}

// BroadcastState — FSM рассылки по админам.
type BroadcastState struct {
	mu       sync.Mutex
	phase    map[int64]BroadcastPhase
	audience map[int64]BroadcastAudience
	pending  map[int64]*BroadcastPending
}

func NewBroadcastState() *BroadcastState {
	return &BroadcastState{
		phase:    make(map[int64]BroadcastPhase),
		audience: make(map[int64]BroadcastAudience),
		pending:  make(map[int64]*BroadcastPending),
	}
}

// SetAwaitingMessage — выбор аудитории: ждём сообщение (сбрасывает предыдущий preview).
func (s *BroadcastState) SetAwaitingMessage(adminID int64, audience BroadcastAudience) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase[adminID] = BroadcastPhaseAwaitingMessage
	s.audience[adminID] = audience
	delete(s.pending, adminID)
}

// SetPreview — контент получен, ждём подтверждения.
func (s *BroadcastState) SetPreview(adminID int64, p *BroadcastPending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase[adminID] = BroadcastPhasePreview
	if p != nil {
		s.audience[adminID] = p.Audience
		s.pending[adminID] = p
	}
}

// Phase текущий этап (по умолчанию Idle).
func (s *BroadcastState) Phase(adminID int64) BroadcastPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase[adminID]
}

// IsAwaitingMessage — ждём ввод сообщения для рассылки.
func (s *BroadcastState) IsAwaitingMessage(adminID int64) bool {
	return s.Phase(adminID) == BroadcastPhaseAwaitingMessage
}

// IsPreview — показан предпросмотр, ждём confirm/cancel.
func (s *BroadcastState) IsPreview(adminID int64) bool {
	return s.Phase(adminID) == BroadcastPhasePreview
}

// Audience выбранная аудитория (для awaiting и preview).
func (s *BroadcastState) Audience(adminID int64) BroadcastAudience {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audience[adminID]
}

// Pending данные для подтверждённой рассылки.
func (s *BroadcastState) Pending(adminID int64) (*BroadcastPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[adminID]
	return p, p != nil
}

// Clear сбрасывает состояние админа.
func (s *BroadcastState) Clear(adminID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.phase, adminID)
	delete(s.audience, adminID)
	delete(s.pending, adminID)
}
