package metering

import (
	"context"
	"errors"
	"github.com/maximhq/bifrost/core/schemas"
	"testing"
)

type receiptMemoryStore struct {
	receipts  []*schemas.BillingReceipt
	snapshots int
	fail      bool
	failFinal bool
	finishes  int
}

func (*receiptMemoryStore) BillingGatewayID(context.Context) (string, error) {
	return "gateway-test", nil
}
func (s *receiptMemoryStore) BeginBillingExecution(_ context.Context, r *schemas.BillingReceipt) error {
	if s.fail {
		return errors.New("offline")
	}
	copy := *r
	s.receipts = append(s.receipts, &copy)
	return nil
}
func (s *receiptMemoryStore) RecordBillingSnapshot(_ context.Context, _ *schemas.BillingReceipt) error {
	s.snapshots++
	return nil
}
func (s *receiptMemoryStore) FinalizeBillingReceipt(_ context.Context, r *schemas.BillingReceipt) (*schemas.BillingReceipt, error) {
	if s.failFinal {
		return nil, errors.New("evidence store offline")
	}
	s.finishes++
	copy := *r
	s.receipts[len(s.receipts)-1] = &copy
	return &copy, nil
}
func receiptContext() *schemas.BifrostContext {
	c := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	c.SetValue(virtualKeyMetadata, map[string]string{"billing_app_id": "app", "billing_tenant_id": "tenant"})
	c.SetValue(schemas.BifrostContextKeyBillingOperation, "op")
	c.SetValue(schemas.BifrostContextKeyBillingHold, "hold")
	c.SetValue(schemas.BifrostContextKeyRequestID, "same-caller-id")
	return c
}
func receiptRequest() *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "test"}}
}

func TestBillingExecutionIdentityAndFailClosed(t *testing.T) {
	s := &receiptMemoryStore{}
	p, _ := New(context.Background(), s)
	for i := 0; i < 2; i++ {
		_, short, err := p.PreLLMHook(receiptContext(), receiptRequest())
		if short != nil || err != nil {
			t.Fatal(short, err)
		}
	}
	if s.receipts[0].ExecutionID == s.receipts[1].ExecutionID || s.receipts[0].EventID == s.receipts[1].EventID {
		t.Fatal("caller ID merged independent work")
	}
	s.fail = true
	_, short, _ := p.PreLLMHook(receiptContext(), receiptRequest())
	if short == nil || short.Error == nil || short.Error.AllowFallbacks == nil || *short.Error.AllowFallbacks {
		t.Fatal("persistence failure allowed provider work/fallback")
	}
}

func TestBillingUnknownAndCancelledPartialUsage(t *testing.T) {
	for _, partial := range []bool{false, true} {
		s := &receiptMemoryStore{}
		p, _ := New(context.Background(), s)
		c := receiptContext()
		p.PreLLMHook(c, receiptRequest())
		var result *schemas.BifrostResponse
		var providerErr *schemas.BifrostError
		if partial {
			providerErr = &schemas.BifrostError{ExtraFields: schemas.BifrostErrorExtraFields{RequestType: schemas.ChatCompletionRequest, BilledUsage: &schemas.BifrostLLMUsage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9}}}
		} else {
			result = &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionRequest}}}
		}
		_, _, err := p.ObserveBillingReceipt(c, result, providerErr)
		if err != nil {
			t.Fatal(err)
		}
		r := s.receipts[0]
		if r.Cost != nil {
			t.Fatal("unknown cost became zero")
		}
		if partial {
			if r.Completeness != "partial" || len(r.Measures) == 0 {
				t.Fatal(r)
			}
		} else {
			if r.Completeness != "unknown" || len(r.Measures) != 0 {
				t.Fatal("missing usage became measured zero", r)
			}
		}
	}
}

func TestBillingTerminalSnapshotIsNotAddedTwice(t *testing.T) {
	s := &receiptMemoryStore{}
	p, _ := New(context.Background(), s)
	c := receiptContext()
	req := receiptRequest()
	req.RequestType = schemas.ChatCompletionStreamRequest
	p.PreLLMHook(c, req)
	response := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}, ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionStreamRequest}}}
	p.ObserveBillingReceipt(c, response, nil)
	c.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	p.ObserveBillingReceipt(c, response, nil)
	p.ObserveBillingReceipt(c, response, nil)
	if s.snapshots != 1 || len(s.receipts) != 1 {
		t.Fatal("unexpected receipt lifecycle", s.snapshots, len(s.receipts))
	}
	if s.finishes != 1 {
		t.Fatal("terminal event written more than once")
	}
	for _, m := range s.receipts[0].Measures {
		if m.Name == "total_tokens" && m.Value != "8" {
			t.Fatal("usage double counted", m)
		}
	}
}

func TestBillingExactMeasuresAndObservedZeroCost(t *testing.T) {
	measures, cost, err := Measures(map[string]any{"prompt_tokens": int64(9007199254740993), "cost": map[string]any{"total_cost": 0}})
	if err != nil || len(measures) != 1 || measures[0].Value != "9007199254740993" || cost == nil || cost.Amount != "0" || cost.Precision != "binary_float64" {
		t.Fatal(measures, cost, err)
	}
	measures, cost, err = Measures(&schemas.SpeechUsage{InputChars: 12})
	if err != nil || cost != nil || len(measures) != 1 || measures[0].Name != "input_characters" {
		t.Fatal("character backfill invented token counts", measures, cost, err)
	}
	_, cost, err = Measures(&schemas.BifrostLLMUsage{Cost: &schemas.BifrostCost{InputCost: 1}})
	if err != nil || cost != nil {
		t.Fatal("a partial cost breakdown became a known zero total", cost, err)
	}
}

func BenchmarkBillingUnmetered(b *testing.B) {
	p, _ := New(context.Background(), &receiptMemoryStore{})
	c := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := receiptRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.PreLLMHook(c, req)
		p.ObserveBillingReceipt(c, nil, nil)
	}
}

func TestBillingCacheAndPostWriteFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		s := &receiptMemoryStore{failFinal: fail}
		p, _ := New(context.Background(), s)
		ctx := receiptContext()
		p.PreLLMHook(ctx, receiptRequest())
		id := s.receipts[0].ExecutionID
		response := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}, ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionRequest, CacheDebug: &schemas.BifrostCacheMetadata{CacheHit: true}}}}
		_, providerErr, err := p.ObserveBillingReceipt(ctx, response, nil)
		if fail {
			if err == nil || providerErr == nil || providerErr.AllowFallbacks == nil || *providerErr.AllowFallbacks || s.receipts[0].ExecutionID != id || s.finishes != 0 {
				t.Fatal("evidence failure did not preserve the original execution", providerErr, err)
			}
		} else {
			if err != nil || len(s.receipts[0].Measures) != 0 || s.receipts[0].Dimensions["cache_hit"] != "true" {
				t.Fatal("historical cached usage was billed as new work", s.receipts[0], err)
			}
		}
	}
}

func TestBillingMediaDimensionsExcludeContent(t *testing.T) {
	req := &schemas.BifrostRequest{ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Input: &schemas.ImageGenerationInput{Prompt: "private prompt"}, Params: &schemas.ImageGenerationParameters{Quality: schemas.Ptr("high"), Size: schemas.Ptr("1024x1024"), NegativePrompt: schemas.Ptr("private negative prompt"), InputImages: []string{"private-image-url"}}}}
	dimensions := map[string]string{}
	captureDimensions(req, dimensions)
	if len(dimensions) != 2 || dimensions["quality"] != "high" || dimensions["size"] != "1024x1024" {
		t.Fatal(dimensions)
	}
	measures, cost, err := Measures(map[string]any{"audio_seconds": 0.000001, "cost": map[string]any{"total_cost": 0.1, "additional_cost": 0.02}})
	if err != nil || len(measures) != 1 || measures[0].Value != "0.000001" || cost.Components["additional"] != "0.02" {
		t.Fatal(measures, cost, err)
	}
}
