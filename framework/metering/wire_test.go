package metering

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/maximhq/bifrost/core/schemas"
	"os"
	"testing"
	"time"
)

func TestBillingWireFixture(t *testing.T) {
	stamp := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	r := schemas.BillingReceipt{Version: schemas.BillingReceiptVersion, GatewayID: "11111111-1111-4111-8111-111111111111", EventID: "22222222-2222-4222-8222-222222222222", ReceiptID: "33333333-3333-4333-8333-333333333333", ExecutionID: "44444444-4444-4444-8444-444444444444", AttemptID: "55555555-5555-4555-8555-555555555555", Scope: schemas.BillingScope{AppID: "videoforge", TenantID: "fixture-tenant"}, OperationKey: "fixture-operation", HoldID: "chld_00000000000000000000000001", RequestID: "caller-correlation-only", Phase: "execution", Status: "completed", Completeness: "complete", Measures: []schemas.BillingMeasure{{Name: "input_tokens", Unit: "token", Value: "128"}, {Name: "output_tokens", Unit: "token", Value: "32"}, {Name: "total_tokens", Unit: "token", Value: "160"}}, Dimensions: map[string]string{"request_type": "chat_completion", "requested_provider": "openai", "requested_model": "fixture-model", "provider": "openai", "model": "fixture-model"}, Cost: &schemas.BillingObservedCost{Amount: "0.000128", Currency: "USD", Source: "gateway_observed", Precision: "binary_float64"}, OccurredAt: stamp}
	payload, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	lease := stamp.Add(time.Minute)
	envelope := schemas.BillingDelivery{PayloadJSON: string(payload), Receipt: r, PayloadSHA256: hex.EncodeToString(digest[:]), ClaimToken: "66666666-6666-4666-8666-666666666666", LeaseUntil: &lease, Attempts: 1}
	delivery, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UPDATE_BILLING_FIXTURES") == "1" {
		if err = os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile("testdata/receipt.json", payload, 0644); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile("testdata/delivery.json", delivery, 0644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile("testdata/receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(payload) {
		t.Fatal("receipt wire fixture changed; coordinate consumer contract before updating")
	}
	actual, err = os.ReadFile("testdata/delivery.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(delivery) {
		t.Fatal("delivery wire fixture changed")
	}
}
