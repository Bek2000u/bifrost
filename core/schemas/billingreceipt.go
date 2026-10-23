package schemas

import "time"

const BillingReceiptVersion = "bifrost.receipt.v1"

const (
	BifrostContextKeyBillingOperation   BifrostContextKey = "bifrost-billing-operation"
	BifrostContextKeyBillingHold        BifrostContextKey = "bifrost-billing-hold"
	BifrostContextKeyBillingReceiptID   BifrostContextKey = "bifrost-billing-receipt-id"
	BifrostContextKeyBillingExecutionID BifrostContextKey = "bifrost-billing-execution-id"
	BifrostContextKeyBillingObserver    BifrostContextKey = "bifrost-billing-observer"
)

// BillingReceiptObserver persists evidence before ordinary post-hooks can enqueue
// lossy logs or settle a provider job. Nil/absent keeps the normal path unchanged.
type BillingReceiptObserver interface {
	ObserveBillingReceipt(*BifrostContext, *BifrostResponse, *BifrostError) (*BifrostResponse, *BifrostError, error)
}

type BillingScope struct {
	AppID    string `json:"app_id"`
	TenantID string `json:"tenant_id"`
}

type BillingMeasure struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Value string `json:"value"`
}

type BillingObservedCost struct {
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Source     string            `json:"source"`
	Precision  string            `json:"precision"`
	Components map[string]string `json:"components,omitempty"`
}

type BillingJobReference struct {
	Kind            string `json:"kind"`
	Provider        string `json:"provider"`
	TaskID          string `json:"task_id"`
	AccountingLogID string `json:"accounting_log_id,omitempty"`
}

type BillingModelUsage struct {
	Model    string               `json:"model"`
	Measures []BillingMeasure     `json:"measures"`
	Cost     *BillingObservedCost `json:"cost"`
}

// BillingReceipt is immutable evidence, not a customer charge. Missing measures
// and nil cost remain unknown. Submeasures can already be included in totals.
type BillingReceipt struct {
	Version           string               `json:"version"`
	GatewayID         string               `json:"gateway_id"`
	EventID           string               `json:"event_id"`
	SupersedesEventID string               `json:"supersedes_event_id,omitempty"`
	ReceiptID         string               `json:"receipt_id"`
	ExecutionID       string               `json:"execution_id"`
	Scope             BillingScope         `json:"scope"`
	OperationKey      string               `json:"operation_key"`
	HoldID            string               `json:"hold_id"`
	RequestID         string               `json:"request_id,omitempty"`
	AttemptID         string               `json:"attempt_id"`
	Phase             string               `json:"phase"`
	Status            string               `json:"status"`
	Completeness      string               `json:"completeness"`
	Measures          []BillingMeasure     `json:"measures"`
	ModelUsage        []BillingModelUsage  `json:"model_usage,omitempty"`
	Dimensions        map[string]string    `json:"dimensions"`
	Cost              *BillingObservedCost `json:"cost"`
	Job               *BillingJobReference `json:"job,omitempty"`
	OccurredAt        time.Time            `json:"occurred_at"`
}

type BillingDelivery struct {
	PayloadJSON    string         `json:"payload_json"`
	Receipt        BillingReceipt `json:"receipt"`
	PayloadSHA256  string         `json:"payload_sha256"`
	ClaimToken     string         `json:"claim_token,omitempty"`
	LeaseUntil     *time.Time     `json:"lease_until,omitempty"`
	Attempts       int            `json:"attempts"`
	AcknowledgedAt *time.Time     `json:"acknowledged_at,omitempty"`
}
