package handlers

import (
	"context"
	"encoding/json"
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/metering"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
	"strings"
	"time"
)

type billingReceiptStore interface {
	BillingGatewayID(context.Context) (string, error)
	GetBillingExecution(context.Context, schemas.BillingScope, string) (*schemas.BillingReceipt, error)
	ListBillingExecutions(context.Context, schemas.BillingScope, string, int) ([]schemas.BillingReceipt, error)
	GetBillingEvent(context.Context, schemas.BillingScope, string) (*schemas.BillingDelivery, error)
	ClaimBillingEvents(context.Context, schemas.BillingScope, time.Time, time.Duration, int) ([]schemas.BillingDelivery, error)
	AckBillingEvent(context.Context, schemas.BillingScope, string, string, string) error
}

type BillingReceiptHandler struct {
	keys  configstore.ConfigStore
	store billingReceiptStore
}

func NewBillingReceiptHandler(store configstore.ConfigStore) *BillingReceiptHandler {
	receipts, ok := store.(billingReceiptStore)
	if !ok {
		return nil
	}
	return &BillingReceiptHandler{keys: store, store: receipts}
}

func (h *BillingReceiptHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/v1/billing/receipts", lib.ChainMiddlewares(h.list, middlewares...))
	r.GET("/v1/billing/receipts/{id}", lib.ChainMiddlewares(h.read, middlewares...))
	r.GET("/v1/billing/events/{id}", lib.ChainMiddlewares(h.event, middlewares...))
	r.POST("/v1/billing/events/claim", lib.ChainMiddlewares(h.claim, middlewares...))
	r.POST("/v1/billing/events/{id}/ack", lib.ChainMiddlewares(h.ack, middlewares...))
}

func (h *BillingReceiptHandler) scope(ctx *fasthttp.RequestCtx) (schemas.BillingScope, bool) {
	value := strings.TrimSpace(strings.TrimPrefix(string(ctx.Request.Header.Peek("Authorization")), "Bearer "))
	if alternate := string(ctx.Request.Header.Peek("x-bf-vk")); value != "" && alternate != "" && value != alternate {
		SendError(ctx, 400, "conflicting backend credentials")
		return schemas.BillingScope{}, false
	}
	if value == "" {
		value = string(ctx.Request.Header.Peek("x-bf-vk"))
	}
	if value == "" {
		SendError(ctx, 401, "scope-bound backend virtual key required")
		return schemas.BillingScope{}, false
	}
	key, err := h.keys.GetVirtualKeyByValue(ctx, value)
	if err != nil || key == nil || (key.IsActive != nil && !*key.IsActive) || (key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now())) {
		SendError(ctx, 401, "invalid backend virtual key")
		return schemas.BillingScope{}, false
	}
	scope, ok := metering.ScopeFromMetadata(key.Metadata)
	if !ok {
		SendError(ctx, 403, "virtual key has no billing scope")
	}
	return scope, ok
}

func (h *BillingReceiptHandler) list(ctx *fasthttp.RequestCtx) {
	scope, ok := h.scope(ctx)
	if !ok {
		return
	}
	rows, err := h.store.ListBillingExecutions(ctx, scope, string(ctx.QueryArgs().Peek("operation_key")), 100)
	if err != nil {
		SendError(ctx, 503, "receipt store unavailable")
		return
	}
	id, idErr := h.store.BillingGatewayID(ctx)
	if idErr != nil {
		SendError(ctx, 503, "receipt store unavailable")
		return
	}
	SendJSON(ctx, map[string]any{"version": schemas.BillingReceiptVersion, "gateway_id": id, "receipts": rows})
}

func (h *BillingReceiptHandler) read(ctx *fasthttp.RequestCtx) {
	scope, ok := h.scope(ctx)
	if !ok {
		return
	}
	row, err := h.store.GetBillingExecution(ctx, scope, ctx.UserValue("id").(string))
	if err != nil {
		SendError(ctx, 404, "receipt not found in this scope")
		return
	}
	SendJSON(ctx, row)
}

func (h *BillingReceiptHandler) event(ctx *fasthttp.RequestCtx) {
	scope, ok := h.scope(ctx)
	if !ok {
		return
	}
	row, err := h.store.GetBillingEvent(ctx, scope, ctx.UserValue("id").(string))
	if err != nil {
		SendError(ctx, 404, "event not found in this scope")
		return
	}
	SendJSON(ctx, row)
}

func (h *BillingReceiptHandler) claim(ctx *fasthttp.RequestCtx) {
	scope, ok := h.scope(ctx)
	if !ok {
		return
	}
	var input struct {
		Limit        int `json:"limit"`
		LeaseSeconds int `json:"lease_seconds"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &input); err != nil {
		SendError(ctx, 400, "invalid claim body")
		return
	}
	if input.LeaseSeconds == 0 {
		input.LeaseSeconds = 60
	}
	if input.LeaseSeconds < 1 || input.LeaseSeconds > 300 {
		SendError(ctx, 400, "invalid lease duration")
		return
	}
	rows, err := h.store.ClaimBillingEvents(ctx, scope, time.Now().UTC(), time.Duration(input.LeaseSeconds)*time.Second, input.Limit)
	if err != nil {
		SendError(ctx, 503, "receipt store unavailable")
		return
	}
	SendJSON(ctx, map[string]any{"events": rows})
}

func (h *BillingReceiptHandler) ack(ctx *fasthttp.RequestCtx) {
	scope, ok := h.scope(ctx)
	if !ok {
		return
	}
	var input struct {
		PayloadSHA256 string `json:"payload_sha256"`
		ClaimToken    string `json:"claim_token"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &input); err != nil || len(input.PayloadSHA256) != 64 || input.ClaimToken == "" {
		SendError(ctx, 400, "digest and claim token are required")
		return
	}
	if err := h.store.AckBillingEvent(ctx, scope, ctx.UserValue("id").(string), input.PayloadSHA256, input.ClaimToken); err != nil {
		SendError(ctx, 409, "event, digest or claim does not match this scope")
		return
	}
	SendJSON(ctx, map[string]any{"acknowledged": true})
}
