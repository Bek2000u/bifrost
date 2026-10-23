package configstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func billingTestStore(t *testing.T, path string) *RDBConfigStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&tables.TableBillingGateway{}, &tables.TableBillingExecution{}, &tables.TableBillingEvent{}))
	require.NoError(t, db.AutoMigrate(&tables.TableProviderJob{}))
	s := &RDBConfigStore{}
	s.db.Store(db)
	return s
}

func billingTestReceipt(t *testing.T, s *RDBConfigStore) *schemas.BillingReceipt {
	t.Helper()
	id, err := s.BillingGatewayID(context.Background())
	require.NoError(t, err)
	r := &schemas.BillingReceipt{Version: schemas.BillingReceiptVersion, GatewayID: id, EventID: uuid.NewString(), ReceiptID: uuid.NewString(), ExecutionID: uuid.NewString(), AttemptID: uuid.NewString(), Scope: schemas.BillingScope{AppID: "app", TenantID: "tenant"}, OperationKey: "operation", HoldID: "hold", RequestID: "same-client-request-id", Phase: "execution", Status: "pending", Completeness: "unknown", Measures: []schemas.BillingMeasure{}, Dimensions: map[string]string{}, OccurredAt: time.Now().UTC()}
	require.NoError(t, s.BeginBillingExecution(context.Background(), r))
	return r
}

func TestBillingOutboxRestartLostAckAndScope(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.Abs("../../tmp/billing-receipts")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0700))
	path := filepath.Join(root, "receipt-"+uuid.NewString()+".sqlite")
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suffix)
		}
	})
	s := billingTestStore(t, path)
	r := billingTestReceipt(t, s)
	// Before completion/restart the original hold remains available, without inventing cost.
	pending, err := s.GetBillingExecution(ctx, r.Scope, r.ExecutionID)
	require.NoError(t, err)
	require.Nil(t, pending.Cost)
	require.Equal(t, "pending", pending.Status)
	require.Empty(t, pending.EventID, "unfinished snapshot must not masquerade as the immutable future event")
	r.Status = "completed"
	_, err = s.FinalizeBillingReceipt(ctx, r)
	require.NoError(t, err)
	now := time.Now().UTC()
	first, err := s.ClaimBillingEvents(ctx, r.Scope, now, time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, first, 1)
	digest := sha256.Sum256([]byte(first[0].PayloadJSON))
	require.Equal(t, hex.EncodeToString(digest[:]), first[0].PayloadSHA256)
	// A second store instance sees the same gateway/event and respects the existing lease.
	restarted := billingTestStore(t, path)
	gatewayID, err := restarted.BillingGatewayID(ctx)
	require.NoError(t, err)
	require.Equal(t, r.GatewayID, gatewayID)
	leased, err := restarted.ClaimBillingEvents(ctx, r.Scope, now.Add(time.Second), time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, leased)
	replay, err := restarted.ClaimBillingEvents(ctx, r.Scope, now.Add(2*time.Minute), time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, replay, 1)
	require.Equal(t, first[0].PayloadJSON, replay[0].PayloadJSON)
	require.Equal(t, r.EventID, replay[0].Receipt.EventID)
	require.Error(t, restarted.AckBillingEvent(ctx, r.Scope, r.EventID, replay[0].PayloadSHA256, first[0].ClaimToken))
	foreign := schemas.BillingScope{AppID: "app", TenantID: "other"}
	_, err = restarted.GetBillingEvent(ctx, foreign, r.EventID)
	require.Error(t, err)
	require.Error(t, restarted.AckBillingEvent(ctx, foreign, r.EventID, replay[0].PayloadSHA256, replay[0].ClaimToken))
	require.Error(t, restarted.AckBillingEvent(ctx, r.Scope, r.EventID, "wrong-digest", replay[0].ClaimToken))
	require.NoError(t, restarted.AckBillingEvent(ctx, r.Scope, r.EventID, replay[0].PayloadSHA256, replay[0].ClaimToken))
	// A lost ACK response is an idempotent ACK, not another financial event.
	require.NoError(t, restarted.AckBillingEvent(ctx, r.Scope, r.EventID, replay[0].PayloadSHA256, replay[0].ClaimToken))
	remaining, err := s.ClaimBillingEvents(ctx, r.Scope, now.Add(5*time.Minute), time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, remaining)
}

func TestBillingTerminalRaceAndTwoWorkers(t *testing.T) {
	s := billingTestStore(t, ":memory:")
	ctx := context.Background()
	r := billingTestReceipt(t, s)
	r.Status = "completed"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.FinalizeBillingReceipt(ctx, r); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	deliveries := make(chan []schemas.BillingDelivery, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := s.ClaimBillingEvents(ctx, r.Scope, time.Now().UTC(), time.Minute, 10)
			if err != nil {
				t.Error(err)
			}
			deliveries <- rows
		}()
	}
	wg.Wait()
	close(deliveries)
	count := 0
	for rows := range deliveries {
		count += len(rows)
	}
	require.Equal(t, 1, count)
	// The same caller ID is deliberately not unique.
	r2 := billingTestReceipt(t, s)
	require.NotEqual(t, r.ExecutionID, r2.ExecutionID)
	rows, err := s.ListBillingExecutions(ctx, r.Scope, "operation", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestBillingJobSettlementKeepsOriginalScopeAndImmutableEvent(t *testing.T) {
	s := billingTestStore(t, ":memory:")
	ctx := context.Background()
	r := billingTestReceipt(t, s)
	r.Job = &schemas.BillingJobReference{Kind: "video", Provider: "fal", TaskID: "provider-task"}
	_, err := s.FinalizeBillingReceipt(ctx, r)
	require.NoError(t, err)
	ref := tables.ProviderJobID("video", "fal", "provider-task")
	require.NoError(t, s.DB().Create(&tables.TableProviderJob{ID: ref, Kind: "video", Provider: "fal", JobID: "provider-task", BillingExecutionID: r.ExecutionID, RunnerID: schemas.Ptr("runner"), AccountingStatus: tables.ProviderJobAccountingStatusProcessing}).Error)
	evidence := &schemas.BillingReceipt{Status: "completed", Completeness: "complete", Measures: []schemas.BillingMeasure{{Name: "video_seconds", Unit: "second", Value: "6"}}, Dimensions: map[string]string{"model": "actual-model"}}
	require.NoError(t, s.RecordBillingJobSettlement(ctx, ref, "aggregate-log", "runner", evidence))
	rows, err := s.ClaimBillingEvents(ctx, r.Scope, time.Now().UTC(), time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	var final schemas.BillingDelivery
	for _, row := range rows {
		if row.Receipt.Phase == "settlement" {
			final = row
		}
	}
	require.Equal(t, r.Scope, final.Receipt.Scope)
	require.Equal(t, r.HoldID, final.Receipt.HoldID)
	require.Nil(t, final.Receipt.Cost)
	require.Equal(t, "aggregate-log", final.Receipt.Job.AccountingLogID)
	evidence.Cost = &schemas.BillingObservedCost{Amount: "9", Currency: "USD"}
	require.NoError(t, s.RecordBillingJobSettlement(ctx, ref, "aggregate-log", "runner", evidence))
	replayed, err := s.GetBillingEvent(ctx, r.Scope, final.Receipt.EventID)
	require.NoError(t, err)
	require.Equal(t, final.PayloadJSON, replayed.PayloadJSON)
	// An actual newly observed provider bill has a separate correction identity.
	evidence.Cost.Source = "provider_reported"
	require.NoError(t, s.RecordBillingJobSettlement(ctx, ref, "aggregate-log", "runner", evidence))
	latest, err := s.GetBillingExecution(ctx, r.Scope, r.ReceiptID)
	require.NoError(t, err)
	require.NotEqual(t, final.Receipt.EventID, latest.EventID)
	require.Equal(t, final.Receipt.EventID, latest.SupersedesEventID)
	unchanged, err := s.GetBillingEvent(ctx, r.Scope, final.Receipt.EventID)
	require.NoError(t, err)
	require.Equal(t, final.PayloadJSON, unchanged.PayloadJSON)
	countBefore, err := s.ListBillingExecutions(ctx, r.Scope, "operation", 100)
	require.NoError(t, err)
	require.Len(t, countBefore, 1)
	require.NoError(t, s.RecordBillingJobSettlement(ctx, ref, "aggregate-log", "runner", evidence))
	latestAgain, err := s.GetBillingExecution(ctx, r.Scope, r.ReceiptID)
	require.NoError(t, err)
	require.Equal(t, latest.EventID, latestAgain.EventID)
	require.NoError(t, s.DB().Model(&tables.TableProviderJob{}).Where("id = ?", ref).Update("runner_id", "another-runner").Error)
	evidence.Cost.Amount = "10"
	require.Error(t, s.RecordBillingJobSettlement(ctx, ref, "aggregate-log", "runner", evidence), "stale job owner must not publish financial evidence")
}
