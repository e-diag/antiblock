package handler

import (
	"fmt"
	"sync"
	"time"
)

const mediaGroupWaitDuration = 1500 * time.Millisecond

// PendingMediaGroup — накопленные сообщения одного альбома (одна медиа-группа)
type PendingMediaGroup struct {
	FromChatID int64
	MessageIDs []int
	Timer      *time.Timer
	Audience   BroadcastAudience // аудитория на момент добавления первого сообщения
}

// BroadcastMediaGroupBuffer буферизует сообщения с MediaGroupID и отдаёт их одним альбомом после таймера
type BroadcastMediaGroupBuffer struct {
	mu     sync.Mutex
	groups map[string]*PendingMediaGroup
}

// NewBroadcastMediaGroupBuffer создаёт буфер медиа-групп для рассылки
func NewBroadcastMediaGroupBuffer() *BroadcastMediaGroupBuffer {
	return &BroadcastMediaGroupBuffer{
		groups: make(map[string]*PendingMediaGroup),
	}
}

func bufferKey(adminID int64, mediaGroupID string) string {
	return fmt.Sprintf("%d_%s", adminID, mediaGroupID)
}

// Add добавляет сообщение в группу. audience сохраняется в группе и передаётся в onFlush при сбросе.
// После истечения таймера вызывается onFlush(adminID, fromChatID, messageIDs, audience).
func (buf *BroadcastMediaGroupBuffer) Add(adminID int64, mediaGroupID string, fromChatID int64, messageID int, audience BroadcastAudience, onFlush func(adminID int64, fromChatID int64, messageIDs []int, audience BroadcastAudience)) (buffered bool) {
	buf.mu.Lock()
	key := bufferKey(adminID, mediaGroupID)
	g, ok := buf.groups[key]
	if !ok {
		g = &PendingMediaGroup{FromChatID: fromChatID, MessageIDs: []int{messageID}, Audience: audience}
		g.Timer = time.AfterFunc(mediaGroupWaitDuration, func() {
			buf.mu.Lock()
			gr := buf.groups[key]
			delete(buf.groups, key)
			buf.mu.Unlock()
			if gr != nil && len(gr.MessageIDs) > 0 && onFlush != nil {
				onFlush(adminID, gr.FromChatID, gr.MessageIDs, gr.Audience)
			}
		})
		buf.groups[key] = g
		buf.mu.Unlock()
		return true
	}
	g.MessageIDs = append(g.MessageIDs, messageID)
	buf.mu.Unlock()
	return true
}

// GetAndRemove забирает накопленную группу (например, при приходе следующего сообщения без этой группы).
// Возвращает (fromChatID, messageIDs, true) если группа была, иначе (0, nil, false).
func (buf *BroadcastMediaGroupBuffer) GetAndRemove(adminID int64, mediaGroupID string) (fromChatID int64, messageIDs []int, ok bool) {
	buf.mu.Lock()
	defer buf.mu.Unlock()
	key := bufferKey(adminID, mediaGroupID)
	g := buf.groups[key]
	if g == nil {
		return 0, nil, false
	}
	if g.Timer != nil {
		g.Timer.Stop()
	}
	delete(buf.groups, key)
	return g.FromChatID, g.MessageIDs, true
}

// FlushAllForAdmin забирает и возвращает все группы для админа (для досрочного сброса при новом сообщении).
// Ключи по mediaGroupID; вызывающий сам решает, как обрабатывать (по одной группе за раз или все).
func (buf *BroadcastMediaGroupBuffer) FlushAllForAdmin(adminID int64) []PendingMediaGroup {
	buf.mu.Lock()
	defer buf.mu.Unlock()
	var result []PendingMediaGroup
	prefix := fmt.Sprintf("%d_", adminID)
	for k, g := range buf.groups {
		if len(prefix) <= len(k) && k[:len(prefix)] == prefix {
			if g.Timer != nil {
				g.Timer.Stop()
			}
			result = append(result, *g)
			delete(buf.groups, k)
		}
	}
	return result
}
