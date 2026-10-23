// Package metering records provider evidence without owning customer balances.
package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

const virtualKeyMetadata schemas.BifrostContextKey = "bifrost-governance-virtual-key-metadata"
const receiptState schemas.BifrostContextKey = "bifrost-billing-receipt-state"

type Store interface {
	BillingGatewayID(context.Context) (string, error)
	BeginBillingExecution(context.Context, *schemas.BillingReceipt) error
	RecordBillingSnapshot(context.Context, *schemas.BillingReceipt) error
	FinalizeBillingReceipt(context.Context, *schemas.BillingReceipt) (*schemas.BillingReceipt, error)
}

type Plugin struct {
	store     Store
	gatewayID string
}
type attempt struct {
	mu       sync.Mutex
	receipt  schemas.BillingReceipt
	terminal bool
}

func New(ctx context.Context, store Store) (*Plugin, error) {
	id, err := store.BillingGatewayID(ctx)
	if err != nil {
		return nil, err
	}
	return &Plugin{store: store, gatewayID: id}, nil
}
func (*Plugin) GetName() string                                                       { return "billing-receipts" }
func (*Plugin) Cleanup() error                                                        { return nil }
func (*Plugin) PreRequestHook(*schemas.BifrostContext, *schemas.BifrostRequest) error { return nil }

// ScopeFromMetadata accepts operator-owned key metadata, never request headers.
func ScopeFromMetadata(metadata map[string]string) (schemas.BillingScope, bool) {
	s := schemas.BillingScope{AppID: metadata["billing_app_id"], TenantID: metadata["billing_tenant_id"]}
	return s, s.AppID != "" && s.TenantID != ""
}

func billingError(message string) *schemas.BifrostError {
	return &schemas.BifrostError{IsBifrostError: true, StatusCode: schemas.Ptr(424), AllowFallbacks: schemas.Ptr(false), Error: &schemas.ErrorField{Message: message}}
}

func billable(t schemas.RequestType) bool {
	switch string(t) {
	case "chat_completion", "chat_completion_stream", "text_completion", "text_completion_stream", "responses", "responses_stream", "embedding", "rerank", "speech", "speech_stream", "transcription", "transcription_stream", "image_generation", "image_generation_stream", "image_edit", "image_edit_stream", "image_variation", "video_generation", "video_remix", "video_edit", "batch_create", "ocr", "compaction", "decision":
		return true
	}
	return false
}

func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	metadata, _ := ctx.Value(virtualKeyMetadata).(map[string]string)
	operation, _ := ctx.Value(schemas.BifrostContextKeyBillingOperation).(string)
	hold, _ := ctx.Value(schemas.BifrostContextKeyBillingHold).(string)
	if metadata["billing_app_id"] == "" && metadata["billing_tenant_id"] == "" && operation == "" && hold == "" {
		return req, nil, nil
	}
	if !billable(req.RequestType) {
		return req, nil, nil
	}
	scope, ok := ScopeFromMetadata(metadata)
	if !ok || operation == "" || hold == "" || len(operation) > 255 || len(hold) > 255 {
		return req, &schemas.LLMPluginShortCircuit{Error: billingError("billing scope, operation and hold reference are required")}, nil
	}
	provider, model, _ := req.GetRequestFields()
	requestID, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string)
	r := schemas.BillingReceipt{Version: schemas.BillingReceiptVersion, GatewayID: p.gatewayID, EventID: uuid.NewString(), ReceiptID: uuid.NewString(), ExecutionID: uuid.NewString(), AttemptID: uuid.NewString(), Scope: scope, OperationKey: operation, HoldID: hold, RequestID: requestID, Phase: "execution", Status: "pending", Completeness: "unknown", Measures: []schemas.BillingMeasure{}, OccurredAt: time.Now().UTC(), Dimensions: map[string]string{"request_type": string(req.RequestType), "requested_provider": string(provider), "requested_model": model}}
	captureDimensions(req, r.Dimensions)
	if err := p.store.BeginBillingExecution(ctx, &r); err != nil {
		return req, &schemas.LLMPluginShortCircuit{Error: billingError("billing evidence could not be persisted; provider was not submitted")}, nil
	}
	ctx.SetValue(receiptState, &attempt{receipt: r})
	ctx.SetValue(schemas.BifrostContextKeyBillingReceiptID, r.ReceiptID)
	ctx.SetValue(schemas.BifrostContextKeyBillingExecutionID, r.ExecutionID)
	ctx.SetValue(schemas.BifrostContextKeyBillingObserver, p)
	return req, nil, nil
}

func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, providerErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return result, providerErr, nil
}

func (p *Plugin) ObserveBillingReceipt(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, providerErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	state, _ := ctx.Value(receiptState).(*attempt)
	if state == nil {
		return result, providerErr, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.terminal {
		return result, providerErr, nil
	}
	r := state.receipt
	r.OccurredAt = time.Now().UTC()
	typeName, provider, _, model := bifrost.GetResponseFields(result, providerErr)
	if provider != "" {
		r.Dimensions["provider"] = string(provider)
	}
	if model != "" {
		r.Dimensions["model"] = model
	}
	for name, key := range map[string]schemas.BifrostContextKey{"retry_index": schemas.BifrostContextKeyNumberOfRetries, "fallback_index": schemas.BifrostContextKeyFallbackIndex} {
		if value := ctx.Value(key); value != nil {
			r.Dimensions[name] = fmt.Sprint(value)
		}
	}
	usage := responseUsage(result)
	if providerErr != nil && providerErr.ExtraFields.BilledUsage != nil {
		usage = providerErr.ExtraFields.BilledUsage
	}
	if native, ok := usage.(*schemas.BifrostLLMUsage); ok {
		if native.ServerSideFallbackModel != nil {
			r.Dimensions["model"] = *native.ServerSideFallbackModel
			r.Dimensions["server_side_fallback"] = "true"
		}
		if native.Speed != nil {
			r.Dimensions["speed"] = *native.Speed
		}
		if native.InferenceGeo != nil {
			r.Dimensions["inference_geo"] = *native.InferenceGeo
		}
	}
	if result != nil && result.ChatResponse != nil && result.ChatResponse.ServiceTier != nil {
		r.Dimensions["service_tier"] = string(*result.ChatResponse.ServiceTier)
	}
	cacheHit := false
	if result != nil {
		if extra := result.GetExtraFields(); extra != nil && extra.CacheDebug != nil {
			cacheHit = extra.CacheDebug.CacheHit
		}
	}
	if cacheHit {
		r.Dimensions["cache_hit"] = "true"
		usage = nil
		r.Measures = []schemas.BillingMeasure{}
		r.Cost = nil
	}
	if usage != nil {
		measures, cost, err := Measures(usage)
		if err != nil {
			return result, billingError("billing usage is invalid; reconcile the original execution"), err
		}
		r.Measures = measures
		r.Cost = cost
	}
	if result != nil && result.ImageGenerationResponse != nil && len(result.ImageGenerationResponse.Data) > 0 {
		r.Measures = append(r.Measures, schemas.BillingMeasure{Name: "images", Unit: "image", Value: strconv.Itoa(len(result.ImageGenerationResponse.Data))})
	}
	terminal := !bifrost.IsStreamRequestType(typeName) || bifrost.IsFinalChunk(ctx) || providerErr != nil
	if !terminal {
		if usage == nil {
			return result, providerErr, nil
		}
		r.Completeness = "partial"
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := p.store.RecordBillingSnapshot(persistCtx, &r); err != nil {
			return result, billingError("billing evidence write failed; reconcile the original execution"), err
		}
		state.receipt = r
		return result, providerErr, nil
	}
	r.Status = "completed"
	r.Completeness = "unknown"
	if len(r.Measures) > 0 {
		r.Completeness = "complete"
	}
	if cacheHit {
		r.Completeness = "complete"
	}
	if providerErr != nil {
		r.Status = "unknown"
		if len(r.Measures) > 0 {
			r.Completeness = "partial"
		}
	}
	if result != nil && result.BatchCreateResponse != nil {
		r.Status = "pending"
		r.Completeness = "unknown"
		r.Job = &schemas.BillingJobReference{Kind: "batch", Provider: string(provider), TaskID: result.BatchCreateResponse.ID}
	}
	if result != nil && result.VideoGenerationResponse != nil {
		v := result.VideoGenerationResponse
		r.Status = "pending"
		r.Completeness = "unknown"
		r.Job = &schemas.BillingJobReference{Kind: "video", Provider: string(provider), TaskID: v.ID}
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := p.store.FinalizeBillingReceipt(persistCtx, &r); err != nil {
		return result, billingError("billing evidence write failed; reconcile the original execution; do not resubmit"), err
	}
	state.receipt = r
	state.terminal = true
	return result, providerErr, nil
}

func responseUsage(r *schemas.BifrostResponse) any {
	if r == nil {
		return nil
	}
	switch {
	case r.ChatResponse != nil:
		if r.ChatResponse.Usage != nil {
			return r.ChatResponse.Usage
		}
	case r.TextCompletionResponse != nil:
		if r.TextCompletionResponse.Usage != nil {
			return r.TextCompletionResponse.Usage
		}
	case r.ResponsesResponse != nil:
		if r.ResponsesResponse.Usage != nil {
			return r.ResponsesResponse.Usage
		}
	case r.ResponsesStreamResponse != nil:
		if r.ResponsesStreamResponse.Response != nil && r.ResponsesStreamResponse.Response.Usage != nil {
			return r.ResponsesStreamResponse.Response.Usage
		}
	case r.EmbeddingResponse != nil:
		if r.EmbeddingResponse.Usage != nil {
			return r.EmbeddingResponse.Usage
		}
	case r.SpeechResponse != nil:
		if r.SpeechResponse.Usage != nil {
			return r.SpeechResponse.Usage
		}
	case r.SpeechStreamResponse != nil:
		if r.SpeechStreamResponse.Usage != nil {
			return r.SpeechStreamResponse.Usage
		}
	case r.TranscriptionResponse != nil:
		if r.TranscriptionResponse.Usage != nil {
			return r.TranscriptionResponse.Usage
		}
	case r.TranscriptionStreamResponse != nil:
		if r.TranscriptionStreamResponse.Usage != nil {
			return r.TranscriptionStreamResponse.Usage
		}
	case r.ImageGenerationResponse != nil:
		if r.ImageGenerationResponse.Usage != nil {
			return r.ImageGenerationResponse.Usage
		}
	case r.ImageGenerationStreamResponse != nil:
		if r.ImageGenerationStreamResponse.Usage != nil {
			return r.ImageGenerationStreamResponse.Usage
		}
	case r.RerankResponse != nil:
		if r.RerankResponse.Usage != nil {
			return r.RerankResponse.Usage
		}
	}
	return nil
}

// Measures decodes only the native usage object, never prompts or output. JSON
// numbers remain decimal strings; float costs are explicitly labelled observations.
func Measures(usage any) ([]schemas.BillingMeasure, *schemas.BillingObservedCost, error) {
	data, err := json.Marshal(usage)
	if err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw map[string]any
	if err = decoder.Decode(&raw); err != nil {
		return nil, nil, err
	}
	result := make([]schemas.BillingMeasure, 0, 12)
	add := func(source map[string]any, key, name, unit string) {
		if n, ok := source[key].(json.Number); ok {
			result = append(result, schemas.BillingMeasure{Name: name, Unit: unit, Value: string(n)})
		}
	}
	for _, field := range [][3]string{{"prompt_tokens", "input_tokens", "token"}, {"input_tokens", "input_tokens", "token"}, {"completion_tokens", "output_tokens", "token"}, {"output_tokens", "output_tokens", "token"}, {"total_tokens", "total_tokens", "token"}, {"input_chars", "input_characters", "character"}, {"audio_seconds", "audio_seconds", "second"}, {"seconds", "audio_seconds", "second"}} {
		add(raw, field[0], field[1], field[2])
	}
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details", "input_token_details"} {
		if details, ok := raw[key].(map[string]any); ok {
			add(details, "cached_read_tokens", "cache_read_tokens", "token")
			add(details, "cached_tokens", "cache_read_tokens", "token")
			add(details, "cached_write_tokens", "cache_write_tokens", "token")
			add(details, "cache_write_tokens", "cache_write_tokens", "token")
			add(details, "audio_tokens", "input_audio_tokens", "token")
			add(details, "image_tokens", "input_image_tokens", "token")
			if writes, ok := details["cached_write_token_details"].(map[string]any); ok {
				add(writes, "cached_write_tokens_5m", "cache_write_5m_tokens", "token")
				add(writes, "cached_write_tokens_1h", "cache_write_1h_tokens", "token")
			}
		}
	}
	for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
		if details, ok := raw[key].(map[string]any); ok {
			add(details, "reasoning_tokens", "reasoning_tokens", "token")
			add(details, "audio_tokens", "output_audio_tokens", "token")
			add(details, "image_tokens", "output_image_tokens", "token")
		}
	}
	var cost *schemas.BillingObservedCost
	if observed, ok := raw["cost"].(map[string]any); ok {
		amount := "0"
		if n, ok := observed["total_cost"].(json.Number); ok {
			amount = string(n)
		}
		value, parseErr := strconv.ParseFloat(amount, 64)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		if value < 0 {
			return nil, nil, fmt.Errorf("negative observed cost")
		}
		cost = &schemas.BillingObservedCost{Amount: strconv.FormatFloat(value, 'f', -1, 64), Currency: "USD", Source: "gateway_observed", Precision: "binary_float64"}
		if _, hasTotal := observed["total_cost"]; !hasTotal {
			for _, name := range []string{"input_cost", "output_cost", "additional_cost"} {
				if number, ok := observed[name].(json.Number); ok {
					component, err := strconv.ParseFloat(string(number), 64)
					if err != nil || component < 0 {
						return nil, nil, fmt.Errorf("invalid observed cost component")
					}
					if component > 0 {
						cost = nil
						break
					}
				}
			}
		}
	}
	seen := make(map[string]bool, len(result))
	dedup := result[:0]
	for _, measure := range result {
		if measure.Value == "0" && strings.HasPrefix(measure.Name, "cache_") {
			continue
		}
		if !seen[measure.Name] {
			seen[measure.Name] = true
			dedup = append(dedup, measure)
		}
	}
	if _, ok := usage.(*schemas.SpeechUsage); ok {
		filtered := dedup[:0]
		for _, m := range dedup {
			if m.Unit == "token" && m.Value == "0" {
				continue
			}
			filtered = append(filtered, m)
		}
		dedup = filtered
	}
	for _, m := range dedup {
		if strings.HasPrefix(m.Value, "-") {
			return nil, nil, fmt.Errorf("negative billing measure %s", m.Name)
		}
	}
	for i := range dedup {
		value, err := decimalString(dedup[i].Value)
		if err != nil {
			return nil, nil, err
		}
		dedup[i].Value = value
	}
	if cost != nil {
		if observed, ok := raw["cost"].(map[string]any); ok {
			for _, name := range []string{"input", "output", "additional"} {
				if number, ok := observed[name+"_cost"].(json.Number); ok {
					value, err := decimalString(string(number))
					if err != nil {
						return nil, nil, err
					}
					if cost.Components == nil {
						cost.Components = map[string]string{}
					}
					cost.Components[name] = value
				}
			}
		}
	}
	return dedup, cost, nil
}

func decimalString(value string) (string, error) {
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("negative billing quantity")
	}
	index := strings.IndexAny(value, "eE")
	if index < 0 {
		return value, nil
	}
	exponent, err := strconv.Atoi(value[index+1:])
	if err != nil || exponent > 1000 || exponent < -1000 {
		return "", fmt.Errorf("invalid billing exponent")
	}
	coefficient := strings.SplitN(value[:index], ".", 2)
	digits := coefficient[0]
	point := len(digits) + exponent
	if len(coefficient) > 1 {
		digits += coefficient[1]
	}
	if point <= 0 {
		return "0." + strings.Repeat("0", -point) + digits, nil
	}
	if point >= len(digits) {
		return digits + strings.Repeat("0", point-len(digits)), nil
	}
	return digits[:point] + "." + digits[point:], nil
}
