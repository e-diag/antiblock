package timeweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const baseURL = "https://api.timeweb.cloud/api/v1"
const baseRootURL = "https://api.timeweb.cloud"

// ErrFloatingIPDailyLimit возвращается, когда исчерпан суточный лимит
// на создание floating IP (10/день).
var ErrFloatingIPDailyLimit = errors.New("timeweb: daily floating IP limit reached (10/day)")

// ErrServerNotFound — GET /servers/{id} вернул 404 (сервер удалён в панели Timeweb).
var ErrServerNotFound = errors.New("timeweb: server not found")

type Client struct {
	token      string
	httpClient *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// IsConfigured true, если задан непустой API-токен.
func (c *Client) IsConfigured() bool {
	return c != nil && strings.TrimSpace(c.token) != ""
}

func (c *Client) doRequest(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("[TimeWeb] %s %s → transport err: %v", method, path, err)
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[TimeWeb] %s %s → read body err: %v (status=%d)", method, path, err, resp.StatusCode)
		return nil, resp.StatusCode, err
	}
	code := resp.StatusCode
	if code >= 200 && code < 300 {
		log.Printf("[TimeWeb] %s %s → status=%d OK", method, path, code)
	} else {
		preview := strings.TrimSpace(string(respBody))
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		log.Printf("[TimeWeb] %s %s → status=%d body=%q", method, path, code, preview)
	}
	return respBody, code, nil
}

// --- Floating IPs ---

// FloatingIP соответствует схеме `floating-ip`.
type FloatingIP struct {
	ID               string `json:"id"`
	IP               string `json:"ip,omitempty"`
	IsDDOSGuard      bool   `json:"is_ddos_guard,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`

	// Ниже поля не используются напрямую, но оставлены для полноты.
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   any    `json:"resource_id,omitempty"`
	Comment      string `json:"comment,omitempty"`
	Ptr          string `json:"ptr,omitempty"`
}

type createFloatingIPRequest struct {
	IsDDOSGuard      bool   `json:"is_ddos_guard"`
	AvailabilityZone string `json:"availability_zone"`
}

type bindFloatingIPRequest struct {
	ResourceType string `json:"resource_type"` // usually "server"
	ResourceID   any    `json:"resource_id"`   // number in API schema
}

// CreateFloatingIP создаёт floating IP в указанной зоне.
func (c *Client) CreateFloatingIP(ctx context.Context, zone string) (*FloatingIP, error) {
	req := createFloatingIPRequest{
		IsDDOSGuard:      false,
		AvailabilityZone: zone,
	}
	respBody, code, err := c.doRequest(ctx, http.MethodPost, "/floating-ips", &req)
	if err != nil {
		return nil, err
	}
	if code == http.StatusTooManyRequests {
		bodyStr := strings.ToLower(string(respBody))
		if strings.Contains(bodyStr, "limit") ||
			strings.Contains(bodyStr, "daily") ||
			strings.Contains(bodyStr, "лимит") {
			return nil, ErrFloatingIPDailyLimit
		}
		return nil, fmt.Errorf("timeweb rate limit (429): %s", strings.TrimSpace(string(respBody)))
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb create floating ip: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		IP string `json:"ip"`
		// На практике ip приходит объектом `floating-ip` (allOf), поэтому распакуем корректно:
		FloatingIP FloatingIP `json:"-"`
	}
	// Универсальный распарсер: response содержит поле `ip`, которое является floating-ip объектом.
	// Поэтому делаем unmarshal через вспомогательную структуру.
	var out2 struct {
		IP FloatingIP `json:"ip"`
	}
	if err := json.Unmarshal(respBody, &out2); err == nil && out2.IP.ID != "" {
		return &out2.IP, nil
	}
	// fallback — если unmarshal не распознал тип, вернём ошибку
	_ = out
	return nil, fmt.Errorf("timeweb create floating ip: unexpected response: %s", strings.TrimSpace(string(respBody)))
}

// BindFloatingIP привязывает floating IP к серверу.
func (c *Client) BindFloatingIP(ctx context.Context, floatingIPID string, serverID int) error {
	req := bindFloatingIPRequest{
		ResourceType: "server",
		ResourceID:   serverID,
	}
	respBody, code, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("/floating-ips/%s/bind", floatingIPID), &req)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("timeweb bind floating ip: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// UnbindFloatingIP отвязывает floating IP от сервера.
func (c *Client) UnbindFloatingIP(ctx context.Context, floatingIPID string) error {
	respBody, code, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("/floating-ips/%s/unbind", floatingIPID), nil)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("timeweb unbind floating ip: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// DeleteFloatingIP удаляет floating IP.
func (c *Client) DeleteFloatingIP(ctx context.Context, floatingIPID string) error {
	respBody, code, err := c.doRequest(ctx, http.MethodDelete, fmt.Sprintf("/floating-ips/%s", floatingIPID), nil)
	if err != nil {
		return err
	}
	// API обычно отдаёт 204 No Content
	if code < 200 || code >= 300 {
		return fmt.Errorf("timeweb delete floating ip: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// --- Servers ---

// Server соответствует схеме `vds` (ответ для сервера).
type Server struct {
	ID               int             `json:"id"`
	Name             string          `json:"name,omitempty"`
	Status           string          `json:"status,omitempty"`
	RootPass         string          `json:"root_pass,omitempty"`
	AvailabilityZone string          `json:"availability_zone,omitempty"`
	Networks         []ServerNetwork `json:"networks,omitempty"`
}

type ServerNetwork struct {
	Type string     `json:"type,omitempty"`
	Ips  []ServerIP `json:"ips,omitempty"`
}

type ServerIP struct {
	Type   string `json:"type,omitempty"`
	IP     string `json:"ip,omitempty"`
	IsMain bool   `json:"is_main,omitempty"`
}

// ExtractMainIPv4 возвращает публичный IPv4 для SSH/API (только v4, без IPv6).
// Сначала главный (is_main) IPv4 в public-сети, иначе любой IPv4 в public.
func ExtractMainIPv4(srv *Server) string {
	if srv == nil {
		return ""
	}
	for _, n := range srv.Networks {
		if !strings.EqualFold(n.Type, "public") {
			continue
		}
		var anyV4 string
		for _, ip := range n.Ips {
			if ip.IP == "" || !parseableIPv4(ip.IP) {
				continue
			}
			if ip.IsMain {
				return ip.IP
			}
			if anyV4 == "" {
				anyV4 = ip.IP
			}
		}
		if anyV4 != "" {
			return anyV4
		}
	}
	return ""
}

func parseableIPv4(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	return ip != nil && ip.To4() != nil
}

// AddServerIP добавляет IP серверу (POST /api/v1/servers/{id}/ips). Для публичного IPv4 укажите ipType "ipv4".
func (c *Client) AddServerIP(ctx context.Context, serverID int, ipType string) (assignedIP string, err error) {
	if ipType == "" {
		ipType = "ipv4"
	}
	body := map[string]string{"type": ipType}
	respBody, code, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("/servers/%d/ips", serverID), body)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("timeweb add server ip: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		ServerIP struct {
			IP string `json:"ip"`
		} `json:"server_ip"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("timeweb add server ip: unmarshal: %w", err)
	}
	return strings.TrimSpace(out.ServerIP.IP), nil
}

// CreateServerRequest соответствует схеме `create-server`.
// Минимально заполняем поля, которые нужны для создания VPS в нужной конфигурации.
type CreateServerRequest struct {
	Name             string `json:"name"`
	PresetID         int    `json:"preset_id,omitempty"`
	OsID             int    `json:"os_id,omitempty"`
	ImageID          string `json:"image_id,omitempty"` // UUID своего образа; взаимоисключение с OsID (см. OpenAPI create-server).
	AvailabilityZone string `json:"availability_zone,omitempty"`
	IsDDOSGuard      bool   `json:"is_ddos_guard,omitempty"`
	SSHKeysIDs       []int  `json:"ssh_keys_ids,omitempty"`
}

type SSHKey struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetSSHKeys возвращает список SSH-ключей аккаунта.
func (c *Client) GetSSHKeys(ctx context.Context) ([]SSHKey, error) {
	respBody, code, err := c.doRequest(ctx, http.MethodGet, "/ssh-keys", nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb get ssh-keys: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		SSHKeys []SSHKey `json:"ssh_keys"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("timeweb get ssh-keys: unmarshal: %w", err)
	}
	return out.SSHKeys, nil
}

// UploadSSHKey загружает публичный SSH-ключ в TimeWeb и возвращает объект ключа.
func (c *Client) UploadSSHKey(ctx context.Context, name, publicKey string) (*SSHKey, error) {
	body := map[string]string{
		"name": name,
		"body": publicKey,
	}
	respBody, code, err := c.doRequest(ctx, http.MethodPost, "/ssh-keys", body)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb upload ssh-key: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		SSHKey SSHKey `json:"ssh_key"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("timeweb upload ssh-key: unmarshal: %w", err)
	}
	return &out.SSHKey, nil
}

// CreateServer создаёт новый сервер (VPS).
func (c *Client) CreateServer(ctx context.Context, req CreateServerRequest) (*Server, error) {
	respBody, code, err := c.doRequest(ctx, http.MethodPost, "/servers", &req)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb create server: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		Server Server `json:"server"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("timeweb create server: unmarshal: %w", err)
	}
	return &out.Server, nil
}

// GetServer возвращает информацию о сервере.
func (c *Client) GetServer(ctx context.Context, serverID int) (*Server, error) {
	respBody, code, err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("/servers/%d", serverID), nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, ErrServerNotFound
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb get server: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		Server Server `json:"server"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("timeweb get server: unmarshal: %w", err)
	}
	return &out.Server, nil
}

// PerformServerAction вызывает POST /servers/{id}/action (например action=reset_password для нового root_pass).
func (c *Client) PerformServerAction(ctx context.Context, serverID int, action string) error {
	action = strings.TrimSpace(action)
	if action == "" {
		return errors.New("timeweb: empty server action")
	}
	body := map[string]string{"action": action}
	respBody, code, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("/servers/%d/action", serverID), body)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return ErrServerNotFound
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("timeweb server action %q: http %d: %s", action, code, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// WaitServerReady polling каждые 10 сек, таймаут 10 мин.
func (c *Client) WaitServerReady(ctx context.Context, serverID int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeweb wait server ready timeout: %w", ctx.Err())
		case <-ticker.C:
			srv, err := c.GetServer(ctx, serverID)
			if err != nil {
				if errors.Is(err, ErrServerNotFound) {
					return err
				}
				continue
			}
			// Swagger enum для vds.status содержит "on".
			if strings.EqualFold(srv.Status, "on") {
				return nil
			}
		}
	}
}

// --- Directories (OS для серверов / тарифы) ---
// См. OpenAPI Timeweb Cloud: https://timeweb.cloud/api-docs
// — список ОС: GET /api/v1/os/servers (operationId getOsList, ключ servers_os).
// — GET /api/v1/images — образы дисков аккаунта, для выбора ОС при создании VPS не подходит.

// ServerOS — элемент ответа GET /api/v1/os/servers (схема servers-os).
type ServerOS struct {
	ID              int    `json:"id"`
	Family          string `json:"family,omitempty"`
	Name            string `json:"name,omitempty"`
	Version         string `json:"version,omitempty"`
	VersionCodename string `json:"version_codename,omitempty"`
	Description     string `json:"description,omitempty"`
}

func parseServersOSListBody(respBody []byte) ([]ServerOS, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &top); err != nil {
		return nil, fmt.Errorf("timeweb os/servers: json: %w", err)
	}
	for _, key := range []string{"servers_os", "servers_OS"} {
		raw, ok := top[key]
		if !ok || len(raw) < 3 || string(raw) == "null" {
			continue
		}
		var list []ServerOS
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("timeweb os/servers: %s: %w", key, err)
		}
		return list, nil
	}
	return []ServerOS{}, nil
}

// GetServerOSList возвращает список операционных систем для облачных серверов.
func (c *Client) GetServerOSList(ctx context.Context) ([]ServerOS, error) {
	respBody, code, err := c.doRequest(ctx, http.MethodGet, "/os/servers", nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb get os/servers: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	return parseServersOSListBody(respBody)
}

// ResolveUbuntu2404OSID возвращает os_id для Ubuntu 24.04 LTS из GET /os/servers.
// Сопоставление: family/name содержит «ubuntu», version 24.04 или codename noble.
func (c *Client) ResolveUbuntu2404OSID(ctx context.Context) (int, error) {
	list, err := c.GetServerOSList(ctx)
	if err != nil {
		return 0, err
	}
	isUbuntu := func(o ServerOS) bool {
		n := strings.ToLower(strings.TrimSpace(o.Name))
		f := strings.ToLower(strings.TrimSpace(o.Family))
		return strings.Contains(n, "ubuntu") || f == "ubuntu" || strings.Contains(f, "ubuntu")
	}
	is2404 := func(o ServerOS) bool {
		v := strings.TrimSpace(o.Version)
		cod := strings.ToLower(strings.TrimSpace(o.VersionCodename))
		if cod == "noble" {
			return true
		}
		if v == "24.04" || strings.HasPrefix(v, "24.04 ") {
			return true
		}
		return strings.Contains(v, "24.04")
	}
	var nobleID, otherID int
	for _, o := range list {
		if !isUbuntu(o) || !is2404(o) {
			continue
		}
		cod := strings.ToLower(strings.TrimSpace(o.VersionCodename))
		if cod == "noble" && nobleID == 0 {
			nobleID = o.ID
		}
		if otherID == 0 {
			otherID = o.ID
		}
	}
	if nobleID != 0 {
		return nobleID, nil
	}
	if otherID != 0 {
		return otherID, nil
	}
	return 0, fmt.Errorf("timeweb: ubuntu 24.04 not found in /os/servers response")
}

// Configuration соответствует схеме `servers-preset`.
type Configuration struct {
	ID               int     `json:"id"`
	Location         string  `json:"location,omitempty"`
	Price            float64 `json:"price,omitempty"`
	CPU              int     `json:"cpu,omitempty"`
	RAM              int     `json:"ram,omitempty"`
	Disk             int     `json:"disk,omitempty"`
	DescriptionShort string  `json:"description_short,omitempty"`
}

type configurationsOutResponse struct {
	ServerPresets []Configuration `json:"server_presets"`
}

// LocationDTO соответствует схеме `location-dto` из /api/v2/locations.
type LocationDTO struct {
	AvailabilityZones []string `json:"availability_zones"`
}

// GetRegions возвращает список доступных availability_zone.
// Используется для менеджерского диалога создания Premium VPS.
func (c *Client) GetRegions(ctx context.Context) ([]string, error) {
	// Endpoint живёт в /api/v2 согласно swagger bundle.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseRootURL+"/api/v2/locations", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("timeweb get regions: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Locations []LocationDTO `json:"locations"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("timeweb get regions: unmarshal: %w", err)
	}

	set := make(map[string]struct{})
	for _, l := range out.Locations {
		for _, z := range l.AvailabilityZones {
			z = strings.TrimSpace(z)
			if z != "" {
				set[z] = struct{}{}
			}
		}
	}

	regions := make([]string, 0, len(set))
	for z := range set {
		regions = append(regions, z)
	}
	return regions, nil
}

// GetRegionsWithOSImages — зоны для диалога создания VPS (из /api/v2/locations).
// Список ОС не связан с GET /images; ОС загружаются через GetServerOSList.
func (c *Client) GetRegionsWithOSImages(ctx context.Context) ([]string, error) {
	regions, err := c.GetRegions(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(regions)
	return regions, nil
}

// GetConfigurations возвращает список доступных конфигураций VPS.
func (c *Client) GetConfigurations(ctx context.Context) ([]Configuration, error) {
	respBody, code, err := c.doRequest(ctx, http.MethodGet, "/presets/servers?limit=200&offset=0", nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb get configurations: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	var out configurationsOutResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("timeweb get configurations: unmarshal: %w", err)
	}
	return out.ServerPresets, nil
}
