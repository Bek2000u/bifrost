package jobaccounting

import (
	"context"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/metering"
	"sort"
	"strconv"
	"strings"
)

type billingReceiptRecorder interface {
	RecordBillingJobSettlement(context.Context, string, string, string, *schemas.BillingReceipt) error
}

func recordBillingSettlement(ctx context.Context, store JobStore, req JobRequest, out *Outcome, settlement *Settlement) error {
	recorder, ok := store.(billingReceiptRecorder)
	if !ok || req.Job.BillingExecutionID == "" || (!settlement.Priced && !settlement.RecordUnpriced) {
		return nil
	}
	evidence := &schemas.BillingReceipt{Status: "completed", Completeness: "unknown", Measures: []schemas.BillingMeasure{}, Dimensions: map[string]string{"provider": string(req.Provider), "model": settlement.Model, "provider_status": req.Job.ProviderStatus}}
	if strings.Contains(req.Job.ProviderStatus, "fail") || req.Job.ProviderStatus == "expired" {
		evidence.Status = "failed"
	}
	if strings.Contains(req.Job.ProviderStatus, "cancel") {
		evidence.Status = "cancelled"
	}
	evidence.Dimensions["priced_count"] = strconv.Itoa(settlement.PricedCount)
	evidence.Dimensions["unpriced_count"] = strconv.Itoa(settlement.UnpricedCount)
	evidence.Dimensions["failed_count"] = strconv.Itoa(settlement.FailedCount)
	if settlement.Usage.TotalTokens > 0 || settlement.Usage.PromptTokens > 0 || settlement.Usage.CompletionTokens > 0 {
		measures, _, err := metering.Measures(&settlement.Usage)
		if err != nil {
			return err
		}
		evidence.Measures = measures
		evidence.Completeness = "complete"
	}
	if settlement.Priced && settlement.Complete {
		evidence.Completeness = "complete"
		evidence.Cost = &schemas.BillingObservedCost{Amount: strconv.FormatFloat(settlement.Cost, 'f', -1, 64), Currency: "USD", Source: "gateway_calculated", Precision: "binary_float64"}
	}
	models := make([]string, 0, len(settlement.ModelBreakdowns))
	for model := range settlement.ModelBreakdowns {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		breakdown := settlement.ModelBreakdowns[model]
		measures, _, err := metering.Measures(&breakdown.Usage)
		if err != nil {
			return err
		}
		item := schemas.BillingModelUsage{Model: model, Measures: measures}
		if breakdown.Cost != nil {
			item.Cost = &schemas.BillingObservedCost{Amount: strconv.FormatFloat(*breakdown.Cost, 'f', -1, 64), Currency: "USD", Source: "gateway_calculated", Precision: "binary_float64"}
		}
		evidence.ModelUsage = append(evidence.ModelUsage, item)
	}
	if settlement.ApplyDebug != nil {
		entry := &logstore.Log{}
		settlement.ApplyDebug(entry)
		if entry.BatchDebugParsed != nil && entry.BatchDebugParsed.Accounting != nil && entry.BatchDebugParsed.Accounting.Incomplete {
			evidence.Completeness = "partial"
		}
		if entry.VideoDebugParsed != nil && entry.VideoDebugParsed.Accounting != nil {
			detail := entry.VideoDebugParsed.Accounting
			if detail.Seconds != nil {
				evidence.Measures = append(evidence.Measures, schemas.BillingMeasure{Name: "video_seconds", Unit: "second", Value: strconv.Itoa(*detail.Seconds)})
			}
			if detail.Size != "" {
				evidence.Dimensions["size"] = detail.Size
			}
			if detail.ProviderCost != nil {
				evidence.Cost = &schemas.BillingObservedCost{Amount: strconv.FormatFloat(*detail.ProviderCost, 'f', -1, 64), Currency: "USD", Source: "provider_reported", Precision: "binary_float64"}
			}
			if !detail.Incomplete && len(evidence.Measures) > 0 {
				evidence.Completeness = "complete"
			}
		}
	}
	if !settlement.Complete && settlement.Priced {
		evidence.Completeness = "partial"
		evidence.Cost = nil
	}
	return recorder.RecordBillingJobSettlement(ctx, req.Job.ID, out.LogID, req.ClaimedBy, evidence)
}
