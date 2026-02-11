package handler

import "sync"

// BroadcastAudience определяет аудиторию рассылки
type BroadcastAudience string

const (
	BroadcastAudienceAll  BroadcastAudience = "all"
	BroadcastAudienceFree BroadcastAudience = "free"
)

// BroadcastState хранит состояние рассылки по админам:
// ожидается ли ввод сообщения и для какой аудитории.
type BroadcastState struct {
	mu        sync.Mutex
	awaiting  map[int64]bool
	audience  map[int64]BroadcastAudience
}

func NewBroadcastState() *BroadcastState {
	return &BroadcastState{
		awaiting: make(map[int64]bool),
		audience: make(map[int64]BroadcastAudience),
	}
}

// SetAwaiting включает режим ожидания сообщения для админа и запоминает аудиторию.
func (s *BroadcastState) SetAwaiting(adminID int64, audience BroadcastAudience) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awaiting[adminID] = true
	s.audience[adminID] = audience
}

// IsAwaiting возвращает, ожидается ли сейчас ввод сообщения от этого админа.
func (s *BroadcastState) IsAwaiting(adminID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.awaiting[adminID]
}

// Audience возвращает выбранную аудиторию для админа.
func (s *BroadcastState) Audience(adminID int64) BroadcastAudience {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audience[adminID]
}

// Clear сбрасывает состояние ожидания и аудиторию для админа.
func (s *BroadcastState) Clear(adminID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.awaiting, adminID)
	delete(s.audience, adminID)
}
