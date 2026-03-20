package timeweb

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Образы с Docker Hub для Premium на VPS: ee — [nineseconds/mtg], dd — [p3terx/mtg].
const (
	DockerImagePremiumEE = "nineseconds/mtg:2"
	DockerImagePremiumDD = "p3terx/mtg"
)

type SSHClient struct {
	host          string
	port          int
	user          string
	keyPath       string
	password      string
	knownHostKey  string
	onHostKeySeen func(hostKey string)
}

func NewSSHClient(host string, port int, user, keyPath string) *SSHClient {
	return &SSHClient{host: host, port: port, user: user, keyPath: keyPath}
}

func (s *SSHClient) WithPassword(password string) *SSHClient {
	s.password = strings.TrimSpace(password)
	return s
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

func (s *SSHClient) newConfig() (*ssh.ClientConfig, error) {
	auth := make([]ssh.AuthMethod, 0, 2)
	if s.password != "" {
		auth = append(auth, ssh.Password(s.password))
	}
	if strings.TrimSpace(s.keyPath) != "" {
		key, err := os.ReadFile(s.keyPath)
		if err != nil {
			if s.password == "" {
				return nil, fmt.Errorf("read ssh key %s: %w", s.keyPath, err)
			}
		} else {
			signer, err := ssh.ParsePrivateKey(key)
			if err != nil {
				if s.password == "" {
					return nil, fmt.Errorf("parse ssh key: %w", err)
				}
			} else {
				auth = append(auth, ssh.PublicKeys(signer))
			}
		}
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("ssh auth is not configured: empty password and key")
	}

	return &ssh.ClientConfig{
		User:            s.user,
		Auth:            auth,
		HostKeyCallback: s.buildHostKeyCallback(),
		Timeout:         30 * time.Second,
	}, nil
}

func (s *SSHClient) dialAddr() string {
	return net.JoinHostPort(s.host, strconv.Itoa(s.port))
}

func (s *SSHClient) RunCommand(ctx context.Context, cmd string) (string, error) {
	cfg, err := s.newConfig()
	if err != nil {
		return "", err
	}
	addr := s.dialAddr()
	log.Printf("[SSH] connecting to %s user=%s (cmd_len=%d)", addr, s.user, len(cmd))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		log.Printf("[SSH] dial %s failed: %v", addr, err)
		return "", fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	out, err := session.CombinedOutput(cmd)
	if err != nil {
		log.Printf("[SSH] %s:%d → command failed err=%v output_len=%d", s.host, s.port, err, len(out))
	} else {
		log.Printf("[SSH] %s:%d → command ok output_len=%d", s.host, s.port, len(out))
	}
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
			return fmt.Errorf("SSH timeout on %s", s.dialAddr())
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
	log.Printf("[SSH] SetupDocker host=%s:%d — install via get.docker.com", s.host, s.port)
	if _, err := s.RunCommand(ctx, "curl -fsSL https://get.docker.com | sh"); err != nil {
		return fmt.Errorf("install docker: %w", err)
	}
	time.Sleep(5 * time.Second)
	return nil
}

// EnsureDockerInstalled ставит Docker через SetupDocker, если бинарника ещё нет (idempotent).
func (s *SSHClient) EnsureDockerInstalled(ctx context.Context) error {
	if _, err := s.RunCommand(ctx, "docker --version"); err == nil {
		return nil
	}
	log.Printf("[SSH] EnsureDockerInstalled host=%s:%d — docker не найден, ставим", s.host, s.port)
	return s.SetupDocker(ctx)
}

// EnsurePremiumHostTuning применяет сетевые/sysctl и лимиты файлов для высокой нагрузки MTProto.
// Выполняется идемпотентно: повторный вызов безопасен.
func (s *SSHClient) EnsurePremiumHostTuning(ctx context.Context) error {
	log.Printf("[SSH] EnsurePremiumHostTuning host=%s:%d", s.host, s.port)
	inner := `set -e
mkdir -p /etc/sysctl.d /etc/systemd/system/docker.service.d
cat >/etc/sysctl.d/99-antiblock-premium.conf <<'EOF'
net.core.somaxconn = 65535
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_fastopen = 3
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
net.ipv4.tcp_mtu_probing = 1
net.ipv4.tcp_slow_start_after_idle = 0
net.ipv4.ip_nonlocal_bind = 0
net.ipv6.conf.all.disable_ipv6 = 1
net.ipv6.conf.default.disable_ipv6 = 1
net.ipv6.conf.lo.disable_ipv6 = 1
EOF
sysctl --system >/dev/null 2>&1 || true

grep -q "^\* soft nofile 1000000$" /etc/security/limits.conf 2>/dev/null || echo "* soft nofile 1000000" >> /etc/security/limits.conf
grep -q "^\* hard nofile 1000000$" /etc/security/limits.conf 2>/dev/null || echo "* hard nofile 1000000" >> /etc/security/limits.conf
grep -q "^root soft nofile 1000000$" /etc/security/limits.conf 2>/dev/null || echo "root soft nofile 1000000" >> /etc/security/limits.conf
grep -q "^root hard nofile 1000000$" /etc/security/limits.conf 2>/dev/null || echo "root hard nofile 1000000" >> /etc/security/limits.conf

cat >/etc/systemd/system/docker.service.d/limits.conf <<'EOF'
[Service]
LimitNOFILE=1000000
EOF
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl restart docker >/dev/null 2>&1 || true
`
	cmd := "bash -ec " + shellSingleQuote(inner)
	if _, err := s.RunCommand(ctx, cmd); err != nil {
		return fmt.Errorf("ensure premium host tuning: %w", err)
	}
	return nil
}

// EnsurePremiumFirewallPorts открывает 443/8443 в ufw, если файрвол активен (best-effort, без ошибки вверх).
func (s *SSHClient) EnsurePremiumFirewallPorts(ctx context.Context) {
	log.Printf("[SSH] EnsurePremiumFirewallPorts host=%s:%d (443/tcp 8443/tcp если ufw active)", s.host, s.port)
	// root по SSH — без sudo; если ufw не установлен или inactive — ничего не делаем.
	cmd := `sh -c 'if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "Status: active"; then ufw allow 443/tcp || true; ufw allow 8443/tcp || true; fi'`
	if _, err := s.RunCommand(ctx, cmd); err != nil {
		log.Printf("[SSH] EnsurePremiumFirewallPorts host=%s:%d: %v", s.host, s.port, err)
	}
}

// PullPremiumMtgImages подтягивает образы ee/dd на Premium VPS (после установки Docker).
func (s *SSHClient) PullPremiumMtgImages(ctx context.Context) error {
	for _, img := range []string{DockerImagePremiumEE, DockerImagePremiumDD} {
		log.Printf("[SSH] docker pull %s host=%s:%d", img, s.host, s.port)
		if _, err := s.RunCommand(ctx, "docker pull "+img); err != nil {
			return fmt.Errorf("docker pull %s: %w", img, err)
		}
	}
	return nil
}

// shellSingleQuote оборачивает строку в одинарные кавычки для безопасного bash (аргумент -ec).
func shellSingleQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// EnsureHostLocalFloatingIP добавляет IPv4 плавающий адрес на интерфейс с default route (DHCP не трогаем)
// и фиксирует список в /etc/netplan/90-antiblock-floating.yaml + netplan apply.
// Нужно для docker -p <FIP>:443:443 — без локального адреса Docker отвечает 125.
func (s *SSHClient) EnsureHostLocalFloatingIP(ctx context.Context, floatingIP string) error {
	floatingIP = strings.TrimSpace(floatingIP)
	if floatingIP == "" {
		return errors.New("EnsureHostLocalFloatingIP: empty IP")
	}
	if ip := net.ParseIP(floatingIP); ip == nil || ip.To4() == nil {
		return fmt.Errorf("EnsureHostLocalFloatingIP: ожидается IPv4, получено %q", floatingIP)
	}

	// Скрипт: FIP передаётся через окружение (после проверки — только цифры и точки).
	inner := `set -e
STATE=/var/lib/antiblock-floating-v4.conf
NETPLAN=/etc/netplan/90-antiblock-floating.yaml
mkdir -p /var/lib
touch "$STATE"
if ! grep -qxF "$FIP" "$STATE" 2>/dev/null; then echo "$FIP" >> "$STATE"; fi
IFACE=$(ip -4 route show default | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
if [ -z "$IFACE" ]; then
  IFACE=$(ip -4 route show default | awk '{print $5; exit}')
fi
if [ -z "$IFACE" ]; then
  echo "EnsureHostLocalFloatingIP: cannot detect interface for default route" >&2
  exit 1
fi

# Сначала пробуем добавить адрес напрямую (для текущей сессии). DHCP не трогаем.
ip addr add "${FIP}/32" dev "$IFACE" 2>/dev/null || true
sort -u "$STATE" -o "$STATE.tmp" && mv "$STATE.tmp" "$STATE"

# Netplan — best-effort (на текущий момент достаточно ip addr add),
# но добавляем адреса в netplan, чтобы после перезапусков не потерять.
if command -v netplan >/dev/null 2>&1; then
  TMPNP=$(mktemp)
  {
    echo "network:"
    echo "  version: 2"
    echo "  ethernets:"
    echo "    ${IFACE}:"
    echo "      addresses:"
    while IFS= read -r line || [ -n "$line" ]; do
      test -z "$line" && continue
      echo "        - ${line}/32"
    done < "$STATE"
  } > "$TMPNP"
  mv "$TMPNP" "$NETPLAN"
  chmod 600 "$NETPLAN" 2>/dev/null || true
  netplan apply >/dev/null 2>&1 || true
fi

# Проверяем, что адрес реально появился на интерфейсе.
i=0
while [ $i -lt 15 ]; do
  if ip -4 addr show dev "$IFACE" 2>/dev/null | grep -qF "${FIP}/32"; then
    exit 0
  fi
  i=$((i+1))
  sleep 2
done
echo "EnsureHostLocalFloatingIP: ${FIP}/32 not present on ${IFACE}" >&2
exit 1
`
	cmd := "FIP=" + shellSingleQuote(floatingIP) + " bash -ec " + shellSingleQuote(inner)
	log.Printf("[SSH] EnsureHostLocalFloatingIP host=%s:%d fip=%s", s.host, s.port, floatingIP)
	if _, err := s.RunCommand(ctx, cmd); err != nil {
		return fmt.Errorf("ensure floating ip on host: %w", err)
	}
	return nil
}

// GenerateEESecret генерирует ee-секрет на сервере.
func (s *SSHClient) GenerateEESecret(ctx context.Context) (string, error) {
	log.Printf("[SSH] GenerateEESecret host=%s:%d image=%s", s.host, s.port, DockerImagePremiumEE)
	out, err := s.RunCommand(ctx,
		"docker run --rm "+DockerImagePremiumEE+" generate-secret --hex vk.com 2>/dev/null")
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

	log.Printf("[SSH] StartPremiumContainers tg_id=%d host=%s bind_ip=%s dd_secret_prefix=%.8s… ee_secret_prefix=%.8s…",
		tgID, s.host, floatingIP, secretDD, secretEE)

	nameEE := fmt.Sprintf("mtg-user-%d-ee", tgID)
	nameDD := fmt.Sprintf("mtg-user-%d-dd", tgID)

	// Останавливаем старые если есть.
	_, _ = s.RunCommand(ctx, fmt.Sprintf("docker rm -f %s %s 2>/dev/null || true", nameEE, nameDD))

	// ee-контейнер: контейнер слушает 443, проброс строго на floatingIP:443.
	cmdEE := fmt.Sprintf(
		"docker run -d --name %s --restart unless-stopped -p %s:443:443 %s simple-run 0.0.0.0:443 %s",
		nameEE, floatingIP, DockerImagePremiumEE, secretEE,
	)
	if _, err := s.RunCommand(ctx, cmdEE); err != nil {
		return fmt.Errorf("start ee container: %w", err)
	}

	// dd-контейнер: проброс строго на floatingIP:8443.
	cmdDD := fmt.Sprintf(
		"docker run -d --name %s --restart unless-stopped -p %s:8443:8443 %s run %s -b 0.0.0.0:8443 -t 127.0.0.1:0",
		nameDD, floatingIP, DockerImagePremiumDD, secretDD,
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
	log.Printf("[SSH] StopPremiumContainers tg_id=%d host=%s names=%s %s", tgID, s.host, nameDD, nameEE)
	_, _ = s.RunCommand(ctx, fmt.Sprintf("docker rm -f %s %s 2>/dev/null || true", nameEE, nameDD))
}
