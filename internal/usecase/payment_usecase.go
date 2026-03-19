package usecase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
)

// PaymentUseCase определяет бизнес-логику для работы с платежами
type PaymentUseCase interface {
	CreateInvoice(amount float64, currency string, description string, userID int64) (payURL string, invoiceID int64, err error)
	CheckInvoiceStatus(invoiceID string) (bool, error)
	GetUserIDByInvoiceID(invoiceID int64) (int64, bool)
	// SetInvoiceMeta сохраняет тип продукта и количество дней для инвойса.
	SetInvoiceMeta(invoiceID int64, kind string, daysGranted int) error
	// GetInvoice возвращает сохранённый инвойс по invoiceID платёжной системы.
	GetInvoice(invoiceID int64) (*domain.Invoice, error)
	SetInvoiceMessage(invoiceID int64, chatID int64, messageID int64) error
	GetInvoiceMessageInfo(invoiceID int64) (chatID int64, messageID int64, ok bool)
	MarkInvoicePaid(invoiceID int64) error
	// CancelInvoice отменяет/удаляет инвойс в платёжной системе (если поддерживается) и помечает его отменённым в БД.
	CancelInvoice(invoiceID int64) error
	RecordStarPayment(tgID int64, amountTotal int64, currency string, daysGranted int, telegramPaymentChargeID string) error
}

type paymentUseCase struct {
	apiToken        string
	apiURL          string
	client          *http.Client
	invRepo         InvoiceRepository
	starPaymentRepo StarPaymentRepository
}

// InvoiceRepository минимальный интерфейс для сохранения счетов
type InvoiceRepository interface {
	Create(inv *domain.Invoice) error
	GetByInvoiceID(invoiceID int64) (*domain.Invoice, error)
	Update(inv *domain.Invoice) error
}

// StarPaymentRepository — сохранение оплат Telegram Stars
type StarPaymentRepository interface {
	Create(p *domain.StarPayment) error
}

// NewPaymentUseCase создает новый use case для платежей
func NewPaymentUseCase(apiToken, apiURL string, invRepo InvoiceRepository, starPaymentRepo StarPaymentRepository) PaymentUseCase {
	return &paymentUseCase{
		apiToken:        apiToken,
		apiURL:          apiURL,
		client:          &http.Client{Timeout: 30 * time.Second},
		invRepo:         invRepo,
		starPaymentRepo: starPaymentRepo,
	}
}

// CryptoCreateInvoiceRequest для Crypto Pay API: asset (TON, USDT, ...), amount (строка), description, payload.
// Оставлено для совместимости; для xRocket используется XRocketCreateInvoiceRequest.
type CreateInvoiceRequest struct {
	Asset       string `json:"asset"`                  // TON, USDT, BTC, ...
	Amount      string `json:"amount"`                 // сумма в криптовалюте, напр. "10.5"
	Description string `json:"description,omitempty"`
	Payload     string `json:"payload,omitempty"`      // до 4kb, например user ID для webhook
}

type CreateInvoiceResponse struct {
	OK     bool   `json:"ok"`
	Result Result `json:"result"`
}

type Result struct {
	InvoiceID       int64  `json:"invoice_id"`
	Status          string `json:"status"`
	PayURL          string `json:"pay_url"`           // deprecated, но может приходить
	BotInvoiceURL   string `json:"bot_invoice_url"`   // основной URL для оплаты
}

type InvoiceStatusResponse struct {
	OK     bool           `json:"ok"`
	Result InvoiceResult  `json:"result"`
}

type InvoiceResult struct {
	InvoiceID int64  `json:"invoice_id"`
	Status    string `json:"status"`
}

// XRocketCreateInvoiceRequest — тело запроса для xRocket /tg-invoices.
// Поля подобраны по публичной документации xRocket Pay API.
type XRocketCreateInvoiceRequest struct {
	Amount      float64 `json:"amount"`                // сумма к оплате
	Coin        string  `json:"coin"`                  // монета, например TON
	Description string  `json:"description,omitempty"` // описание в интерфейсе xRocket
	Payload     string  `json:"payload,omitempty"`     // произвольные данные (ID пользователя)
	NumPayments int     `json:"numPayments,omitempty"` // количество активаций (по умолчанию 1)
}

// XRocketCreateInvoiceResponse — ожидаемый ответ xRocket для /tg-invoices.
// По фактическому ответу: id приходит как строка, ссылка — в поле link.
type XRocketCreateInvoiceResponse struct {
	Data struct {
		ID   string `json:"id"`   // строковый ID инвойса
		URL  string `json:"url"`  // может быть пустым
		Link string `json:"link"` // tg-ссылка на оплату
	} `json:"data"`
}

func (uc *paymentUseCase) CreateInvoice(amount float64, currency string, description string, userID int64) (payURL string, invoiceID int64, err error) {
	// Если в apiURL указан xRocket — используем интеграцию xRocket Pay.
	if strings.Contains(strings.ToLower(uc.apiURL), "xrocket") {
		return uc.createInvoiceXRocket(amount, currency, description, userID)
	}
	// Иначе — старая интеграция CryptoPay (на случай обратной совместимости).
	asset := "TON"
	if currency != "" && currency != "USD" {
		asset = currency
	}
	reqBody := CreateInvoiceRequest{
		Asset:       asset,
		Amount:      fmt.Sprintf("%.2f", amount),
		Description: description,
		Payload:     fmt.Sprintf("%d", userID),
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/createInvoice", uc.apiURL),
		bytes.NewBuffer(jsonData))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Crypto-Pay-API-Token", uc.apiToken)

	resp, err := uc.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read response: %w", err)
	}

	var invoiceResp CreateInvoiceResponse
	if err := json.Unmarshal(body, &invoiceResp); err != nil {
		log.Printf("[payment] CreateInvoice unmarshal error: %v, body: %s", err, string(body))
		return "", 0, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if !invoiceResp.OK {
		log.Printf("[payment] CreateInvoice API error: status=%d, body=%s", resp.StatusCode, string(body))
		return "", 0, fmt.Errorf("cryptopay API error (status %d, check token and request)", resp.StatusCode)
	}

	inv := &domain.Invoice{
		InvoiceID: invoiceResp.Result.InvoiceID,
		UserID:    userID,
		Kind:      "premium",
		DaysGranted: 0,
		Status:    "pending",
		Amount:    amount,
		Currency:  asset,
	}
	if err := uc.invRepo.Create(inv); err != nil {
		return "", 0, fmt.Errorf("failed to save invoice: %w", err)
	}

	payURL = invoiceResp.Result.BotInvoiceURL
	if payURL == "" {
		payURL = invoiceResp.Result.PayURL
	}
	return payURL, invoiceResp.Result.InvoiceID, nil
}

func (uc *paymentUseCase) createInvoiceXRocket(amount float64, currency string, description string, userID int64) (payURL string, invoiceID int64, err error) {
	coin := "TON"
	if currency != "" {
		coin = currency
	}
	bodyReq := XRocketCreateInvoiceRequest{
		Amount:      amount,
		Coin:        coin,
		Description: description,
		Payload:     fmt.Sprintf("%d", userID),
		NumPayments: 1,
	}
	jsonData, err := json.Marshal(bodyReq)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal xRocket request: %w", err)
	}

	url := strings.TrimRight(uc.apiURL, "/") + "/tg-invoices"
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create xRocket request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Согласно документации xRocket, ключ передаётся в заголовке Rocket-Pay-Key.
	req.Header.Set("Rocket-Pay-Key", uc.apiToken)

	resp, err := uc.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to send xRocket request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read xRocket response: %w", err)
	}

	var invoiceResp XRocketCreateInvoiceResponse
	if err := json.Unmarshal(body, &invoiceResp); err != nil {
		log.Printf("[payment] xRocket CreateInvoice unmarshal error: %v, body: %s", err, string(body))
		return "", 0, fmt.Errorf("failed to unmarshal xRocket response: %w", err)
	}
	if invoiceResp.Data.ID == "" {
		log.Printf("[payment] xRocket CreateInvoice: unexpected response, body=%s", string(body))
		return "", 0, fmt.Errorf("xrocket API error: empty invoice id")
	}

	// xRocket возвращает строковый ID; доменная модель хранит int64 invoice_id (исторически для CryptoPay).
	// Для совместимости парсим строку в int64, а в случае ошибки логируем и возвращаем её.
	parsedID, err := strconv.ParseInt(invoiceResp.Data.ID, 10, 64)
	if err != nil {
		log.Printf("[payment] xRocket CreateInvoice: cannot parse invoice id %q: %v, body=%s", invoiceResp.Data.ID, err, string(body))
		return "", 0, fmt.Errorf("xrocket API error: invalid invoice id")
	}

	inv := &domain.Invoice{
		InvoiceID: parsedID,
		UserID:    userID,
		Kind:      "premium",
		DaysGranted: 0,
		Status:    "pending",
		Amount:    amount,
		Currency:  coin,
	}
	if err := uc.invRepo.Create(inv); err != nil {
		return "", 0, fmt.Errorf("failed to save invoice: %w", err)
	}

	payURL = invoiceResp.Data.Link
	if payURL == "" {
		payURL = invoiceResp.Data.URL
	}
	if payURL == "" {
		log.Printf("[payment] xRocket CreateInvoice: no payment URL in response, body=%s", string(body))
		return "", 0, fmt.Errorf("xrocket API error: no payment url")
	}

	return payURL, parsedID, nil
}

func (uc *paymentUseCase) GetUserIDByInvoiceID(invoiceID int64) (int64, bool) {
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil || inv == nil || inv.Status == "paid" {
		return 0, false
	}
	return inv.UserID, true
}

func (uc *paymentUseCase) GetInvoice(invoiceID int64) (*domain.Invoice, error) {
	return uc.invRepo.GetByInvoiceID(invoiceID)
}

func (uc *paymentUseCase) SetInvoiceMeta(invoiceID int64, kind string, daysGranted int) error {
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil || inv == nil {
		return fmt.Errorf("invoice not found")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = "premium"
	}
	inv.Kind = kind
	if daysGranted > 0 {
		inv.DaysGranted = daysGranted
	}
	return uc.invRepo.Update(inv)
}

func (uc *paymentUseCase) SetInvoiceMessage(invoiceID int64, chatID int64, messageID int64) error {
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil || inv == nil {
		return fmt.Errorf("invoice not found")
	}
	inv.ChatID = chatID
	inv.MessageID = messageID
	return uc.invRepo.Update(inv)
}

func (uc *paymentUseCase) GetInvoiceMessageInfo(invoiceID int64) (chatID int64, messageID int64, ok bool) {
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil || inv == nil || inv.ChatID == 0 || inv.MessageID == 0 {
		return 0, 0, false
	}
	return inv.ChatID, inv.MessageID, true
}

func (uc *paymentUseCase) MarkInvoicePaid(invoiceID int64) error {
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil || inv == nil {
		return fmt.Errorf("invoice not found")
	}
	inv.Status = "paid"
	now := time.Now()
	inv.PaidAt = &now
	return uc.invRepo.Update(inv)
}

func (uc *paymentUseCase) CancelInvoice(invoiceID int64) error {
	// Сначала помечаем в БД (если есть), чтобы инвойс не считался актуальным даже при проблемах с API.
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil {
		return err
	}
	if inv != nil && inv.Status != "paid" {
		inv.Status = "cancelled"
		if err := uc.invRepo.Update(inv); err != nil {
			return err
		}
	}

	// Для xRocket есть DELETE /tg-invoices/{id}. Для старого CryptoPay отмены нет — просто оставляем cancelled в БД.
	if !strings.Contains(strings.ToLower(uc.apiURL), "xrocket") && !strings.Contains(strings.ToLower(uc.apiURL), "pay.xrocket") && !strings.Contains(strings.ToLower(uc.apiURL), "xrocket.tg") {
		return nil
	}
	return uc.cancelInvoiceXRocket(invoiceID)
}

func (uc *paymentUseCase) cancelInvoiceXRocket(invoiceID int64) error {
	base := strings.TrimRight(uc.apiURL, "/")
	// В конфиге по умолчанию base = https://pay.xrocket.tg, а эндпоинты начинаются с /tg-invoices
	url := fmt.Sprintf("%s/tg-invoices/%d", base, invoiceID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create xRocket cancel request: %w", err)
	}
	req.Header.Set("Rocket-Pay-Key", uc.apiToken)

	resp, err := uc.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send xRocket cancel request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("xrocket cancel invoice failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func (uc *paymentUseCase) CheckInvoiceStatus(invoiceID string) (bool, error) {
	req, err := http.NewRequest("GET", 
		fmt.Sprintf("%s/getInvoices?invoice_ids=%s", uc.apiURL, invoiceID), nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Crypto-Pay-API-Token", uc.apiToken)

	resp, err := uc.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read response: %w", err)
	}

	var statusResp InvoiceStatusResponse
	if err := json.Unmarshal(body, &statusResp); err != nil {
		return false, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if !statusResp.OK {
		return false, fmt.Errorf("cryptobot API error")
	}

	return statusResp.Result.Status == "paid", nil
}

func (uc *paymentUseCase) RecordStarPayment(tgID int64, amountTotal int64, currency string, daysGranted int, telegramPaymentChargeID string) error {
	if uc.starPaymentRepo == nil {
		return nil
	}
	p := &domain.StarPayment{
		TGID:                    tgID,
		AmountTotal:             amountTotal,
		Currency:                currency,
		DaysGranted:             daysGranted,
		TelegramPaymentChargeID: telegramPaymentChargeID,
	}
	return uc.starPaymentRepo.Create(p)
}
