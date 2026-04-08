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
	// RecordYooKassaPayment сохраняет запись об успешной оплате через ЮKassa.
	RecordYooKassaPayment(
		tgID int64,
		tariffType string,
		amountRub int,
		daysGranted int,
		telegramChargeID string,
		providerChargeID string,
	) error
	HasYooKassaPayment(providerChargeID string) (bool, error)
	TryStartPaymentEvent(provider, externalID string) (bool, error)
	MarkPaymentEventSucceeded(provider, externalID string) error
	MarkPaymentEventFailed(provider, externalID string) error
	// CreateYooKassaInvoice сохраняет pending запись для Smart Payment (для clean-up висящих платежей).
	CreateYooKassaInvoice(inv *domain.YooKassaInvoice) error
	GetYooKassaInvoice(paymentID string) (*domain.YooKassaInvoice, error)
	MarkYooKassaInvoicePaid(paymentID string) error
	ListPendingYooKassaInvoicesOlderThan(cutoff time.Time) ([]*domain.YooKassaInvoice, error)
	DeleteYooKassaInvoice(paymentID string) error
	CancelYooKassaPayment(paymentID string) error
}

type paymentUseCase struct {
	apiToken              string
	apiURL                string
	client                *http.Client
	invRepo               InvoiceRepository
	starPaymentRepo       StarPaymentRepository
	yooKassaPaymentRepo   YooKassaPaymentRepository
	yooKassaInvoiceRepo   YooKassaInvoiceRepository
	paymentEventRepo      PaymentEventRepository
	yooKassaShopID        string
	yooKassaSecretKey     string
}

// InvoiceRepository минимальный интерфейс для сохранения счетов
type InvoiceRepository interface {
	Create(inv *domain.Invoice) error
	GetByInvoiceID(invoiceID int64) (*domain.Invoice, error)
	Update(inv *domain.Invoice) error
	ListPendingOlderThan(cutoff time.Time) ([]*domain.Invoice, error)
	ListPending() ([]*domain.Invoice, error)
	DeleteByInvoiceID(invoiceID int64) error
}

// StarPaymentRepository — сохранение оплат Telegram Stars
type StarPaymentRepository interface {
	Create(p *domain.StarPayment) error
}

// YooKassaPaymentRepository — сохранение оплат ЮKassa (RUB через Telegram Payments).
type YooKassaPaymentRepository interface {
	Create(p *domain.YooKassaPayment) error
	ExistsByProviderPaymentChargeID(providerPaymentChargeID string) (bool, error)
}

type YooKassaInvoiceRepository interface {
	Create(inv *domain.YooKassaInvoice) error
	GetByPaymentID(paymentID string) (*domain.YooKassaInvoice, error)
	ListPendingOlderThan(cutoff time.Time) ([]*domain.YooKassaInvoice, error)
	MarkPaid(paymentID string) error
	MarkCancelled(paymentID string) error
	DeleteByPaymentID(paymentID string) error
}

type PaymentEventRepository interface {
	TryStart(provider, externalID string) (started bool, err error)
	MarkSucceeded(provider, externalID string) error
	MarkFailed(provider, externalID string) error
}

// NewPaymentUseCase создает новый use case для платежей
func NewPaymentUseCase(
	apiToken, apiURL string,
	invRepo InvoiceRepository,
	starPaymentRepo StarPaymentRepository,
	yooKassaPaymentRepo YooKassaPaymentRepository,
	yooKassaInvoiceRepo YooKassaInvoiceRepository,
	paymentEventRepo PaymentEventRepository,
	yooKassaShopID string,
	yooKassaSecretKey string,
) PaymentUseCase {
	return &paymentUseCase{
		apiToken:              apiToken,
		apiURL:                apiURL,
		client:                &http.Client{Timeout: 30 * time.Second},
		invRepo:               invRepo,
		starPaymentRepo:       starPaymentRepo,
		yooKassaPaymentRepo:   yooKassaPaymentRepo,
		yooKassaInvoiceRepo:   yooKassaInvoiceRepo,
		paymentEventRepo:      paymentEventRepo,
		yooKassaShopID:        yooKassaShopID,
		yooKassaSecretKey:     yooKassaSecretKey,
	}
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
	// В текущей сборке оплата работает через xRocket (остальные провайдеры отключены).
	return uc.createInvoiceXRocket(amount, currency, description, userID)
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

	// Для xRocket есть DELETE /tg-invoices/{id}.
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
	err := uc.starPaymentRepo.Create(p)
	if isDuplicateKeyError(err) {
		return nil
	}
	return err
}

func (uc *paymentUseCase) RecordYooKassaPayment(
	tgID int64,
	tariffType string,
	amountRub int,
	daysGranted int,
	telegramChargeID string,
	providerChargeID string,
) error {
	if uc.yooKassaPaymentRepo == nil {
		return nil
	}
	p := &domain.YooKassaPayment{
		TGID:                      tgID,
		TariffType:                tariffType,
		AmountRub:                 amountRub,
		DaysGranted:               daysGranted,
		TelegramPaymentChargeID:   telegramChargeID,
		ProviderPaymentChargeID:   providerChargeID,
	}
	err := uc.yooKassaPaymentRepo.Create(p)
	if isDuplicateKeyError(err) {
		return nil
	}
	return err
}

func (uc *paymentUseCase) HasYooKassaPayment(providerChargeID string) (bool, error) {
	if uc.yooKassaPaymentRepo == nil {
		return false, nil
	}
	return uc.yooKassaPaymentRepo.ExistsByProviderPaymentChargeID(providerChargeID)
}

func (uc *paymentUseCase) TryStartPaymentEvent(provider, externalID string) (bool, error) {
	if uc.paymentEventRepo == nil {
		return true, nil
	}
	return uc.paymentEventRepo.TryStart(provider, externalID)
}

func (uc *paymentUseCase) MarkPaymentEventSucceeded(provider, externalID string) error {
	if uc.paymentEventRepo == nil {
		return nil
	}
	return uc.paymentEventRepo.MarkSucceeded(provider, externalID)
}

func (uc *paymentUseCase) MarkPaymentEventFailed(provider, externalID string) error {
	if uc.paymentEventRepo == nil {
		return nil
	}
	return uc.paymentEventRepo.MarkFailed(provider, externalID)
}

func (uc *paymentUseCase) CreateYooKassaInvoice(inv *domain.YooKassaInvoice) error {
	if uc.yooKassaInvoiceRepo == nil || inv == nil {
		return nil
	}
	return uc.yooKassaInvoiceRepo.Create(inv)
}

func (uc *paymentUseCase) GetYooKassaInvoice(paymentID string) (*domain.YooKassaInvoice, error) {
	if uc.yooKassaInvoiceRepo == nil || strings.TrimSpace(paymentID) == "" {
		return nil, nil
	}
	return uc.yooKassaInvoiceRepo.GetByPaymentID(paymentID)
}

func (uc *paymentUseCase) MarkYooKassaInvoicePaid(paymentID string) error {
	if uc.yooKassaInvoiceRepo == nil {
		return nil
	}
	return uc.yooKassaInvoiceRepo.MarkPaid(paymentID)
}

func (uc *paymentUseCase) ListPendingYooKassaInvoicesOlderThan(cutoff time.Time) ([]*domain.YooKassaInvoice, error) {
	if uc.yooKassaInvoiceRepo == nil {
		return nil, nil
	}
	return uc.yooKassaInvoiceRepo.ListPendingOlderThan(cutoff)
}

func (uc *paymentUseCase) DeleteYooKassaInvoice(paymentID string) error {
	if uc.yooKassaInvoiceRepo == nil {
		return nil
	}
	return uc.yooKassaInvoiceRepo.DeleteByPaymentID(paymentID)
}

func (uc *paymentUseCase) CancelYooKassaPayment(paymentID string) error {
	if uc.yooKassaShopID == "" || uc.yooKassaSecretKey == "" || paymentID == "" {
		return nil
	}
	url := fmt.Sprintf("https://api.yookassa.ru/v3/payments/%s/cancel", paymentID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(uc.yooKassaShopID, uc.yooKassaSecretKey)
	req.Header.Set("Idempotence-Key", fmt.Sprintf("cancel-%s-%d", paymentID, time.Now().UnixNano()))
	resp, err := uc.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("yookassa cancel failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
