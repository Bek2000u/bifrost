package fal

import (
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
)

// billing reads final billed line items for the exact upstream request. Missing,
// delayed or forbidden billing remains unknown and never fails successful media.
func (provider *FalProvider) billing(ctx *schemas.BifrostContext, key schemas.Key, model, id string, headers map[string]string) (*schemas.BifrostCost, *float64, []FalBillingEvent) {
	headers["x-bf-fal-billing-status"] = "pending"
	end := time.Now().UTC()
	query := url.Values{"source": {"billed"}, "request_id": {id}, "endpoint_id": {model}, "start": {end.AddDate(0, 0, -90).Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)}}
	var events []FalBillingEvent
	cursors := map[string]bool{}
	for {
		data, _, err := provider.request(ctx, key, http.MethodGet, provider.platformBaseURL+"/models/billing-events?"+query.Encode(), nil, "application/json", true, false)
		if err != nil {
			return nil, nil, nil
		}
		var page FalBillingPage
		if sonic.Unmarshal(data, &page) != nil || page.HasMore == nil {
			return nil, nil, nil
		}
		for _, event := range page.Events {
			if event.RequestID == id && event.EndpointID == model {
				events = append(events, event)
			}
		}
		if !*page.HasMore {
			break
		}
		if page.NextCursor == "" || cursors[page.NextCursor] {
			return nil, nil, nil
		}
		cursors[page.NextCursor] = true
		query.Set("cursor", page.NextCursor)
	}
	cost, seconds, err := billedUsage(events)
	if err != nil || cost == nil {
		return nil, nil, nil
	}
	headers["x-bf-fal-billing-status"] = "billed"
	return cost, seconds, events
}

func billedUsage(events []FalBillingEvent) (*schemas.BifrostCost, *float64, error) {
	if len(events) == 0 {
		return nil, nil, nil
	}
	total := new(big.Rat)
	duration := new(big.Rat)
	hasDuration := false
	for _, event := range events {
		var amount *big.Rat
		if event.CostNanoUSD != "" {
			nano, ok := new(big.Rat).SetString(event.CostNanoUSD.String())
			if !ok || !nano.IsInt() || nano.Sign() < 0 {
				return nil, nil, fmt.Errorf("invalid fal billed nano USD")
			}
			amount = nano.Quo(nano, big.NewRat(1000000000, 1))
		} else {
			var ok bool
			amount, ok = new(big.Rat).SetString(event.CostTotal.String())
			if !ok || amount.Sign() < 0 {
				return nil, nil, fmt.Errorf("invalid fal billed cost_total")
			}
		}
		total.Add(total, amount)
		var quantity *big.Rat
		if event.Quantity != "" {
			var ok bool
			quantity, ok = new(big.Rat).SetString(event.Quantity.String())
			if !ok || quantity.Sign() < 0 {
				return nil, nil, fmt.Errorf("invalid fal billed quantity")
			}
		}
		factor := int64(0)
		switch event.Unit {
		case "second", "seconds", "audio_second", "audio_seconds", "video_second", "video_seconds":
			factor = 1
		case "minute", "minutes", "audio_minute", "audio_minutes":
			factor = 60
		}
		if factor > 0 && quantity != nil {
			quantity.Mul(quantity, big.NewRat(factor, 1))
			duration.Add(duration, quantity)
			hasDuration = true
		}
	}
	usd, _ := total.Float64()
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return nil, nil, fmt.Errorf("invalid fal total cost")
	}
	cost := &schemas.BifrostCost{TotalCost: usd}
	var seconds *float64
	if hasDuration {
		value, _ := duration.Float64()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, nil, fmt.Errorf("invalid fal duration")
		}
		seconds = &value
	}
	return cost, seconds, nil
}

func nativeDuration(native *FalResult) *float64 {
	value := native.DurationSeconds
	if value == nil {
		value = native.Duration
	}
	if native.Audio != nil && native.Audio.Duration != nil {
		value = native.Audio.Duration
	}
	if value == nil || *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}
