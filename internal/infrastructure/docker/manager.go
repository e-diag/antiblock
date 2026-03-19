package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/yourusername/antiblock/internal/domain"
)

const (
	imageNameDD         = "p3terx/mtg"
	imageNameEE         = "nineseconds/mtg:2"
	UserContainerNameDD = "mtg-user-%d-dd"
	UserContainerNameEE = "mtg-user-%d-ee"
	// Старое имя оставляем для обратной совместимости (используется в старых записях/командах).
	UserContainerName = "mtg-user-%d"
)

// Manager инкапсулирует работу с Docker для mtg-контейнеров.
type Manager struct {
	cli *client.Client
}

// GenerateEESecretViaDocker генерирует ee-секрет на удалённом Docker (по TLS),
// запуская ephemeral-контейнер nineseconds/mtg:2.
// Это нужно для Pro-групп: контейнер бота не имеет доступа к локальному Docker daemon.
func (m *Manager) GenerateEESecretViaDocker(ctx context.Context) (string, error) {
	if m == nil || m.cli == nil {
		return "", fmt.Errorf("docker manager is nil")
	}

	cfg := &container.Config{
		Image: imageNameEE,
		Cmd:   []string{"generate-secret", "--hex", "vk.com"},
	}

	hostCfg := &container.HostConfig{}
	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create ee gen container: %w", err)
	}
	containerID := resp.ID

	// На всякий случай: если контекст тайм-аутнулся, try-best-effort очистим контейнер.
	defer func() {
		_ = m.cli.ContainerRemove(context.Background(), containerID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
	}()

	if err := m.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start ee gen container: %w", err)
	}

	waitCh, errCh := m.cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return "", fmt.Errorf("wait ee gen container: %w", err)
		}
	case <-waitCh:
	}

	logs, err := m.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: false,
		Timestamps: false,
		Follow:     false,
	})
	if err != nil {
		return "", fmt.Errorf("logs ee gen container: %w", err)
	}
	defer logs.Close()

	raw, err := io.ReadAll(logs)
	if err != nil {
		return "", fmt.Errorf("read ee gen logs: %w", err)
	}

	// Docker logs могут содержать мультиплекс/непечатаемые байты — фильтруем их.
	clean := strings.Map(func(r rune) rune {
		if r >= 32 && r <= 126 {
			return r
		}
		// Сохраняем переносы строк, чтобы можно было построчно искать.
		if r == '\n' || r == '\r' {
			return r
		}
		return -1
	}, string(raw))

	// Быстрый парсер: ищем строку с префиксом "ee" длиной >= 34.
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ee") && len(line) >= 34 {
			return line, nil
		}
	}

	// Для диагностики — покажем первые символы, но без логов целиком.
	snippet := bytes.NewBufferString(clean).String()
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	return "", fmt.Errorf("ee secret not found in container output: %q", snippet)
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
		log.Printf("[Docker TLS] failed to connect to %s: %v", hostURL, err)
		return nil, err
	}
	return &Manager{cli: cli}, nil
}

// CreateUserContainer запускает контейнер p3terx/mtg для пользователя:
// NetworkMode: host (производительность, BBR), Cmd: run <secret> -b 0.0.0.0:<port> -t 127.0.0.1:0.
// Перед созданием существующий контейнер с тем же именем удаляется (Force: true).
func (m *Manager) CreateUserContainer(
	ctx context.Context,
	userTGID int64,
	proxy *domain.ProxyNode,
) error {
	if proxy == nil {
		return fmt.Errorf("proxy is nil")
	}

	name := fmt.Sprintf(UserContainerNameDD, userTGID)
	portStr := fmt.Sprintf("%d", proxy.Port)

	// Удаляем контейнер с таким именем, если уже существует (Force: true).
	if err := m.cli.ContainerRemove(ctx, name, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}); err != nil {
		if !client.IsErrNotFound(err) {
			log.Printf("[Docker] remove existing container %s failed: %v", name, err)
			return fmt.Errorf("remove existing container %s: %w", name, err)
		}
	}

	// Подтянуть образ (если его нет)
	rc, err := m.cli.ImagePull(ctx, imageNameDD, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	} else {
		log.Printf("[Docker] image pull %s: %v (continuing with existing image)", imageNameDD, err)
	}

	// run <secret> -b 0.0.0.0:<port> -t 127.0.0.1:0 — stats на случайном порту, без конфликта 3129.
	cfg := &container.Config{
		Image: imageNameDD,
		Cmd:   []string{"run", proxy.Secret, "-b", "0.0.0.0:" + portStr, "-t", "127.0.0.1:0"},
	}

	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "1m",
				"max-file": "1",
			},
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		log.Printf("[Docker] container create failed name=%s: %v", name, err)
		return fmt.Errorf("create container: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		log.Printf("[Docker] container start failed id=%s: %v", resp.ID[:12], err)
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

// CreateUserContainerEE создаёт ee-контейнер nineseconds/mtg:2 с ee-секретом на порту (ddPort + 10000).
func (m *Manager) CreateUserContainerEE(ctx context.Context, userTGID int64, proxy *domain.ProxyNode) error {
	if proxy == nil || proxy.SecretEE == "" {
		return fmt.Errorf("proxy or SecretEE is nil")
	}

	name := fmt.Sprintf(UserContainerNameEE, userTGID)
	bind := fmt.Sprintf("0.0.0.0:%d", proxy.Port+10000)

	_ = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true, RemoveVolumes: true})

	rc, err := m.cli.ImagePull(ctx, imageNameEE, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}

	cfg := &container.Config{
		Image: imageNameEE,
		Cmd:   []string{"simple-run", bind, proxy.SecretEE},
	}
	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "1m",
				"max-file": "1",
			},
		},
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create ee container: %w", err)
	}
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("start ee container: %w", err)
	}
	return nil
}

// RemoveUserContainerEE удаляет ee-контейнер пользователя.
func (m *Manager) RemoveUserContainerEE(ctx context.Context, userTGID int64) error {
	name := fmt.Sprintf(UserContainerNameEE, userTGID)
	return m.RemoveUserContainer(ctx, name)
}

// CreateProContainerDD создаёт dd-контейнер для Pro-группы на Pro-сервере.
func (m *Manager) CreateProContainerDD(group *domain.ProGroup) error {
	if group == nil {
		return fmt.Errorf("group is nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := group.ContainerDD
	_ = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})

	rc, err := m.cli.ImagePull(ctx, imageNameDD, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}

	cfg := &container.Config{
		Image: imageNameDD,
		Cmd:   []string{"run", group.SecretDD, "-b", fmt.Sprintf("0.0.0.0:%d", group.PortDD), "-t", "127.0.0.1:0"},
	}
	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "1m",
				"max-file": "1",
			},
		},
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return err
	}
	return m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{})
}

// CreateProContainerEE создаёт ee-контейнер для Pro-группы.
func (m *Manager) CreateProContainerEE(group *domain.ProGroup) error {
	if group == nil {
		return fmt.Errorf("group is nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := group.ContainerEE
	_ = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})

	rc, err := m.cli.ImagePull(ctx, imageNameEE, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}

	bind := fmt.Sprintf("0.0.0.0:%d", group.PortEE)
	cfg := &container.Config{
		Image: imageNameEE,
		Cmd:   []string{"simple-run", bind, group.SecretEE},
	}
	hostCfg := &container.HostConfig{
		NetworkMode: "host",
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "1m",
				"max-file": "1",
			},
		},
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return err
	}
	return m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{})
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

