package timeweb

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHClient struct {
	host          string
	port          int
	user          string
	password      string
	knownHostKey  string
	onHostKeySeen func(hostKey string)
}

func NewSSHClient(host string, port int, user, password string) *SSHClient {
	return &SSHClient{host: host, port: port, user: user, password: password}
}

// WithKnownHostKey задаёт известный host key для верификации.
// Если knownKey пустой — при первом подключении ключ будет принят и сохранён через onSave.
func (s *SSHClient) WithKnownHostKey(knownKey string, onSave func(hostKey string)) *SSHClient {
	s.knownHostKey = knownKey
	s.onHostKeySeen = onSave
	return s
}

func (s *SSHClient) buildHostKeyCallback() ssh.HostKeyCallback {
	if s.knownHostKey != "" {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			got := base64.StdEncoding.EncodeToString(key.Marshal())
			if got != s.knownHostKey {
				expectedPrefix := s.knownHostKey
				if len(expectedPrefix) > 16 {
					expectedPrefix = expectedPrefix[:16] + "..."
				}
				gotPrefix := got
				if len(gotPrefix) > 16 {
					gotPrefix = gotPrefix[:16] + "..."
				}
				return fmt.Errorf("SSH host key mismatch for %s: expected %s, got %s", hostname, expectedPrefix, gotPrefix)
			}
			return nil
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		hostKey := base64.StdEncoding.EncodeToString(key.Marshal())
		if s.onHostKeySeen != nil {
			s.onHostKeySeen(hostKey)
		}
		return nil
	}
}

func (s *SSHClient) newConfig() *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            s.user,
		Auth:            []ssh.AuthMethod{ssh.Password(s.password)},
		HostKeyCallback: s.buildHostKeyCallback(),
		Timeout:         30 * time.Second,
	}
}

func (s *SSHClient) RunCommand(ctx context.Context, cmd string) (string, error) {
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", s.host, s.port), s.newConfig())
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", s.host, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

// WaitSSHReady ждёт пока SSH станет доступен (сервер загружается).
func (s *SSHClient) WaitSSHReady(ctx context.Context) error {
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.C:
			return fmt.Errorf("SSH timeout on %s", s.host)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := s.RunCommand(ctx, "echo ok"); err == nil {
				return nil
			}
		}
	}
}

// SetupDocker устанавливает Docker на чистый Ubuntu-сервер.
func (s *SSHClient) SetupDocker(ctx context.Context) error {
	if _, err := s.RunCommand(ctx, "curl -fsSL https://get.docker.com | sh"); err != nil {
		return fmt.Errorf("install docker: %w", err)
	}
	time.Sleep(5 * time.Second)
	return nil
}

// GenerateEESecret генерирует ee-секрет на сервере.
func (s *SSHClient) GenerateEESecret(ctx context.Context) (string, error) {
	out, err := s.RunCommand(ctx,
		"docker run --rm nineseconds/mtg:2 generate-secret --hex vk.com 2>/dev/null")
	if err != nil {
		return "", fmt.Errorf("generate ee secret: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ee") && len(line) >= 34 {
			return line, nil
		}
	}
	return "", fmt.Errorf("ee secret not found in output")
}

// StartPremiumContainers запускает ee (443) и dd (8443) контейнеры,
// биндируя их на конкретный floatingIP.
func (s *SSHClient) StartPremiumContainers(ctx context.Context, tgID int64, floatingIP, secretDD, secretEE string) error {
	if floatingIP == "" || secretDD == "" || secretEE == "" {
		return fmt.Errorf("StartPremiumContainers: empty params")
	}

	nameEE := fmt.Sprintf("mtg-user-%d-ee", tgID)
	nameDD := fmt.Sprintf("mtg-user-%d-dd", tgID)

	// Останавливаем старые если есть.
	_, _ = s.RunCommand(ctx, fmt.Sprintf("docker rm -f %s %s 2>/dev/null || true", nameEE, nameDD))

	// ee-контейнер: контейнер слушает 443, проброс строго на floatingIP:443.
	cmdEE := fmt.Sprintf(
		"docker run -d --name %s --restart unless-stopped -p %s:443:443 nineseconds/mtg:2 simple-run 0.0.0.0:443 %s",
		nameEE, floatingIP, secretEE,
	)
	if _, err := s.RunCommand(ctx, cmdEE); err != nil {
		return fmt.Errorf("start ee container: %w", err)
	}

	// dd-контейнер: проброс строго на floatingIP:8443.
	cmdDD := fmt.Sprintf(
		"docker run -d --name %s --restart unless-stopped -p %s:8443:8443 p3terx/mtg run %s -b 0.0.0.0:8443 -t 127.0.0.1:0",
		nameDD, floatingIP, secretDD,
	)
	if _, err := s.RunCommand(ctx, cmdDD); err != nil {
		// ee уже работает, dd мог не стартовать.
		return fmt.Errorf("start dd container: %w", err)
	}

	return nil
}

// StopPremiumContainers останавливает контейнеры юзера.
func (s *SSHClient) StopPremiumContainers(ctx context.Context, tgID int64) {
	nameEE := fmt.Sprintf("mtg-user-%d-ee", tgID)
	nameDD := fmt.Sprintf("mtg-user-%d-dd", tgID)
	_, _ = s.RunCommand(ctx, fmt.Sprintf("docker rm -f %s %s 2>/dev/null || true", nameEE, nameDD))
}

