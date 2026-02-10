package usecase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PaymentUseCase определяет бизнес-логику для работы с платежами
type PaymentUseCase interface {
	CreateInvoice(amount float64, currency string, description string, userID int64) (string, error)
	CheckInvoiceStatus(invoiceID string) (bool, error)
}

type paymentUseCase struct {
	apiToken string
	apiURL   string
	client   *http.Client
}

// NewPaymentUseCase создает новый use case для платежей
func NewPaymentUseCase(apiToken, apiURL string) PaymentUseCase {
	return &paymentUseCase{
		apiToken: apiToken,
		apiURL:   apiURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type CreateInvoiceRequest struct {
	Amount      float64 `json:"amount"`
	Currency    string  `json:"currency"`
	Description string  `json:"description"`
	UserID      int64   `json:"paid_btn_name,omitempty"`
}

type CreateInvoiceResponse struct {
	OK     bool   `json:"ok"`
	Result Result `json:"result"`
}

type Result struct {
	InvoiceID int64  `json:"invoice_id"`
	Status    string `json:"status"`
	PayURL    string `json:"pay_url"`
}

type InvoiceStatusResponse struct {
	OK     bool   `json:"ok"`
	Result Invoice `json:"result"`
}

type Invoice struct {
	InvoiceID int64  `json:"invoice_id"`
	Status    string `json:"status"`
}

func (uc *paymentUseCase) CreateInvoice(amount float64, currency string, description string, userID int64) (string, error) {
	reqBody := CreateInvoiceRequest{
		Amount:      amount,
		Currency:    currency,
		Description: description,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/createInvoice", uc.apiURL), 
		bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Crypto-Pay-API-Token", uc.apiToken)

	resp, err := uc.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	var invoiceResp CreateInvoiceResponse
	if err := json.Unmarshal(body, &invoiceResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if !invoiceResp.OK {
		return "", fmt.Errorf("cryptobot API error")
	}

	return invoiceResp.Result.PayURL, nil
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
