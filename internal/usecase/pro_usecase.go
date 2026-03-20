package usecase

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
	"github.com/yourusername/antiblock/internal/repository"
)

type ProUseCase interface {
	ActivateProSubscription(user *domain.User, days int, serverIP string, dockerMgr *docker.Manager, groupCycleDays int) (*domain.ProGroup, bool, error)
	RevokeProSubscription(user *domain.User) error
	GetActiveSubscription(userID uint) (*domain.ProSubscription, error)
	CleanupExpiredGroups(dockerMgr *docker.Manager, cycleDays int) error
	GetActiveGroups() ([]*domain.ProGroup, error)
	GetGroupByID(id uint) (*domain.ProGroup, error)
	CountActiveSubscribersByGroup(groupID uint) (int64, error)
	SetOnProRotated(fn func(tgID int64, group *domain.ProGroup))
}

type proUseCase struct {
	groupRepo repository.ProGroupRepository
	subRepo   repository.ProSubscriptionRepository
	proxyRepo repository.ProxyRepository
	userRepo  repository.UserRepository
	onRotated func(tgID int64, group *domain.ProGroup)
}

func NewProUseCase(
	groupRepo repository.ProGroupRepository,
	subRepo repository.ProSubscriptionRepository,
	proxyRepo repository.ProxyRepository,
	userRepo repository.UserRepository,
) ProUseCase {
	return &proUseCase{
		groupRepo: groupRepo,
		subRepo:   subRepo,
		proxyRepo: proxyRepo,
		userRepo:  userRepo,
	}
}

func (uc *proUseCase) SetOnProRotated(fn func(tgID int64, group *domain.ProGroup)) {
	uc.onRotated = fn
}

func (uc *proUseCase) RevokeProSubscription(user *domain.User) error {
	if user == nil {
		return fmt.Errorf("user is nil")
	}
	if err := uc.subRepo.ExpireByUserID(user.ID); err != nil {
		return err
	}
	return nil
}

func utcDayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func sameUTCDay(a, b time.Time) bool {
	return utcDayStart(a).Equal(utcDayStart(b))
}

// proEEPorts — пул портов для ee (fake-TLS) контейнеров Pro-групп.
// Индекс = dayOfMonth - 1 (день 1 -> 443, день 2 -> 80, ...).
var proEEPorts = []int{
	443, 80, 8085, 8443, 2053, 2083, 2087, 2096,
	9443, 1194, 3128, 4443, 5228, 5443, 6443,
	7443, 8880, 8888, 9000, 9080, 9090, 9100,
	9200, 9300, 10443, 11443, 12443, 13443, 14443,
	15443, 16443,
}

// proDDPorts — отдельный пул портов для dd контейнеров Pro-групп.
// Не пересекается с proEEPorts и с диапазоном legacy Premium 20000-29999.
var proDDPorts = []int{
	8080, 8081, 8082, 8088, 8181, 8282, 8383, 8484,
	8585, 8686, 8787, 8899, 8989, 9010, 9060, 9110,
	9160, 9210, 9260, 9310, 9360, 9410, 9460, 9510,
	9560, 9610, 9660, 9710, 9760, 9810, 9860, 9910,
}

// proPortForDay возвращает детерминированный порт Pro-контейнера по дате.
func proPortForDay(date time.Time, ports []int) int {
	idx := (date.Day() - 1) % len(ports)
	return ports[idx]
}

func secretPreview(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func (uc *proUseCase) createGroupForDay(dayStart time.Time, serverIP string, dockerMgr *docker.Manager, cycleDays int) (*domain.ProGroup, error) {
	if cycleDays <= 0 {
		cycleDays = 30
	}

	genCtx, genCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer genCancel()

	secretDD, err := generatePremiumSecret()
	if err != nil {
		return nil, fmt.Errorf("generate dd secret: %w", err)
	}
	if dockerMgr == nil {
		return nil, fmt.Errorf("dockerMgr is required to generate ee secret for Pro group")
	}
	secretEE, err := dockerMgr.GenerateEESecretViaDocker(genCtx)
	if err != nil {
		log.Printf("[Pro] createGroupForDay: GenerateEESecretViaDocker failed date=%s serverIP=%s: %v",
			dayStart.Format("2006-01-02"), serverIP, err)
		return nil, fmt.Errorf("generate ee secret: %w", err)
	}

	now := time.Now().UTC()
	dayStart = utcDayStart(dayStart)
	portDD := proPortForDay(dayStart, proDDPorts)
	portEE := proPortForDay(dayStart, proEEPorts)
	infraUntil := now.AddDate(0, 0, cycleDays)

	group := &domain.ProGroup{
		Date:                    dayStart,
		ServerIP:                serverIP,
		PortDD:                  portDD,
		PortEE:                  portEE,
		SecretDD:                secretDD,
		SecretEE:                secretEE,
		InfrastructureExpiresAt: infraUntil,
		Status:                  domain.ProxyStatusActive,
	}

	if err := uc.groupRepo.Create(group); err != nil {
		return nil, err
	}
	group.ContainerDD = fmt.Sprintf(docker.ProContainerNameDD, group.ID)
	group.ContainerEE = fmt.Sprintf(docker.ProContainerNameEE, group.ID)
	if err := uc.groupRepo.Update(group); err != nil {
		return nil, err
	}

	log.Printf("[Pro] createGroupForDay date=%s portDD=%d portEE=%d secretDD=%s... secretEE=%s...",
		dayStart.Format("2006-01-02"), portDD, portEE, secretPreview(secretDD), secretPreview(secretEE))

	if err := dockerMgr.CreateProContainerDD(group); err != nil {
		log.Printf("[Pro] CreateProContainerDD failed date=%s: %v", dayStart.Format("2006-01-02"), err)
		return nil, fmt.Errorf("create dd container: %w", err)
	}
	if err := dockerMgr.CreateProContainerEE(group); err != nil {
		log.Printf("[Pro] CreateProContainerEE failed date=%s: %v", dayStart.Format("2006-01-02"), err)
		return nil, fmt.Errorf("create ee container: %w", err)
	}

	return group, nil
}

// rotateGroupInPlace новые порты/секреты, те же подписчики (тот же pro_group_id).
func (uc *proUseCase) rotateGroupInPlace(g *domain.ProGroup, serverIP string, dockerMgr *docker.Manager, cycleDays int) (*domain.ProGroup, error) {
	if cycleDays <= 0 {
		cycleDays = 30
	}
	uc.teardownGroupContainers(dockerMgr, g)

	genCtx, genCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer genCancel()

	secretDD, err := generatePremiumSecret()
	if err != nil {
		return nil, err
	}
	if dockerMgr == nil {
		return nil, fmt.Errorf("dockerMgr is required to generate ee secret for Pro group")
	}
	secretEE, err := dockerMgr.GenerateEESecretViaDocker(genCtx)
	if err != nil {
		return nil, err
	}
	dayStart := utcDayStart(g.Date)
	portDD := proPortForDay(dayStart, proDDPorts)
	portEE := proPortForDay(dayStart, proEEPorts)

	g.SecretDD = secretDD
	g.SecretEE = secretEE
	g.PortDD = portDD
	g.PortEE = portEE
	g.ServerIP = serverIP
	g.InfrastructureExpiresAt = time.Now().UTC().AddDate(0, 0, cycleDays)
	g.Status = domain.ProxyStatusActive

	if err := uc.groupRepo.Update(g); err != nil {
		return nil, err
	}

	if err := dockerMgr.CreateProContainerDD(g); err != nil {
		return nil, fmt.Errorf("rotate Pro dd: %w", err)
	}
	if err := dockerMgr.CreateProContainerEE(g); err != nil {
		return nil, fmt.Errorf("rotate Pro ee: %w", err)
	}

	subs, _ := uc.subRepo.GetActiveByGroupID(g.ID)
	if uc.onRotated != nil && uc.userRepo != nil {
		for _, sub := range subs {
			u, err := uc.userRepo.GetByID(sub.UserID)
			if err == nil && u != nil {
				uc.onRotated(u.TGID, g)
			}
		}
	}
	return g, nil
}

// ensureTodayGroup группа текущего UTC-дня: создать / при истёкшей инфраструктуре — обновить на месте.
func (uc *proUseCase) ensureTodayGroup(serverIP string, dockerMgr *docker.Manager, cycleDays int) (*domain.ProGroup, error) {
	now := time.Now().UTC()
	g, err := uc.groupRepo.GetByDate(now)
	if err != nil {
		return nil, err
	}
	if g == nil {
		newGroup, err := uc.createGroupForDay(now, serverIP, dockerMgr, cycleDays)
		if err != nil {
			if isDuplicateKeyError(err) {
				return uc.groupRepo.GetByDate(now)
			}
			return nil, err
		}
		return newGroup, nil
	}
	if g.InfrastructureExpiresAt.After(now) {
		return g, nil
	}
	return uc.rotateGroupInPlace(g, serverIP, dockerMgr, cycleDays)
}

func (uc *proUseCase) findFreeProPort(min, max int) (int, error) {
	if uc.proxyRepo == nil {
		return 0, fmt.Errorf("proxyRepo is nil")
	}
	return uc.proxyRepo.FindFirstFreePort(min, max)
}

func (uc *proUseCase) ActivateProSubscription(user *domain.User, days int, serverIP string, dockerMgr *docker.Manager, groupCycleDays int) (*domain.ProGroup, bool, error) {
	if user == nil {
		return nil, false, fmt.Errorf("user is nil")
	}
	if days <= 0 {
		days = 30
	}
	if groupCycleDays <= 0 {
		groupCycleDays = days
	}

	existing, err := uc.subRepo.GetByUserID(user.ID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		existing.ExpiresAt = existing.ExpiresAt.AddDate(0, 0, days)
		if err := uc.subRepo.Update(existing); err != nil {
			return nil, false, err
		}
		g, err := uc.groupRepo.GetByID(existing.ProGroupID)
		if err != nil || g == nil {
			return nil, true, fmt.Errorf("pro group not found for subscription")
		}
		return g, true, nil
	}

	group, err := uc.ensureTodayGroup(serverIP, dockerMgr, groupCycleDays)
	if err != nil {
		return nil, false, err
	}

	sub := &domain.ProSubscription{
		UserID:     user.ID,
		ProGroupID: group.ID,
		ExpiresAt:  time.Now().UTC().AddDate(0, 0, days),
	}
	if err := uc.subRepo.Create(sub); err != nil {
		return nil, false, err
	}
	return group, false, nil
}

func (uc *proUseCase) GetActiveSubscription(userID uint) (*domain.ProSubscription, error) {
	return uc.subRepo.GetByUserID(userID)
}

func (uc *proUseCase) GetGroupByID(id uint) (*domain.ProGroup, error) {
	return uc.groupRepo.GetByID(id)
}

func (uc *proUseCase) CountActiveSubscribersByGroup(groupID uint) (int64, error) {
	return uc.subRepo.CountActiveByGroupID(groupID)
}

func (uc *proUseCase) CleanupExpiredGroups(dockerMgr *docker.Manager, cycleDays int) error {
	if cycleDays <= 0 {
		cycleDays = 30
	}
	now := time.Now().UTC()
	groups, err := uc.groupRepo.ListGroupsNeedingRotation(now)
	if err != nil {
		return err
	}

	var expiredToday, expiredOld []*domain.ProGroup
	for _, g := range groups {
		if sameUTCDay(g.Date, now) {
			expiredToday = append(expiredToday, g)
		} else {
			expiredOld = append(expiredOld, g)
		}
	}

	// 1) Сначала обновляем группу текущего дня (если протухла), чтобы миграции шли в живую группу дня.
	for _, g := range expiredToday {
		if _, err := uc.rotateGroupInPlace(g, g.ServerIP, dockerMgr, cycleDays); err != nil {
			log.Printf("[Pro] rotate today group %d: %v", g.ID, err)
		} else {
			log.Printf("[Pro] rotated in place group %d (день UTC совпадает с сегодня)", g.ID)
		}
	}

	serverIP := ""
	if len(expiredOld) > 0 {
		serverIP = expiredOld[0].ServerIP
	}
	if serverIP == "" && len(expiredToday) > 0 {
		serverIP = expiredToday[0].ServerIP
	}

	for _, g := range expiredOld {
		subs, err := uc.subRepo.GetActiveByGroupID(g.ID)
		if err != nil {
			log.Printf("[Pro] list subs group %d: %v", g.ID, err)
			continue
		}
		if len(subs) == 0 {
			uc.teardownGroupContainers(dockerMgr, g)
			g.Status = domain.ProxyStatusInactive
			_ = uc.groupRepo.Update(g)
			continue
		}
		sp := g.ServerIP
		if sp == "" {
			sp = serverIP
		}
		tgt, err := uc.ensureTodayGroup(sp, dockerMgr, cycleDays)
		if err != nil {
			log.Printf("[Pro] ensure today for migrate from %d: %v", g.ID, err)
			continue
		}
		for _, sub := range subs {
			sub.ProGroupID = tgt.ID
			if err := uc.subRepo.Update(sub); err != nil {
				log.Printf("[Pro] migrate sub %d: %v", sub.ID, err)
				continue
			}
			if uc.onRotated != nil && uc.userRepo != nil {
				u, err := uc.userRepo.GetByID(sub.UserID)
				if err == nil && u != nil {
					uc.onRotated(u.TGID, tgt)
				}
			}
		}
		uc.teardownGroupContainers(dockerMgr, g)
		g.Status = domain.ProxyStatusInactive
		_ = uc.groupRepo.Update(g)
		log.Printf("[Pro] migrated group %d -> today group %d (%d users)", g.ID, tgt.ID, len(subs))
	}
	return nil
}

func (uc *proUseCase) teardownGroupContainers(dockerMgr *docker.Manager, g *domain.ProGroup) {
	if dockerMgr == nil || g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if g.ContainerDD != "" {
		_ = dockerMgr.RemoveUserContainer(ctx, g.ContainerDD)
	}
	if g.ContainerEE != "" {
		_ = dockerMgr.RemoveUserContainer(ctx, g.ContainerEE)
	}
}

func (uc *proUseCase) GetActiveGroups() ([]*domain.ProGroup, error) {
	return uc.groupRepo.GetActiveGroups()
}
