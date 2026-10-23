package bifrost

import (
	"context"
	"github.com/maximhq/bifrost/core/schemas"
	"testing"
)

type billingObserverProbe struct{ observed bool }

func (p *billingObserverProbe) ObserveBillingReceipt(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	p.observed = true
	return r, e, nil
}

type billingPostHookProbe struct {
	fakeRoutingPlugin
	observer       *billingObserverProbe
	beforeObserver bool
}

type billingPreHookProbe struct {
	fakeRoutingPlugin
	observer *billingObserverProbe
}

func (p *billingPreHookProbe) PreLLMHook(ctx *schemas.BifrostContext, r *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	ctx.SetValue(schemas.BifrostContextKeyBillingObserver, p.observer)
	return r, nil, nil
}

func (p *billingPostHookProbe) PostLLMHook(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	p.beforeObserver = !p.observer.observed
	return r, e, nil
}

func TestBillingObserverPrecedesLoggingAndSkipsInternalPolls(t *testing.T) {
	observer := &billingObserverProbe{}
	post := &billingPostHookProbe{fakeRoutingPlugin: fakeRoutingPlugin{name: "logging-probe"}, observer: observer}
	start := &billingPreHookProbe{fakeRoutingPlugin: fakeRoutingPlugin{name: "billing-receipts"}, observer: observer}
	pipeline := newRoutingCommitPipeline(start, post)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, _, entered := pipeline.RunLLMPreHooks(ctx, &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest})
	pipeline.RunPostLLMHooks(ctx, nil, nil, entered)
	if !observer.observed || post.beforeObserver {
		t.Fatal("job/log post-hook ran before durable billing observation")
	}
	observer.observed = false
	ctx.SetValue(schemas.BifrostContextKeySkipPluginPipeline, true)
	pipeline.RunPostLLMHooks(ctx, nil, nil, 1)
	if observer.observed {
		t.Fatal("internal provider poll generated another receipt")
	}
}

func BenchmarkBillingObserverDisabledCheck(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if observer, ok := ctx.Value(schemas.BifrostContextKeyBillingObserver).(schemas.BillingReceiptObserver); ok {
			observer.ObserveBillingReceipt(ctx, nil, nil)
		}
	}
}
