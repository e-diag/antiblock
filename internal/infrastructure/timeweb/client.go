package timeweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const baseURL = "https://api.timeweb.cloud/api/v1"
const baseRootURL = "https://api.timeweb.cloud"

// ErrFloatingIPDailyLimit возвращается, когда исчерпан суточный лимит
// на создание floating IP (10/день).
var ErrFloatingIPDailyLimit = errors.New("timeweb: daily floating IP limit reached (10/day)")

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

// CreateServerRequest соответствует схеме `create-server`.
// Минимально заполняем поля, которые нужны для создания VPS в нужной конфигурации.
type CreateServerRequest struct {
	Name             string `json:"name"`
	PresetID         int    `json:"preset_id,omitempty"`
	ImageID          string `json:"image_id,omitempty"`
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
				continue
			}
			// Swagger enum для vds.status содержит "on".
			if strings.EqualFold(srv.Status, "on") {
				return nil
			}
		}
	}
}

// --- Directories (OS images / Configurations) ---

// OSImage соответствует схеме `image`.
type OSImage struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	AvailabilityZones []string `json:"availability_zones,omitempty"`
}

// filterOSImagesByAvailabilityZone оставляет образы для указанной зоны, если в ответе API есть поле зон.
// Если ни у одного образа зоны не заданы — возвращает images как есть.
func filterOSImagesByAvailabilityZone(images []OSImage, zone string) []OSImage {
	az := strings.TrimSpace(zone)
	if az == "" {
		return images
	}
	hasZoneInfo := false
	for _, img := range images {
		if len(img.AvailabilityZones) > 0 {
			hasZoneInfo = true
			break
		}
	}
	if !hasZoneInfo {
		return images
	}
	out := make([]OSImage, 0, len(images))
	for _, img := range images {
		for _, z := range img.AvailabilityZones {
			if strings.EqualFold(strings.TrimSpace(z), az) {
				out = append(out, img)
				break
			}
		}
	}
	return out
}

// osImageJSON — разбор элемента списка образов (id может быть строкой или числом).
type osImageJSON struct {
	ID                json.RawMessage `json:"id"`
	Name              string          `json:"name"`
	AvailabilityZone  string          `json:"availability_zone,omitempty"`
	AvailabilityZones []string        `json:"availability_zones,omitempty"`
}

func normalizeOSImageID(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return strconv.FormatInt(int64(f), 10)
	}
	return strings.Trim(string(raw), `"`)
}

func mergeImageZones(j osImageJSON) []string {
	if len(j.AvailabilityZones) > 0 {
		return j.AvailabilityZones
	}
	if z := strings.TrimSpace(j.AvailabilityZone); z != "" {
		return []string{z}
	}
	return nil
}

func osImageJSONToOSImages(jList []osImageJSON) []OSImage {
	out := make([]OSImage, 0, len(jList))
	for _, j := range jList {
		id := normalizeOSImageID(j.ID)
		if id == "" && strings.TrimSpace(j.Name) == "" {
			continue
		}
		out = append(out, OSImage{
			ID:                id,
			Name:              j.Name,
			AvailabilityZones: mergeImageZones(j),
		})
	}
	return out
}

// parseOSImagesFromBody извлекает список образов из тела ответа GET /images (разные варианты ключей в OpenAPI).
func parseOSImagesFromBody(respBody []byte) ([]OSImage, error) {
	respBody = bytes.TrimSpace(respBody)
	if len(respBody) > 0 && respBody[0] == '[' {
		var jList []osImageJSON
		if err := json.Unmarshal(respBody, &jList); err == nil {
			return osImageJSONToOSImages(jList), nil
		}
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &top); err != nil {
		return nil, fmt.Errorf("timeweb images: top-level json: %w", err)
	}

	tryKeys := []string{"images", "image_list", "server_images", "data"}
	var rawList json.RawMessage
	for _, k := range tryKeys {
		if v, ok := top[k]; ok && len(v) > 2 && string(v) != "null" {
			rawList = v
			break
		}
	}
	// Вложенный объект image: { "images": [...] }
	if len(rawList) == 0 {
		if imgWrap, ok := top["image"]; ok && len(imgWrap) > 2 && string(imgWrap) != "null" {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(imgWrap, &nested); err == nil {
				for _, k := range tryKeys {
					if v, ok := nested[k]; ok && len(v) > 2 && string(v) != "null" {
						rawList = v
						break
					}
				}
			}
		}
	}
	if len(rawList) == 0 {
		return []OSImage{}, nil
	}

	var jList []osImageJSON
	if err := json.Unmarshal(rawList, &jList); err != nil {
		return nil, fmt.Errorf("timeweb images: list: %w", err)
	}
	return osImageJSONToOSImages(jList), nil
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

// fetchOSImagesAll загружает список образов одним запросом без availability_zone.
// Параметр availability_zone в query у TimeWeb часто даёт 200 и пустой массив — не используем его для выборки.
func (c *Client) fetchOSImagesAll(ctx context.Context) ([]OSImage, error) {
	q := url.Values{}
	q.Set("limit", "200")
	q.Set("offset", "0")
	path := "/images?" + q.Encode()

	respBody, code, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("timeweb get os images: http %d: %s", code, strings.TrimSpace(string(respBody)))
	}
	return parseOSImagesFromBody(respBody)
}

// GetOSImages возвращает список образов ОС, при необходимости отфильтрованный по зоне (по полю availability_zones в JSON).
func (c *Client) GetOSImages(ctx context.Context, availabilityZone string) ([]OSImage, error) {
	all, err := c.fetchOSImagesAll(ctx)
	if err != nil {
		return nil, err
	}
	az := strings.TrimSpace(availabilityZone)
	if az == "" {
		return all, nil
	}
	filtered := filterOSImagesByAvailabilityZone(all, az)
	if len(filtered) > 0 {
		return filtered, nil
	}
	// В ответе нет совпадения зоны с кодом из /locations — показываем полный список (сервер всё равно привяжет образ к выбранной зоне при создании).
	return all, nil
}

// GetRegionsWithOSImages возвращает зоны, где по данным API есть образы (поле availability_zones).
// Если у образов зоны не указаны — fallback на все зоны из GetRegions (как раньше).
func (c *Client) GetRegionsWithOSImages(ctx context.Context) ([]string, error) {
	all, err := c.fetchOSImagesAll(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("timeweb: пустой список образов ОС")
	}
	set := make(map[string]struct{})
	for _, img := range all {
		for _, z := range img.AvailabilityZones {
			z = strings.TrimSpace(z)
			if z != "" {
				set[z] = struct{}{}
			}
		}
	}
	if len(set) == 0 {
		return c.GetRegions(ctx)
	}
	regions := make([]string, 0, len(set))
	for z := range set {
		regions = append(regions, z)
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
