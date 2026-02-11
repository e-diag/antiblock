package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/yourusername/antiblock/internal/domain"
)

const (
	imageName         = "p3terx/mtg"
	UserContainerName = "mtg-user-%d"     // формат имени контейнера по tg_id
	memoryLimitBytes  = 100 * 1024 * 1024 // 100MB на контейнер mtg
)

// Manager инкапсулирует работу с Docker для mtg-контейнеров.
type Manager struct {
	cli *client.Client
}

// NewManager создает новый Docker‑менеджер, используя переменные окружения Docker.
func NewManager() (*Manager, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &Manager{cli: cli}, nil
}

// CreateUserContainer запускает контейнер p3terx/mtg для конкретного пользователя.
// Используются порт и секрет из ProxyNode, сеть host и лимит памяти 100MB.
func (m *Manager) CreateUserContainer(
	ctx context.Context,
	userTGID int64,
	proxy *domain.ProxyNode,
) error {
	if proxy == nil {
		return fmt.Errorf("proxy is nil")
	}

	name := fmt.Sprintf(UserContainerName, userTGID)

	// На всякий случай удаляем старый контейнер с тем же именем
	_ = m.RemoveUserContainer(ctx, name)

	// Подтянуть образ (если его нет)
	rc, err := m.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}

	cfg := &container.Config{
		Image: imageName,
		Env: []string{
			fmt.Sprintf("PORT=%d", proxy.Port),
			fmt.Sprintf("SECRET=%s", proxy.Secret),
		},
		Cmd: []string{"run"},
	}

	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		Resources: container.Resources{
			Memory: memoryLimitBytes,
		},
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// На случай ошибки запуска удаляем только что созданный контейнер,
		// чтобы не оставлять "мёртвые" контейнеры в системе.
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return fmt.Errorf("start container: %w", err)
	}

	return nil
}

// RemoveUserContainer полностью удаляет контейнер по имени.
func (m *Manager) RemoveUserContainer(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	return m.cli.ContainerRemove(ctx, name, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
}

// IsContainerRunning проверяет, запущен ли контейнер.
func (m *Manager) IsContainerRunning(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	inspect, err := m.cli.ContainerInspect(ctx, name)
	if client.IsErrNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if inspect.ContainerJSONBase == nil || inspect.ContainerJSONBase.State == nil {
		return false, nil
	}
	return inspect.ContainerJSONBase.State.Running, nil
}

