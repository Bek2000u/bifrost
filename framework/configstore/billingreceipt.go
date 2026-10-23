package configstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrBillingReceiptConflict = errors.New("billing receipt identity or payload conflict")

func billingScope(db *gorm.DB, scope schemas.BillingScope) *gorm.DB {
	return db.Where("app_id = ? AND tenant_id = ?", scope.AppID, scope.TenantID)
}

func (s *RDBConfigStore) BillingGatewayID(ctx context.Context) (string, error) {
	row := tables.TableBillingGateway{ID: "default", GatewayID: uuid.NewString()}
	db := s.DB().WithContext(ctx)
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return "", err
	}
	if err := db.First(&row, "id = ?", "default").Error; err != nil {
		return "", err
	}
	return row.GatewayID, nil
}

func (s *RDBConfigStore) BeginBillingExecution(ctx context.Context, receipt *schemas.BillingReceipt) error {
	if receipt == nil || receipt.Scope.AppID == "" || receipt.Scope.TenantID == "" || receipt.OperationKey == "" || receipt.HoldID == "" || receipt.ExecutionID == "" || receipt.EventID == "" {
		return fmt.Errorf("billing scope, operation, hold and server identities are required")
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	row := tables.TableBillingExecution{ID: receipt.ExecutionID, ReceiptID: receipt.ReceiptID, AppID: receipt.Scope.AppID, TenantID: receipt.Scope.TenantID, OperationKey: receipt.OperationKey, HoldID: receipt.HoldID, EventID: receipt.EventID, SettlementEventID: uuid.NewString(), EvidenceJSON: string(data)}
	return s.DB().WithContext(ctx).Create(&row).Error
}

// RecordBillingSnapshot persists only observed usage, not content. A terminal
// writer always wins over a late stream snapshot.
func (s *RDBConfigStore) RecordBillingSnapshot(ctx context.Context, receipt *schemas.BillingReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return billingScope(s.DB().WithContext(ctx).Model(&tables.TableBillingExecution{}), receipt.Scope).
		Where("id = ? AND initial_recorded = ?", receipt.ExecutionID, false).
		Update("evidence_json", string(data)).Error
}

// FinalizeBillingReceipt atomically stores an immutable event and its durable
// outbox row. Repeated terminal writers return the first committed evidence.
func (s *RDBConfigStore) FinalizeBillingReceipt(ctx context.Context, receipt *schemas.BillingReceipt) (*schemas.BillingReceipt, error) {
	var stored schemas.BillingReceipt
	err := s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var execution tables.TableBillingExecution
		if err := billingScope(tx, receipt.Scope).First(&execution, "id = ?", receipt.ExecutionID).Error; err != nil {
			return err
		}
		if execution.OperationKey != receipt.OperationKey || execution.HoldID != receipt.HoldID {
			return ErrBillingReceiptConflict
		}
		flag, eventID := "initial_recorded", execution.EventID
		if receipt.Phase == "settlement" {
			flag, eventID = "settlement_recorded", execution.SettlementEventID
		}
		r := *receipt
		r.EventID = eventID
		data, err := json.Marshal(&r)
		if err != nil {
			return err
		}
		updates := map[string]any{flag: true, "evidence_json": string(data), "updated_at": time.Now().UTC()}
		if r.Job != nil {
			updates["job_reference"] = tables.ProviderJobID(r.Job.Kind, r.Job.Provider, r.Job.TaskID)
		}
		result := tx.Model(&tables.TableBillingExecution{}).Where("id = ? AND "+flag+" = ?", execution.ID, false).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var event tables.TableBillingEvent
			if err := tx.First(&event, "id = ?", eventID).Error; err != nil {
				return err
			}
			return json.Unmarshal([]byte(event.Payload), &stored)
		}
		digest := sha256.Sum256(data)
		event := tables.TableBillingEvent{ID: eventID, AppID: r.Scope.AppID, TenantID: r.Scope.TenantID, Payload: string(data), PayloadSHA256: hex.EncodeToString(digest[:]), CreatedAt: time.Now().UTC()}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		stored = r
		return nil
	})
	return &stored, err
}

func (s *RDBConfigStore) GetBillingExecution(ctx context.Context, scope schemas.BillingScope, id string) (*schemas.BillingReceipt, error) {
	var row tables.TableBillingExecution
	if err := billingScope(s.DB().WithContext(ctx), scope).Where("(id = ? OR receipt_id = ?)", id, id).First(&row).Error; err != nil {
		return nil, err
	}
	var receipt schemas.BillingReceipt
	if err := json.Unmarshal([]byte(row.EvidenceJSON), &receipt); err != nil {
		return nil, err
	}
	if !row.InitialRecorded {
		receipt.EventID = ""
	}
	return &receipt, nil
}

func (s *RDBConfigStore) ListBillingExecutions(ctx context.Context, scope schemas.BillingScope, operation string, limit int) ([]schemas.BillingReceipt, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	db := billingScope(s.DB().WithContext(ctx), scope)
	if operation != "" {
		db = db.Where("operation_key = ?", operation)
	}
	var rows []tables.TableBillingExecution
	if err := db.Order("created_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]schemas.BillingReceipt, 0, len(rows))
	for _, row := range rows {
		var r schemas.BillingReceipt
		if err := json.Unmarshal([]byte(row.EvidenceJSON), &r); err != nil {
			return nil, err
		}
		if !row.InitialRecorded {
			r.EventID = ""
		}
		result = append(result, r)
	}
	return result, nil
}

func billingDelivery(row tables.TableBillingEvent) (schemas.BillingDelivery, error) {
	var receipt schemas.BillingReceipt
	err := json.Unmarshal([]byte(row.Payload), &receipt)
	return schemas.BillingDelivery{PayloadJSON: row.Payload, Receipt: receipt, PayloadSHA256: row.PayloadSHA256, ClaimToken: row.ClaimToken, LeaseUntil: row.LeaseUntil, Attempts: row.Attempts, AcknowledgedAt: row.AcknowledgedAt}, err
}

// RecordBillingJobSettlement binds the native jobaccounting result back to the
// original scope/hold. A retrieve request can never replace that binding.
func (s *RDBConfigStore) RecordBillingJobSettlement(ctx context.Context, jobReference, accountingLogID, runnerID string, evidence *schemas.BillingReceipt) error {
	if runnerID == "" {
		return ErrBillingReceiptConflict
	}
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job tables.TableProviderJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND runner_id = ? AND accounting_status = ?", jobReference, runnerID, tables.ProviderJobAccountingStatusProcessing).First(&job).Error; err != nil {
			return err
		}
		if job.BillingExecutionID == "" {
			return nil
		}
		var count int64
		if err := tx.Model(&tables.TableBillingExecution{}).Where("id = ? AND job_reference = ?", job.BillingExecutionID, jobReference).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return ErrBillingReceiptConflict
		}
		bound := &RDBConfigStore{}
		bound.db.Store(tx)
		return bound.recordBillingJobSettlement(ctx, jobReference, accountingLogID, job.BillingExecutionID, evidence)
	})
}

func (s *RDBConfigStore) recordBillingJobSettlement(ctx context.Context, jobReference, accountingLogID, executionID string, evidence *schemas.BillingReceipt) error {
	var rows []tables.TableBillingExecution
	if err := s.DB().WithContext(ctx).Where("job_reference = ? AND id = ?", jobReference, executionID).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var receipt schemas.BillingReceipt
		if err := json.Unmarshal([]byte(row.EvidenceJSON), &receipt); err != nil {
			return err
		}
		previous := receipt
		receipt.Phase = "settlement"
		receipt.Status = evidence.Status
		receipt.Completeness = evidence.Completeness
		receipt.Measures = evidence.Measures
		receipt.ModelUsage = evidence.ModelUsage
		receipt.Cost = evidence.Cost
		receipt.OccurredAt = time.Now().UTC()
		if receipt.Job == nil {
			return ErrBillingReceiptConflict
		}
		receipt.Job.AccountingLogID = accountingLogID
		for key, value := range evidence.Dimensions {
			receipt.Dimensions[key] = value
		}
		if row.SettlementRecorded {
			oldCost, _ := json.Marshal(previous.Cost)
			newCost, _ := json.Marshal(evidence.Cost)
			newBill := evidence.Cost != nil && evidence.Cost.Source == "provider_reported" && string(oldCost) != string(newCost)
			newMeasures := evidence.Completeness == "complete" && billingMeasuresFingerprint(&previous) != billingMeasuresFingerprint(evidence)
			if !newBill && !newMeasures {
				continue
			}
			receipt.SupersedesEventID = previous.EventID
			if err := s.correctBillingJobReceipt(ctx, row, &receipt); err != nil {
				return err
			}
			continue
		}
		if _, err := s.FinalizeBillingReceipt(ctx, &receipt); err != nil {
			return err
		}
	}
	return nil
}

func billingMeasuresFingerprint(receipt *schemas.BillingReceipt) string {
	type modelMeasures struct {
		Model    string
		Measures []schemas.BillingMeasure
	}
	models := make([]modelMeasures, 0, len(receipt.ModelUsage))
	for _, item := range receipt.ModelUsage {
		models = append(models, modelMeasures{item.Model, item.Measures})
	}
	data, _ := json.Marshal(struct {
		Measures []schemas.BillingMeasure
		Models   []modelMeasures
	}{receipt.Measures, models})
	return string(data)
}

// Corrections are new immutable events. Only newly observed provider bills or
// recovered measures enter here; a catalog-price-only re-drive cannot rewrite
// an already emitted event or silently change the customer's previous capture.
func (s *RDBConfigStore) correctBillingJobReceipt(ctx context.Context, previous tables.TableBillingExecution, receipt *schemas.BillingReceipt) error {
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		receipt.EventID = uuid.NewString()
		payload, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		result := billingScope(tx.Model(&tables.TableBillingExecution{}), receipt.Scope).
			Where("id = ? AND settlement_event_id = ? AND settlement_recorded = ?", previous.ID, previous.SettlementEventID, true).
			Updates(map[string]any{"settlement_event_id": receipt.EventID, "evidence_json": string(payload), "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		digest := sha256.Sum256(payload)
		return tx.Create(&tables.TableBillingEvent{ID: receipt.EventID, AppID: receipt.Scope.AppID, TenantID: receipt.Scope.TenantID, Payload: string(payload), PayloadSHA256: hex.EncodeToString(digest[:]), CreatedAt: time.Now().UTC()}).Error
	})
}

func (s *RDBConfigStore) GetBillingEvent(ctx context.Context, scope schemas.BillingScope, id string) (*schemas.BillingDelivery, error) {
	var row tables.TableBillingEvent
	if err := billingScope(s.DB().WithContext(ctx), scope).First(&row, "id = ?", id).Error; err != nil {
		return nil, err
	}
	d, err := billingDelivery(row)
	return &d, err
}

// ClaimBillingEvents uses a per-row compare-and-swap lease, including on SQLite.
// A lost ACK or process restarts redelivers the exact persisted event and digest.
func (s *RDBConfigStore) ClaimBillingEvents(ctx context.Context, scope schemas.BillingScope, now time.Time, lease time.Duration, limit int) ([]schemas.BillingDelivery, error) {
	if lease < time.Second || lease > 5*time.Minute {
		return nil, fmt.Errorf("billing lease must be between one second and five minutes")
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	db := s.DB().WithContext(ctx)
	var rows []tables.TableBillingEvent
	due := "acknowledged_at IS NULL AND (lease_until IS NULL OR lease_until <= ?)"
	if err := billingScope(db, scope).Where(due, now).Order("created_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	deliveries := make([]schemas.BillingDelivery, 0, len(rows))
	for _, row := range rows {
		token, until := uuid.NewString(), now.Add(lease)
		result := billingScope(db.Model(&tables.TableBillingEvent{}), scope).Where("id = ?", row.ID).Where(due, now).
			Updates(map[string]any{"claim_token": token, "lease_until": until, "attempts": gorm.Expr("attempts + 1")})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		row.ClaimToken = token
		row.LeaseUntil = &until
		row.Attempts++
		d, err := billingDelivery(row)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, nil
}

func (s *RDBConfigStore) AckBillingEvent(ctx context.Context, scope schemas.BillingScope, id, digest, token string) error {
	if token == "" || len(digest) != 64 {
		return ErrBillingReceiptConflict
	}
	db := s.DB().WithContext(ctx)
	var row tables.TableBillingEvent
	if err := billingScope(db, scope).First(&row, "id = ?", id).Error; err != nil {
		return err
	}
	if row.PayloadSHA256 != digest {
		return ErrBillingReceiptConflict
	}
	if row.AcknowledgedAt != nil {
		return nil
	}
	result := billingScope(db.Model(&tables.TableBillingEvent{}), scope).Where("id = ? AND payload_sha256 = ? AND claim_token = ? AND acknowledged_at IS NULL", id, digest, token).
		Update("acknowledged_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		var acknowledged tables.TableBillingEvent
		if err := billingScope(db, scope).First(&acknowledged, "id = ?", id).Error; err == nil && acknowledged.PayloadSHA256 == digest && acknowledged.AcknowledgedAt != nil {
			return nil
		}
		return ErrBillingReceiptConflict
	}
	return nil
}
