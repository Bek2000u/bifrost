package tables

import "time"

type TableBillingGateway struct {
	ID        string `gorm:"primaryKey;type:varchar(50)"`
	GatewayID string `gorm:"type:varchar(50);not null"`
}

func (TableBillingGateway) TableName() string { return "billing_gateway" }

// TableBillingExecution keeps the pre-submit binding and last observed evidence
// outside the lossy log writer. No prompt, output, credentials or media is stored.
type TableBillingExecution struct {
	ID                 string `gorm:"primaryKey;type:varchar(50)"`
	ReceiptID          string `gorm:"type:varchar(50);not null;uniqueIndex"`
	AppID              string `gorm:"type:varchar(255);not null;index:idx_billing_scope_operation,priority:1"`
	TenantID           string `gorm:"type:varchar(255);not null;index:idx_billing_scope_operation,priority:2"`
	OperationKey       string `gorm:"type:varchar(255);not null;index:idx_billing_scope_operation,priority:3"`
	HoldID             string `gorm:"type:varchar(255);not null"`
	EventID            string `gorm:"type:varchar(50);not null"`
	SettlementEventID  string `gorm:"type:varchar(50);not null"`
	JobReference       string `gorm:"type:varchar(512);index"`
	EvidenceJSON       string `gorm:"type:text;not null"`
	InitialRecorded    bool   `gorm:"not null;default:false"`
	SettlementRecorded bool   `gorm:"not null;default:false"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (TableBillingExecution) TableName() string { return "billing_executions" }

type TableBillingEvent struct {
	ID             string     `gorm:"primaryKey;type:varchar(50)"`
	AppID          string     `gorm:"type:varchar(255);not null;index:idx_billing_delivery,priority:1"`
	TenantID       string     `gorm:"type:varchar(255);not null;index:idx_billing_delivery,priority:2"`
	Payload        string     `gorm:"type:text;not null"`
	PayloadSHA256  string     `gorm:"type:varchar(64);not null"`
	ClaimToken     string     `gorm:"type:varchar(50)"`
	LeaseUntil     *time.Time `gorm:"index:idx_billing_delivery,priority:4"`
	Attempts       int        `gorm:"not null;default:0"`
	AcknowledgedAt *time.Time `gorm:"index:idx_billing_delivery,priority:3"`
	CreatedAt      time.Time
}

func (TableBillingEvent) TableName() string { return "billing_events" }
