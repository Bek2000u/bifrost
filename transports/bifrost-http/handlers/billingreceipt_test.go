package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/valyala/fasthttp"
	"testing"
	"time"
)

type billingHTTPStore struct {
	configstore.ConfigStore
	scope schemas.BillingScope
	acked bool
	token string
}

func (s *billingHTTPStore) BillingGatewayID(context.Context) (string, error) {
	return "11111111-1111-4111-8111-111111111111", nil
}

func (s *billingHTTPStore) GetVirtualKeyByValue(_ context.Context, value string) (*tables.TableVirtualKey, error) {
	if value != "backend-key" {
		return nil, errors.New("invalid")
	}
	return &tables.TableVirtualKey{Metadata: map[string]string{"billing_app_id": "app", "billing_tenant_id": "tenant"}, IsActive: schemas.Ptr(true)}, nil
}
func (s *billingHTTPStore) GetBillingExecution(_ context.Context, scope schemas.BillingScope, id string) (*schemas.BillingReceipt, error) {
	s.scope = scope
	if id != "own" {
		return nil, errors.New("not found")
	}
	return &schemas.BillingReceipt{ReceiptID: id, Scope: scope}, nil
}
func (s *billingHTTPStore) ListBillingExecutions(_ context.Context, scope schemas.BillingScope, _ string, _ int) ([]schemas.BillingReceipt, error) {
	s.scope = scope
	return []schemas.BillingReceipt{}, nil
}
func (s *billingHTTPStore) GetBillingEvent(_ context.Context, scope schemas.BillingScope, _ string) (*schemas.BillingDelivery, error) {
	s.scope = scope
	return &schemas.BillingDelivery{}, nil
}
func (s *billingHTTPStore) ClaimBillingEvents(_ context.Context, scope schemas.BillingScope, _ time.Time, _ time.Duration, _ int) ([]schemas.BillingDelivery, error) {
	s.scope = scope
	return []schemas.BillingDelivery{}, nil
}
func (s *billingHTTPStore) AckBillingEvent(_ context.Context, scope schemas.BillingScope, _, _, token string) error {
	s.scope = scope
	s.acked = true
	s.token = token
	return nil
}

func TestBillingHTTPUsesAuthenticatedScopeAndRequiresAckIdentity(t *testing.T) {
	store := &billingHTTPStore{}
	handler := NewBillingReceiptHandler(store)
	routes := router.New()
	handler.RegisterRoutes(routes)
	call := func(method, path, key, body string) *fasthttp.RequestCtx {
		c := &fasthttp.RequestCtx{}
		c.Request.Header.SetMethod(method)
		c.Request.SetRequestURI(path)
		if key != "" {
			c.Request.Header.Set("Authorization", "Bearer "+key)
		}
		c.Request.SetBodyString(body)
		routes.Handler(c)
		return c
	}
	if c := call("GET", "/v1/billing/receipts", "", ""); c.Response.StatusCode() != 401 {
		t.Fatal(c.Response.StatusCode())
	}
	if c := call("GET", "/v1/billing/receipts", "bad-key", ""); c.Response.StatusCode() != 401 {
		t.Fatal(c.Response.StatusCode())
	}
	c := call("GET", "/v1/billing/receipts/own?app_id=foreign&tenant_id=other", "backend-key", "")
	if c.Response.StatusCode() != 200 || store.scope.AppID != "app" || store.scope.TenantID != "tenant" {
		t.Fatal("request input overrode credential scope", c.Response.StatusCode(), store.scope)
	}
	var receipt schemas.BillingReceipt
	if err := json.Unmarshal(c.Response.Body(), &receipt); err != nil || receipt.ReceiptID != "own" {
		t.Fatal(err)
	}
	c = call("POST", "/v1/billing/events/event/ack", "backend-key", `{"payload_sha256":"short"}`)
	if c.Response.StatusCode() != 400 || store.acked {
		t.Fatal("incomplete ACK was accepted")
	}
	c = call("POST", "/v1/billing/events/claim", "backend-key", `{"lease_seconds":301}`)
	if c.Response.StatusCode() != 400 {
		t.Fatal("unbounded lease was accepted")
	}
}
