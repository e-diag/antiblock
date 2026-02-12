package docker

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/yourusername/antiblock/internal/domain"
)

const (
	imageName         = "p3terx/mtg"
	UserContainerName = "mtg-user-%d" // формат имени контейнера по tg_id
)

// Manager инкапсулирует работу с Docker для mtg-контейнеров.
type Manager struct {
	cli *client.Client
}

// NewManager создает новый Docker‑менеджер, используя переменные окружения Docker (локальный демон).
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

// NewManagerTLS создает Docker‑менеджер для подключения к удалённому демону по TLS (порт 2376).
// certPath — каталог с сертификатами: ca.pem, cert.pem, key.pem (например /antiblock/docker-certs/).
func NewManagerTLS(host string, port int, certPath string) (*Manager, error) {
	if host == "" || certPath == "" {
		return nil, fmt.Errorf("host and certPath are required for TLS")
	}
	if port <= 0 {
		port = 2376
	}
	hostURL := fmt.Sprintf("tcp://%s:%d", host, port)
	ca := filepath.Join(certPath, "ca.pem")
	cert := filepath.Join(certPath, "cert.pem")
	key := filepath.Join(certPath, "key.pem")

	cli, err := client.NewClientWithOpts(
		client.WithHost(hostURL),
		client.WithTLSClientConfig(ca, cert, key),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &Manager{cli: cli}, nil
}

// CreateUserContainer запускает контейнер p3terx/mtg для пользователя по инструкции:
// образ p3terx/mtg, NetworkMode host, Cmd: run <secret> -b 0.0.0.0:<port>, RestartPolicy unless-stopped.
// Секрет и порт берутся из proxy (секрет уже с префиксом dd, 34 символа).
func (m *Manager) CreateUserContainer(
	ctx context.Context,
	userTGID int64,
	proxy *domain.ProxyNode,
) error {
	if proxy == nil {
		return fmt.Errorf("proxy is nil")
	}

	name := fmt.Sprintf(UserContainerName, userTGID)
	portStr := fmt.Sprintf("%d", proxy.Port)

	// На всякий случай удаляем старый контейнер с тем же именем
	_ = m.RemoveUserContainer(ctx, name)

	// Подтянуть образ (если его нет)
	rc, err := m.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}

	// Команда по инструкции: run <secret> -b 0.0.0.0:<port>
	cfg := &container.Config{
		Image: imageName,
		Cmd:   []string{"run", proxy.Secret, "-b", "0.0.0.0:" + portStr},
	}

	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
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

