package jobaccounting

import (
	"context"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
	"testing"
)

type billingSettlementProbe struct {
	JobStore
	receipt *schemas.BillingReceipt
	calls   int
}

func (s *billingSettlementProbe) RecordBillingJobSettlement(_ context.Context, _, _, _ string, evidence *schemas.BillingReceipt) error {
	s.receipt = evidence
	s.calls++
	return nil
}

func TestBillingJobUnknownZeroAndVideoMeasures(t *testing.T) {
	for _, known := range []bool{false, true} {
		probe := &billingSettlementProbe{}
		job := &tables.TableProviderJob{ID: "video-job:fal:task", BillingExecutionID: "original-execution", ProviderStatus: "completed"}
		req := JobRequest{Provider: schemas.Fal, Job: job}
		out := &Outcome{LogID: "original-accounting-log"}
		settlement := &Settlement{RecordUnpriced: true, Priced: known, Complete: known, Model: "model", ApplyDebug: func(row *logstore.Log) {
			row.VideoDebugParsed = &schemas.BifrostVideoDebug{Accounting: &schemas.VideoAccountingDebug{Seconds: schemas.Ptr(6), Size: "1280x720"}}
		}}
		if err := recordBillingSettlement(context.Background(), probe, req, out, settlement); err != nil {
			t.Fatal(err)
		}
		if probe.calls != 1 || len(probe.receipt.Measures) != 1 || probe.receipt.Measures[0].Value != "6" {
			t.Fatal(probe.receipt)
		}
		if (probe.receipt.Cost != nil) != known {
			t.Fatal("unknown and measured zero were conflated", probe.receipt)
		}
		if known && probe.receipt.Cost.Amount != "0" {
			t.Fatal("known zero cost lost")
		}
	}
}

func TestBillingMixedBatchPartialAndUnmeteredNoop(t *testing.T) {
	probe := &billingSettlementProbe{}
	job := &tables.TableProviderJob{ID: "batch-job:openai:task", BillingExecutionID: "execution", ProviderStatus: "completed"}
	req := JobRequest{Provider: schemas.OpenAI, Job: job}
	settlement := &Settlement{RecordUnpriced: true, Usage: schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 12, TotalTokens: 22}, ModelBreakdowns: map[string]schemas.BatchModelBreakdown{"a": {Usage: schemas.BifrostLLMUsage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}}, "b": {Usage: schemas.BifrostLLMUsage{PromptTokens: 4, CompletionTokens: 8, TotalTokens: 12}}}, ApplyDebug: func(row *logstore.Log) {
		row.BatchDebugParsed = &schemas.BifrostBatchDebug{Accounting: &schemas.BatchAccountingDebug{Incomplete: true}}
	}}
	if err := recordBillingSettlement(context.Background(), probe, req, &Outcome{LogID: "log"}, settlement); err != nil {
		t.Fatal(err)
	}
	if len(probe.receipt.ModelUsage) != 2 || probe.receipt.Completeness != "partial" || probe.receipt.Cost != nil {
		t.Fatal(probe.receipt)
	}
	job.BillingExecutionID = ""
	probe.calls = 0
	if err := recordBillingSettlement(context.Background(), probe, req, &Outcome{}, settlement); err != nil || probe.calls != 0 {
		t.Fatal("unmetered job acquired extra receipt work")
	}
}
