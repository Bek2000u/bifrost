package lumean

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

type LumeanProvider struct {
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	networkConfig       schemas.NetworkConfig
	timeout             time.Duration
	pollInterval        time.Duration
	sendBackRawRequest  bool
	sendBackRawResponse bool
}

var _ schemas.Provider = (*LumeanProvider)(nil)

func NewLumeanProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*LumeanProvider, error) {
	if config == nil {
		return nil, fmt.Errorf("provider config is required")
	}
	config.CheckAndSetDefaults()
	timeout := time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds) * time.Second
	client := &fasthttp.Client{
		ReadTimeout: timeout, WriteTimeout: timeout, MaxConnsPerHost: config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnDuration: time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds) * time.Second,
		MaxConnWaitTimeout:  timeout, MaxConnDuration: time.Duration(schemas.DefaultMaxConnDurationInSeconds) * time.Second,
		MaxIdemponentCallAttempts: 1, ConnPoolStrategy: fasthttp.FIFO,
	}
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = "https://api.lumean.app/api/public"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")
	base, err := url.Parse(config.NetworkConfig.BaseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid lumean base URL")
	}
	return &LumeanProvider{client: client, streamingClient: providerUtils.BuildStreamingClient(client),
		networkConfig: config.NetworkConfig, timeout: timeout, pollInterval: time.Second,
		sendBackRawRequest: config.SendBackRawRequest, sendBackRawResponse: config.SendBackRawResponse}, nil
}

func (provider *LumeanProvider) GetProviderKey() schemas.ModelProvider { return schemas.Lumean }

func (provider *LumeanProvider) do(ctx context.Context, key schemas.Key, method, target string, body []byte, authenticate bool) ([]byte, *schemas.BifrostError) {
	return provider.doWithHeaders(ctx, key, method, target, body, authenticate, nil)
}

func (provider *LumeanProvider) doWithHeaders(ctx context.Context, key schemas.Key, method, target string, body []byte, authenticate bool, headers map[string]string) ([]byte, *schemas.BifrostError) {
	if ctx.Err() != nil {
		return nil, providerUtils.NewBifrostTimeoutError("lumean request cancelled", ctx.Err())
	}
	req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(target)
	req.Header.SetMethod(method)
	req.Header.SetContentType("application/json")
	if authenticate {
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, []string{"Authorization", "X-API-KEY"})
		req.Header.Set("X-API-KEY", key.Value.GetValue())
	}
	if body != nil {
		req.SetBody(body)
	}
	latency, err, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return nil, providerUtils.SetErrorLatency(parseLumeanError(resp), latency)
	}
	if headers != nil {
		headers["Content-Type"] = string(resp.Header.ContentType())
	}
	return append([]byte(nil), resp.Body()...), nil
}

func (provider *LumeanProvider) json(ctx context.Context, key schemas.Key, method, path string, body any, result any) *schemas.BifrostError {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = providerUtils.MarshalSorted(body)
		if err != nil {
			return providerUtils.NewBifrostBadRequestError(err.Error())
		}
	}
	raw, err := provider.do(ctx, key, method, provider.networkConfig.BaseURL+path, encoded, true)
	if err != nil {
		return err
	}
	if err := sonic.Unmarshal(raw, result); err != nil {
		return providerUtils.NewBifrostOperationError("invalid lumean response", err)
	}
	return nil
}

func (provider *LumeanProvider) artifactURL(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	base, _ := url.Parse(provider.networkConfig.BaseURL)
	if u.Scheme == base.Scheme && u.Host == base.Host {
		return true
	}
	if u.Scheme != "https" || u.Port() != "" {
		return false
	}
	for _, host := range []string{"lumean.app", "api.lumean.app", "storage.lumean.app", "files.cartesia.ai"} {
		if u.Hostname() == host {
			return true
		}
	}
	return false
}

func (provider *LumeanProvider) wait(ctx context.Context) bool {
	timer := time.NewTimer(provider.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (provider *LumeanProvider) pendingError(model, id, clientID, message string) *schemas.BifrostError {
	if id != "" {
		message += "; task_id=" + id
	}
	if clientID != "" {
		message += "; client_task_id=" + clientID
	}
	err := providerUtils.NewBifrostOperationError(message, nil)
	err.IsBifrostError = true
	err.AllowFallbacks = schemas.Ptr(false)
	err.ExtraFields.RequestType = schemas.SpeechRequest
	err.ExtraFields.Provider = provider.GetProviderKey()
	err.ExtraFields.OriginalModelRequested = model
	err.ExtraFields.RawResponse = &LumeanReceipt{TaskID: id, ClientTaskID: clientID, Status: "reconcile_required"}
	return err
}

func (provider *LumeanProvider) taskHeader(ctx *schemas.BifrostContext, id string) {
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, map[string]string{"x-bf-provider-task-id": id})
}

func (provider *LumeanProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	models, err := provider.catalog(ctx, key)
	if err != nil {
		return nil, err
	}
	pipeline := &providerUtils.ListModelsPipeline{AllowedModels: key.Models, BlacklistedModels: key.BlacklistedModels,
		Aliases: key.Aliases, Unfiltered: request.Unfiltered, ProviderKey: provider.GetProviderKey(), MatchFns: providerUtils.DefaultMatchFns()}
	result := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	if pipeline.ShouldEarlyExit() {
		return result, nil
	}
	included := make(map[string]bool, len(models))
	for _, model := range models {
		for _, filtered := range pipeline.FilterModel(model.ID) {
			entry := schemas.Model{ID: "lumean/" + filtered.ResolvedID, Name: schemas.Ptr(model.Name), SupportedMethods: []string{"speech"}}
			if filtered.AliasValue != "" {
				entry.Alias = schemas.Ptr(filtered.AliasValue)
			}
			result.Data = append(result.Data, entry)
			included[strings.ToLower(filtered.ResolvedID)] = true
		}
	}
	result.Data = append(result.Data, pipeline.BackfillModels(included)...)
	return result, nil
}

func (provider *LumeanProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("model listing request is required")
	}
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, provider.listModelsByKey)
}
