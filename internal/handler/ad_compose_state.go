package handler

import "sync"

// AdComposeStep — шаг диалога добавления/редактирования объявления
type AdComposeStep int

const (
	AdComposeIdle AdComposeStep = iota
	AdComposeText
	AdComposeChannel
	AdComposeButton
	AdComposeHours
)

// AdComposeData — собранные данные при создании/редактировании объявления
type AdComposeData struct {
	Step          AdComposeStep
	Text          string
	ChannelLink   string
	ChannelUsername string
	ButtonText    string
	ButtonURL     string
	ExpiresHours  int
	EditingID     uint // 0 = новый, иначе ID редактируемого объявления
}

// AdComposeState хранит состояние диалога добавления/редактирования объявления по adminID
type AdComposeState struct {
	mu   sync.Mutex
	data map[int64]*AdComposeData
}

func NewAdComposeState() *AdComposeState {
	return &AdComposeState{data: make(map[int64]*AdComposeData)}
}

func (s *AdComposeState) Get(adminID int64) *AdComposeData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[adminID]
}

func (s *AdComposeState) Set(adminID int64, d *AdComposeData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d == nil {
		delete(s.data, adminID)
		return
	}
	s.data[adminID] = d
}

func (s *AdComposeState) Clear(adminID int64) {
	s.Set(adminID, nil)
}
