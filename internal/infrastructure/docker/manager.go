package docker

import (
	"context"
	"fmt"
	"io"
	"log"
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

	log.Printf("[Docker TLS] connecting to %s (ca=%s cert=%s key=%s)", hostURL, ca, cert, key)
	cli, err := client.NewClientWithOpts(
		client.WithHost(hostURL),
		client.WithTLSClientConfig(ca, cert, key),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	log.Printf("[Docker TLS] connected to %s", hostURL)
	return &Manager{cli: cli}, nil
}

// CreateUserContainer запускает контейнер p3terx/mtg для пользователя:
// NetworkMode: host (производительность, BBR), Cmd: run <secret> -b 0.0.0.0:<port> -stats-addr 127.0.0.1:0.
// Перед созданием существующий контейнер с тем же именем удаляется (Force: true).
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
	log.Printf("[Docker] creating container name=%s port=%d image=%s bind=0.0.0.0:%s (host network)", name, proxy.Port, imageName, portStr)

	// Удаляем контейнер с таким именем, если уже существует (Force: true).
	if err := m.cli.ContainerRemove(ctx, name, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}); err != nil {
		if !client.IsErrNotFound(err) {
			log.Printf("[Docker] remove existing container %s failed: %v", name, err)
			return fmt.Errorf("remove existing container %s: %w", name, err)
		}
	} else {
		log.Printf("[Docker] removed existing container %s", name)
	}

	// Подтянуть образ (если его нет)
	rc, err := m.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
		log.Printf("[Docker] image %s pulled or already present", imageName)
	} else {
		log.Printf("[Docker] image pull %s: %v (continuing with existing image)", imageName, err)
	}

	// run <secret> -b 0.0.0.0:<port> -stats-addr 127.0.0.1:0 — stats на случайном порту, без конфликта 3129.
	cfg := &container.Config{
		Image: imageName,
		Cmd:   []string{"run", proxy.Secret, "-b", "0.0.0.0:" + portStr, "-stats-addr", "127.0.0.1:0"},
	}

	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		log.Printf("[Docker] container create failed name=%s: %v", name, err)
		return fmt.Errorf("create container: %w", err)
	}
	log.Printf("[Docker] container created id=%s name=%s", resp.ID[:12], name)

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		log.Printf("[Docker] container start failed id=%s: %v", resp.ID[:12], err)
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return fmt.Errorf("start container: %w", err)
	}
	log.Printf("[Docker] container started name=%s port=%d (proxy at 0.0.0.0:%s)", name, proxy.Port, portStr)
	return nil
}

// RemoveUserContainer полностью удаляет контейнер по имени. Если контейнера нет — возвращает nil.
func (m *Manager) RemoveUserContainer(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	err := m.cli.ContainerRemove(ctx, name, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if client.IsErrNotFound(err) {
		return nil
	}
	if err != nil {
		log.Printf("[Docker] remove container %s: %v", name, err)
		return err
	}
	log.Printf("[Docker] container removed name=%s", name)
	return nil
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

