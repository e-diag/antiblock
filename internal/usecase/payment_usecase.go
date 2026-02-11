package usecase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/yourusername/antiblock/internal/domain"
)

// PaymentUseCase определяет бизнес-логику для работы с платежами
type PaymentUseCase interface {
	CreateInvoice(amount float64, currency string, description string, userID int64) (payURL string, invoiceID int64, err error)
	CheckInvoiceStatus(invoiceID string) (bool, error)
	GetUserIDByInvoiceID(invoiceID int64) (int64, bool)
	MarkInvoicePaid(invoiceID int64) error
}

type paymentUseCase struct {
	apiToken string
	apiURL   string
	client   *http.Client
	invRepo  InvoiceRepository
}

// InvoiceRepository минимальный интерфейс для сохранения счетов
type InvoiceRepository interface {
	Create(inv *domain.Invoice) error
	GetByInvoiceID(invoiceID int64) (*domain.Invoice, error)
	Update(inv *domain.Invoice) error
}

// NewPaymentUseCase создает новый use case для платежей
func NewPaymentUseCase(apiToken, apiURL string, invRepo InvoiceRepository) PaymentUseCase {
	return &paymentUseCase{
		apiToken: apiToken,
		apiURL:   apiURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		invRepo: invRepo,
	}
}

// CreateInvoiceRequest для Crypto Pay API: asset (USDT), amount (строка), description, payload
type CreateInvoiceRequest struct {
	Asset       string `json:"asset"`                  // USDT, TON, BTC, ...
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

func (uc *paymentUseCase) CreateInvoice(amount float64, currency string, description string, userID int64) (payURL string, invoiceID int64, err error) {
	asset := "USDT"
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

func (uc *paymentUseCase) GetUserIDByInvoiceID(invoiceID int64) (int64, bool) {
	inv, err := uc.invRepo.GetByInvoiceID(invoiceID)
	if err != nil || inv == nil || inv.Status == "paid" {
		return 0, false
	}
	return inv.UserID, true
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
