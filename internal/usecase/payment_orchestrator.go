package usecase

import (
	"fmt"
	"strings"
)

type PaymentProcessStatus string

const (
	PaymentProcessed        PaymentProcessStatus = "processed"
	PaymentAlreadyProcessed PaymentProcessStatus = "already_processed"
	PaymentValidationFailed PaymentProcessStatus = "validation_failed"
)

type PaymentEventInput struct {
	Provider   string
	ExternalID string
	TGID       int64
	Tariff     string
	Days       int
	AmountRub  int
	Currency   string
}

type PaymentProcessResult struct {
	Status PaymentProcessStatus
}

type PaymentOrchestrator struct {
	paymentUC       PaymentUseCase
	activatePremium func(tgID int64, days int) error
	activatePro     func(tgID int64, days int) error
	recordPayment   func(in PaymentEventInput) error
	finalize        func(in PaymentEventInput) error
}

func NewPaymentOrchestrator(
	paymentUC PaymentUseCase,
	activatePremium func(tgID int64, days int) error,
	activatePro func(tgID int64, days int) error,
	recordPayment func(in PaymentEventInput) error,
	finalize func(in PaymentEventInput) error,
) *PaymentOrchestrator {
	return &PaymentOrchestrator{
		paymentUC:       paymentUC,
		activatePremium: activatePremium,
		activatePro:     activatePro,
		recordPayment:   recordPayment,
		finalize:        finalize,
	}
}

func (o *PaymentOrchestrator) ProcessPaidEvent(in PaymentEventInput) (*PaymentProcessResult, error) {
	if o == nil || o.paymentUC == nil {
		return nil, fmt.Errorf("payment orchestrator is not configured")
	}
	if in.ExternalID == "" || in.TGID <= 0 || in.Days <= 0 {
		return &PaymentProcessResult{Status: PaymentValidationFailed}, nil
	}
	tariff := strings.ToLower(strings.TrimSpace(in.Tariff))
	if tariff != "premium" && tariff != "pro" {
		return &PaymentProcessResult{Status: PaymentValidationFailed}, nil
	}

	started, err := o.paymentUC.TryStartPaymentEvent(in.Provider, in.ExternalID)
	if err != nil {
		return nil, err
	}
	if !started {
		return &PaymentProcessResult{Status: PaymentAlreadyProcessed}, nil
	}
	processingDone := false
	defer func() {
		if !processingDone {
			_ = o.paymentUC.MarkPaymentEventFailed(in.Provider, in.ExternalID)
		}
	}()

	if o.recordPayment != nil {
		if err := o.recordPayment(in); err != nil {
			return nil, err
		}
	}

	if tariff == "premium" {
		if o.activatePremium == nil {
			return nil, fmt.Errorf("premium activator is nil")
		}
		if err := o.activatePremium(in.TGID, in.Days); err != nil {
			return nil, err
		}
	} else {
		if o.activatePro == nil {
			return nil, fmt.Errorf("pro activator is nil")
		}
		if err := o.activatePro(in.TGID, in.Days); err != nil {
			return nil, err
		}
	}

	if o.finalize != nil {
		if err := o.finalize(in); err != nil {
			return nil, err
		}
	}
	if err := o.paymentUC.MarkPaymentEventSucceeded(in.Provider, in.ExternalID); err != nil {
		return nil, err
	}
	processingDone = true
	return &PaymentProcessResult{Status: PaymentProcessed}, nil
}

