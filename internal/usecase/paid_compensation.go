package usecase

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/repository"
	"gorm.io/gorm"
)

// Ключ очереди уведомлений после атомарной компенсации (JSON []int64 tg_id).
const SettingCompensationNoticeQueueV1 = "compensation_notice_queue_v1"
const paidOpsCompensationLockKey = "paidops:compensation:apply"

// TextCompensation14Days — текст извинений и уведомления о продлении на 14 дней.
const TextCompensation14Days = "Мы понимаем, что последнюю неделю все прокси были недоступны. Продлеваем вашу подписку на <b>14 дней</b> — спасибо, что остаетесь с нами!"

// Compensate14DaysTransactional атомарно начисляет +14 дн., ставит маркер и очередь уведомлений в одной транзакции.
// Уведомления в Telegram нужно отправить отдельно (RunCompensationNotifyDrain), чтобы не блокировать транзакцию и не ловить flood.
func Compensate14DaysTransactional(db *gorm.DB, p *PaidOps) error {
	if db == nil || p == nil || p.Settings == nil {
		return fmt.Errorf("db and paid ops required")
	}
	lockOwner := p.effectiveLockOwner()
	if p.Locker != nil {
		if err := p.Locker.Acquire(paidOpsCompensationLockKey, lockOwner, 20*time.Minute); err != nil {
			return err
		}
		defer p.Locker.Release(paidOpsCompensationLockKey, lockOwner)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		sr := repository.NewSettingsRepository(tx)
		if v, _ := sr.Get(SettingPaidCompensation14dV1); v != "" {
			return ErrPaidCompensationAlreadyDone
		}
		ur := repository.NewUserRepository(tx)
		var subRepo repository.ProSubscriptionRepository
		if p.Subs != nil {
			subRepo = repository.NewProSubscriptionRepository(tx)
		}
		ids, err := ur.ListPaidActiveUserIDs()
		if err != nil {
			return err
		}
		const addDays = 14
		var notifyTG []int64
		for _, id := range ids {
			u, err := ur.GetByID(id)
			if err != nil || u == nil {
				continue
			}
			premiumOK := false
			if u.IsPremiumActive() && u.PremiumUntil != nil {
				nu := u.PremiumUntil.AddDate(0, 0, addDays)
				u.PremiumUntil = &nu
				premiumOK = true
			}
			proOK := false
			if subRepo != nil {
				if sub, _ := subRepo.GetByUserID(u.ID); sub != nil {
					sub.ExpiresAt = sub.ExpiresAt.AddDate(0, 0, addDays)
					if err := subRepo.Update(sub); err != nil {
						return fmt.Errorf("pro sub user_id=%d: %w", u.ID, err)
					}
					proOK = true
				}
			}
			if premiumOK {
				if err := ur.Update(u); err != nil {
					return fmt.Errorf("user user_id=%d: %w", u.ID, err)
				}
			}
			if !premiumOK && !proOK {
				continue
			}
			notifyTG = append(notifyTG, u.TGID)
		}
		raw, err := json.Marshal(notifyTG)
		if err != nil {
			return err
		}
		if err := sr.Set(SettingCompensationNoticeQueueV1, string(raw)); err != nil {
			return err
		}
		ts := time.Now().UTC().Format(time.RFC3339)
		if err := sr.Set(SettingPaidCompensation14dV1, ts); err != nil {
			return err
		}
		return nil
	})
}

// ErrCompensationNoticeQueueEmpty — очередь уведомлений пуста или уже обработана.
var ErrCompensationNoticeQueueEmpty = errors.New("очередь уведомлений о компенсации пуста")

// RunCompensationNotifyDrain отправляет сообщения из очереди с паузой; снимает tg_id из очереди после каждой успешной отправки.
func RunCompensationNotifyDrain(settings repository.SettingsRepository, send func(tgID int64, text string) error, delay time.Duration) (sent int, err error) {
	if settings == nil || send == nil {
		return 0, fmt.Errorf("invalid args")
	}
	raw, err := settings.Get(SettingCompensationNoticeQueueV1)
	if err != nil || strings.TrimSpace(raw) == "" {
		return 0, ErrCompensationNoticeQueueEmpty
	}
	var pending []int64
	if err := json.Unmarshal([]byte(raw), &pending); err != nil {
		return 0, fmt.Errorf("parse queue: %w", err)
	}
	if len(pending) == 0 {
		_ = settings.Set(SettingCompensationNoticeQueueV1, "")
		return 0, ErrCompensationNoticeQueueEmpty
	}
	remaining := make([]int64, 0, len(pending))
	for _, tgID := range pending {
		if err := send(tgID, TextCompensation14Days); err != nil {
			log.Printf("[PaidOps] notify tg_id=%d: %v", tgID, err)
			remaining = append(remaining, tgID)
			continue
		}
		sent++
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	if len(remaining) == 0 {
		_ = settings.Set(SettingCompensationNoticeQueueV1, "")
	} else {
		b, _ := json.Marshal(remaining)
		if err := settings.Set(SettingCompensationNoticeQueueV1, string(b)); err != nil {
			return sent, err
		}
	}
	return sent, nil
}
