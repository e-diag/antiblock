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
	imageNameEE = "nineseconds/mtg:2"
	// Legacy (p3terx) — только для удаления старых контейнеров при миграции.
	imageNameLegacyDD         = "p3terx/mtg"
	UserContainerNameEE1      = "mtg-user-%d-ee1"
	UserContainerNameEE2      = "mtg-user-%d-ee2"
	UserContainerNameLegacyDD = "mtg-user-%d-dd"
	UserContainerNameLegacyEE = "mtg-user-%d-ee"
	ProContainerNameEE1       = "mtg-pro-%d-ee1"
	ProContainerNameEE2       = "mtg-pro-%d-ee2"
	ProContainerNameLegacyDD  = "mtg-pro-%d-dd"
	ProContainerNameLegacyEE  = "mtg-pro-%d-ee"
	// Старое имя оставляем для обратной совместимости (используется в старых записях/командах).
	UserContainerName = "mtg-user-%d"
)

// Manager инкапсулирует работу с Docker для mtg-контейнеров.
type Manager struct {
	cli *client.Client
}

func (m *Manager) GetClient() *client.Client {
	if m == nil {
		return nil
	}
	return m.cli
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
	var containerID string
	defer func() {
		if containerID != "" {
			_ = m.cli.ContainerRemove(context.Background(), containerID, container.RemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
		}
	}()

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create ee gen container: %w", err)
	}
	containerID = resp.ID

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
		line = strings.TrimPrefix(line, "/")
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

// createUserEEOnPort — nineseconds/mtg:2 на host network (legacy Premium на одном сервере).
func (m *Manager) createUserEEOnPort(ctx context.Context, containerName string, port int, secret string) error {
	if secret == "" {
		return fmt.Errorf("empty ee secret")
	}
	_ = m.cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true, RemoveVolumes: true})

	rc, err := m.cli.ImagePull(ctx, imageNameEE, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	} else {
		log.Printf("[Docker] image pull %s: %v (continuing)", imageNameEE, err)
	}

	bind := fmt.Sprintf("0.0.0.0:%d", port)
	cfg := &container.Config{
		Image: imageNameEE,
		Cmd:   []string{"simple-run", bind, secret},
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
	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("create ee container %s: %w", containerName, err)
	}
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("start ee container %s: %w", containerName, err)
	}
	return nil
}

// CreateUserPremiumEEContainers — два ee-прокси (nineseconds) для legacy Premium: proxy.Port и proxy.Port+10000.
func (m *Manager) CreateUserPremiumEEContainers(ctx context.Context, userTGID int64, proxy *domain.ProxyNode) error {
	if proxy == nil || proxy.Secret == "" || proxy.SecretEE == "" {
		return fmt.Errorf("proxy or ee secrets empty")
	}
	if err := m.createUserEEOnPort(ctx, fmt.Sprintf(UserContainerNameEE1, userTGID), proxy.Port, proxy.Secret); err != nil {
		return err
	}
	return m.createUserEEOnPort(ctx, fmt.Sprintf(UserContainerNameEE2, userTGID), proxy.Port+10000, proxy.SecretEE)
}

// RemoveUserPremiumEEContainers удаляет ee1/ee2 и старые dd/ee контейнеры пользователя (миграция).
// Включая bare mtg-user-{id} (старые p3terx до схемы с суффиксами) — иначе порт остаётся занят.
func (m *Manager) RemoveUserPremiumEEContainers(ctx context.Context, userTGID int64) {
	for _, name := range []string{
		fmt.Sprintf(UserContainerNameEE1, userTGID),
		fmt.Sprintf(UserContainerNameEE2, userTGID),
		fmt.Sprintf(UserContainerNameLegacyDD, userTGID),
		fmt.Sprintf(UserContainerNameLegacyEE, userTGID),
		fmt.Sprintf(UserContainerName, userTGID),
	} {
		_ = m.RemoveUserContainer(ctx, name)
	}
}

// RemoveUserContainerEE удаляет старый ee-контейнер mtg-user-{id}-ee.
func (m *Manager) RemoveUserContainerEE(ctx context.Context, userTGID int64) error {
	name := fmt.Sprintf(UserContainerNameLegacyEE, userTGID)
	return m.RemoveUserContainer(ctx, name)
}

// CreateProGroupEEContainers поднимает два ee-контейнера Pro-группы (nineseconds).
// Поля PortDD/SecretDD и PortEE/SecretEE — два слота ee (имена колонок исторические).
func (m *Manager) CreateProGroupEEContainers(group *domain.ProGroup) error {
	if group == nil {
		return fmt.Errorf("group is nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.createProEEContainer(ctx, group.ContainerDD, group.PortDD, group.SecretDD); err != nil {
		return fmt.Errorf("pro ee slot 1: %w", err)
	}
	if err := m.createProEEContainer(ctx, group.ContainerEE, group.PortEE, group.SecretEE); err != nil {
		return fmt.Errorf("pro ee slot 2: %w", err)
	}
	return nil
}

func (m *Manager) createProEEContainer(ctx context.Context, name string, port int, secret string) error {
	_ = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})

	rc, err := m.cli.ImagePull(ctx, imageNameEE, image.PullOptions{})
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}

	bind := fmt.Sprintf("0.0.0.0:%d", port)
	cfg := &container.Config{
		Image: imageNameEE,
		Cmd:   []string{"simple-run", bind, secret},
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
