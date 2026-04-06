package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"github.com/yourusername/antiblock/internal/infrastructure/alert"
	"github.com/yourusername/antiblock/internal/infrastructure/config"
	"github.com/yourusername/antiblock/internal/infrastructure/docker"
)

// DockerMonitorWorker следит за использованием памяти контейнерами mtg-user-{tg_id}
// и шлёт алерты в служебный чат при превышении лимита 100MB.
type DockerMonitorWorker struct {
	alerts *alert.TelegramAlerter
	cfg      config.WorkerConfig
	stop     chan struct{}
	stopOnce sync.Once
	cli      *client.Client
}

func NewDockerMonitorWorker(
	alerts *alert.TelegramAlerter,
	cfg config.WorkerConfig,
	dockerMgr *docker.Manager,
) *DockerMonitorWorker {
	var cli *client.Client
	if dockerMgr != nil {
		cli = dockerMgr.GetClient()
	}
	return &DockerMonitorWorker{
		alerts: alerts,
		cfg:    cfg,
		stop:     make(chan struct{}),
		cli:      cli,
	}
}

func (w *DockerMonitorWorker) Start() {
	if !w.cfg.Enabled {
		log.Println("Docker monitor worker is disabled")
		return
	}

	interval := w.cfg.Interval()
	if interval <= 0 {
		interval = time.Minute * 5
	}

	log.Printf("Starting docker monitor worker (interval: %v)", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// первая проверка сразу
	w.checkOnce()

	for {
		select {
		case <-ticker.C:
			w.checkOnce()
		case <-w.stop:
			log.Println("Docker monitor worker stopped")
			return
		}
	}
}

func (w *DockerMonitorWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

func (w *DockerMonitorWorker) checkOnce() {
	if w.cli == nil {
		log.Printf("docker monitor: Docker client not configured, skipping")
		return
	}

	timeout := w.cfg.Timeout()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := filters.NewArgs()
	args.Add("name", "mtg-user-")

	containers, err := w.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		log.Printf("docker monitor: list containers error: %v", err)
		w.alerts.Send(ctx, alert.Report{
			Type:    "docker_list_containers",
			Source:  "worker/docker_monitor",
			ErrText: err.Error(),
		})
		return
	}

	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		} else {
			// если имён нет, пропускаем контейнер, чтобы не получить панику
			continue
		}
		if !strings.HasPrefix(name, "mtg-user-") {
			continue
		}

		tgIDStr := strings.TrimPrefix(name, "mtg-user-")
		if tgIDStr == "" {
			continue
		}

		statsResp, err := w.cli.ContainerStats(ctx, c.ID, false)
		if err != nil {
			log.Printf("docker monitor: stats error for %s: %v", name, err)
			continue
		}

		var stats container.StatsResponse
		if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
			statsResp.Body.Close()
			log.Printf("docker monitor: stats decode error for %s: %v", name, err)
			continue
		}
		statsResp.Body.Close()

		usage := float64(stats.MemoryStats.Usage)
		limit := float64(stats.MemoryStats.Limit)
		if limit <= 0 {
			limit = 100 * 1024 * 1024
		}

		usageMB := usage / (1024 * 1024)
		if usageMB <= 100 {
			continue
		}

		tgID, _ := strconv.ParseInt(tgIDStr, 10, 64)
		w.alerts.Send(ctx, alert.Report{
			Type:     "docker_high_memory",
			Source:   "worker/docker_monitor",
			UserTGID: tgID,
			Extra:    fmt.Sprintf("container=%s memory_mb=%.1f (limit 100 MB)", name, usageMB),
			ErrText:  "превышен лимит памяти контейнера mtg-user",
		})
	}
}
