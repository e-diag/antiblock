package usecase

import (
	"encoding/json"
	"testing"
)

func TestRunCompensationNotifyDrain_ClearsQueue(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal([]int64{1000001, 1000002})
	s := &memSettingsStore{vals: map[string]string{
		SettingCompensationNoticeQueueV1: string(raw),
	}}
	var calls int
	send := func(tgID int64, text string) error {
		calls++
		if text != TextCompensation14Days {
			t.Fatalf("unexpected text")
		}
		return nil
	}
	n, err := RunCompensationNotifyDrain(s, send, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || calls != 2 {
		t.Fatalf("want n=2 calls=2, got n=%d calls=%d", n, calls)
	}
	v, _ := s.Get(SettingCompensationNoticeQueueV1)
	if v != "" {
		t.Fatalf("queue should be cleared, got %q", v)
	}
}
