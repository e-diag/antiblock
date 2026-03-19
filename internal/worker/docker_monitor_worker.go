package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/go-telegram/bot"

	"github.com/yourusername/antiblock/internal/infrastructure/config"
)

// DockerMonitorWorker следит за использованием памяти контейнерами mtg-user-{tg_id}
// и уведомляет админов при превышении лимита 100MB.
type DockerMonitorWorker struct {
	bot      *bot.Bot
	adminIDs []int64
	cfg      config.WorkerConfig
	stop     chan struct{}
	cli      *client.Client
}

func NewDockerMonitorWorker(b *bot.Bot, adminIDs []int64, cfg config.WorkerConfig) *DockerMonitorWorker {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Printf("docker monitor: failed to init client: %v", err)
		cli = nil
	}
	return &DockerMonitorWorker{
		bot:      b,
		adminIDs: adminIDs,
		cfg:      cfg,
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
	close(w.stop)
}

func (w *DockerMonitorWorker) checkOnce() {
	timeout := w.cfg.Timeout()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cli := w.cli
	if cli == nil {
		w.notifyAdmins(ctx, "❗ Docker monitor: Docker client не инициализирован.")
		return
	}

	args := filters.NewArgs()
	args.Add("name", "mtg-user-")

	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		log.Printf("docker monitor: list containers error: %v", err)
		w.notifyAdmins(ctx, fmt.Sprintf("❗ Ошибка чтения списка контейнеров Docker: %v", err))
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

		statsResp, err := cli.ContainerStats(ctx, c.ID, false)
		if err != nil {
			log.Printf("docker monitor: stats error for %s: %v", name, err)
			continue
		}

		var stats types.StatsJSON
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

		// Шлём алерт всем админам
		text := fmt.Sprintf(
			"⚠️ <b>Превышен лимит памяти</b>\n\n"+
				"Контейнер: <code>%s</code>\n"+
				"TG ID: <code>%s</code>\n"+
				"Память: <b>%.1f MB</b> (лимит 100 MB)\n",
			name, tgIDStr, usageMB,
		)

		w.notifyAdmins(ctx, text)
	}
}

// notifyAdmins отправляет сообщение всем администраторам.
func (w *DockerMonitorWorker) notifyAdmins(ctx context.Context, text string) {
	for _, adminID := range w.adminIDs {
		_, _ = w.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    adminID,
			Text:      text,
			ParseMode: "HTML",
		})
	}
}

