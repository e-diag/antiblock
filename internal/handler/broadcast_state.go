package handler

import "sync"

// BroadcastState хранит, какой админ ожидает ввод текста рассылки
type BroadcastState struct {
	mu       sync.Mutex
	awaiting map[int64]bool
}

func NewBroadcastState() *BroadcastState {
	return &BroadcastState{
		awaiting: make(map[int64]bool),
	}
}

func (s *BroadcastState) SetAwaiting(adminID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awaiting[adminID] = true
}

func (s *BroadcastState) IsAwaiting(adminID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.awaiting[adminID]
}

func (s *BroadcastState) Clear(adminID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.awaiting, adminID)
}
