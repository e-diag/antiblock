package handler

import "sync"

// MessageState хранит ID последнего сообщения бота для пользователя (для редактирования вместо нового)
type MessageState struct {
	mu   sync.RWMutex
	msgs map[int64]int
}

func NewMessageState() *MessageState {
	return &MessageState{msgs: make(map[int64]int)}
}

func (s *MessageState) Set(userID int64, msgID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs[userID] = msgID
}

func (s *MessageState) Get(userID int64) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.msgs[userID]
	return id, ok
}

func (s *MessageState) Clear(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.msgs, userID)
}
