// Package fastgen implements Fastgen's native prompt and asynchronous media APIs.
package fastgen

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

type FastgenProvider struct {
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	networkConfig       schemas.NetworkConfig
	logger              schemas.Logger
	sendBackRawRequest  bool
	sendBackRawResponse bool
	pollInterval        time.Duration
}

var _ schemas.Provider = (*FastgenProvider)(nil)

func NewFastgenProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*FastgenProvider, error) {
	config.CheckAndSetDefaults()
	timeout := time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds) * time.Second
	client := &fasthttp.Client{ReadTimeout: timeout, WriteTimeout: timeout, MaxConnsPerHost: config.NetworkConfig.MaxConnsPerHost, MaxIdleConnDuration: time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds) * time.Second, MaxConnWaitTimeout: timeout, MaxConnDuration: time.Duration(schemas.DefaultMaxConnDurationInSeconds) * time.Second, ConnPoolStrategy: fasthttp.FIFO}
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	// A stale keepalive connection can fail after the paid POST was accepted.
	// Disable fasthttp's internal retry as well as core retries on unknown outcomes.
	client.MaxIdemponentCallAttempts = 1
	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = "https://api.fast-gen.ai"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")
	return &FastgenProvider{client: client, streamingClient: providerUtils.BuildStreamingClient(client), networkConfig: config.NetworkConfig, logger: logger, sendBackRawRequest: config.SendBackRawRequest, sendBackRawResponse: config.SendBackRawResponse, pollInterval: time.Second}, nil
}

func (provider *FastgenProvider) GetProviderKey() schemas.ModelProvider { return schemas.Fastgen }

func (provider *FastgenProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, func(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
		body, _, err := provider.request(ctx, key, http.MethodGet, "/api/v6/models", nil, false)
		if err != nil {
			return nil, err
		}
		var models []FastgenModel
		if decode := sonic.Unmarshal(body, &models); decode != nil {
			return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, decode)
		}
		return ToBifrostListModelsResponse(models, key, request.Unfiltered), nil
	})
}

func (provider *FastgenProvider) ChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFastgenChatRequest(request))
	})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	nativeBody, headers, err := provider.request(ctx, key, http.MethodPost, "/api/v6/prompts/generate", body, true)
	if err != nil {
		return nil, err
	}
	var native FastgenPromptResponse
	if decode := sonic.Unmarshal(nativeBody, &native); decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, decode), "")
	}
	result, decode := ToBifrostChatResponse(&native, request.Model)
	if decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(decode.Error(), nil), native.OperationID)
	}
	result.ExtraFields = provider.extra(ctx, body, &native, headers, start)
	return result, nil
}

func (provider *FastgenProvider) TextCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	if request == nil || request.Input == nil || request.Input.PromptStr == nil || len(request.Input.PromptArray) > 0 {
		return nil, invalidInput("fastgen prompt-generator requires one text prompt")
	}
	result, err := provider.ChatCompletion(ctx, key, request.ToBifrostChatRequest())
	if err != nil {
		return nil, err
	}
	return result.ToTextCompletionResponse(), nil
}

func (provider *FastgenProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, invalidInput("responses request is required")
	}
	if request.Params != nil && (len(request.Params.Tools) > 0 || request.Params.ToolChoice != nil || request.Params.PreviousResponseID != nil || request.Params.Conversation != nil || request.Params.Background != nil && *request.Params.Background || request.Params.Text != nil && request.Params.Text.Format != nil && request.Params.Text.Format.Type != "text") {
		return nil, invalidInput("fastgen does not support responses tools")
	}
	result, err := provider.ChatCompletion(ctx, key, request.ToChatRequest())
	if err != nil {
		return nil, err
	}
	return result.ToBifrostResponsesResponse(), nil
}

func (provider *FastgenProvider) ImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFastgenImageGenerationRequest(request))
	})
	if err != nil {
		return nil, err
	}
	var extra map[string]any
	format := "url"
	if request.Params != nil {
		extra = request.Params.ExtraParams
		if request.Params.ResponseFormat != nil {
			format = *request.Params.ResponseFormat
		}
	}
	return provider.image(ctx, key, request.Model, body, extra, format)
}

func (provider *FastgenProvider) ImageEdit(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFastgenImageEditRequest(request))
	})
	if err != nil {
		return nil, err
	}
	var extra map[string]any
	format := "url"
	if request.Params != nil {
		extra = request.Params.ExtraParams
		if request.Params.ResponseFormat != nil {
			format = *request.Params.ResponseFormat
		}
	}
	return provider.image(ctx, key, request.Model, body, extra, format)
}

func (provider *FastgenProvider) image(ctx *schemas.BifrostContext, key schemas.Key, model string, body []byte, options map[string]any, format string) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	start := time.Now()
	native, headers, err := provider.complete(ctx, key, body, options)
	if err != nil {
		return nil, err
	}
	target, decode := resultURL(native, "image", provider.networkConfig.BaseURL)
	if decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(decode.Error(), nil), native.ID)
	}
	data := schemas.ImageData{URL: target}
	if format == "b64_json" {
		content, _, downloadErr := provider.download(ctx, target)
		if downloadErr != nil {
			return nil, recoveryRequired(downloadErr, native.ID)
		}
		data = schemas.ImageData{B64JSON: base64.StdEncoding.EncodeToString(content)}
	}
	return &schemas.BifrostImageGenerationResponse{ID: native.ID, Model: model, Data: []schemas.ImageData{data}, ExtraFields: provider.extra(ctx, body, native, headers, start)}, nil
}

func (provider *FastgenProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFastgenSpeechRequest(request))
	})
	if err != nil {
		return nil, err
	}
	var extra map[string]any
	if request.Params != nil {
		extra = request.Params.ExtraParams
	}
	start := time.Now()
	native, headers, err := provider.complete(ctx, key, body, extra)
	if err != nil {
		return nil, err
	}
	target, decode := resultURL(native, "audio", provider.networkConfig.BaseURL)
	if decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(decode.Error(), nil), native.ID)
	}
	content, _, err := provider.download(ctx, target)
	if err != nil {
		return nil, recoveryRequired(err, native.ID)
	}
	chars := 0
	if request.Input != nil {
		chars = utf8.RuneCountInString(request.Input.Input)
	}
	return &schemas.BifrostSpeechResponse{Audio: content, Usage: &schemas.SpeechUsage{InputChars: chars}, ExtraFields: provider.extra(ctx, body, native, headers, start)}, nil
}

func (provider *FastgenProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	var converted *FastgenGenerationRequest
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		var conversion error
		converted, conversion = ToFastgenTranscriptionRequest(request)
		return asRequest(converted, conversion)
	})
	if err != nil {
		return nil, err
	}
	var extra map[string]any
	if request.Params != nil {
		extra = request.Params.ExtraParams
	}
	start := time.Now()
	native, headers, err := provider.complete(ctx, key, body, extra)
	if err != nil {
		return nil, err
	}
	requireWords := true
	if converted != nil {
		requireWords = converted.ExtraParams["word_timestamps"] == true && converted.ExtraParams["smart"] != true
		if vocabulary, ok := converted.ExtraParams["custom_vocabulary"].([]any); ok && len(vocabulary) > 0 {
			requireWords = false
		}
		if vocabulary, ok := converted.ExtraParams["custom_vocabulary"].([]string); ok && len(vocabulary) > 0 {
			requireWords = false
		}
	}
	result, decode := ToBifrostTranscriptionResponse(native, requireWords)
	if decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(decode.Error(), nil), native.ID)
	}
	result.ExtraFields = provider.extra(ctx, body, native, headers, start)
	return result, nil
}

func (provider *FastgenProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFastgenVideoGenerationRequest(request))
	})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	native, headers, err := provider.submit(ctx, key, body)
	if err != nil {
		return nil, err
	}
	if native.Model == "" {
		native.Model = request.Model
	}
	result, decode := ToBifrostVideoGenerationResponse(native, provider.networkConfig.BaseURL)
	if decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(decode.Error(), nil), native.ID)
	}
	result.ExtraFields = provider.extra(ctx, body, native, headers, start)
	return result, nil
}

func (provider *FastgenProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, invalidInput("video id is required")
	}
	id := providerUtils.StripVideoIDProviderSuffix(request.ID, schemas.Fastgen)
	start := time.Now()
	native, headers, err := provider.retrieve(ctx, key, id)
	if err != nil {
		return nil, err
	}
	result, decode := ToBifrostVideoGenerationResponse(native, provider.networkConfig.BaseURL)
	if decode != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(decode.Error(), nil), id)
	}
	result.ExtraFields = provider.extra(ctx, nil, native, headers, start)
	return result, nil
}

func (provider *FastgenProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	if request == nil || request.Variant != nil && *request.Variant != schemas.VideoDownloadVariantVideo {
		return nil, invalidInput("fastgen only supports the video download variant")
	}
	video, err := provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Fastgen, ID: request.ID})
	if err != nil {
		return nil, err
	}
	if video.Status != schemas.VideoStatusCompleted || len(video.Videos) != 1 || video.Videos[0].URL == nil {
		return nil, invalidInput("fastgen video is not ready")
	}
	body, mime, err := provider.download(ctx, *video.Videos[0].URL)
	if err != nil {
		return nil, err
	}
	return &schemas.BifrostVideoDownloadResponse{VideoID: video.ID, Content: body, ContentType: mime, ExtraFields: video.ExtraFields}, nil
}

func (provider *FastgenProvider) submit(ctx *schemas.BifrostContext, key schemas.Key, body []byte) (*FastgenGenerationResponse, map[string]string, *schemas.BifrostError) {
	data, headers, err := provider.request(ctx, key, http.MethodPost, "/api/v6/generations", body, true)
	if err != nil {
		return nil, nil, err
	}
	var native FastgenGenerationResponse
	if decode := sonic.Unmarshal(data, &native); decode != nil {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, decode), "")
	}
	if native.ID == "" {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fastgen omitted generation id", nil), "")
	}
	provider.receipt(ctx, &native, headers)
	return &native, headers, nil
}

func (provider *FastgenProvider) retrieve(ctx *schemas.BifrostContext, key schemas.Key, id string) (*FastgenGenerationResponse, map[string]string, *schemas.BifrostError) {
	escaped, err := providerUtils.EscapeResourceID(id, "generation id")
	if err != nil {
		return nil, nil, err
	}
	data, headers, err := provider.request(ctx, key, http.MethodGet, "/api/v6/generations/"+escaped, nil, false)
	if err != nil {
		return nil, nil, recoveryRequired(err, id)
	}
	var native FastgenGenerationResponse
	if decode := sonic.Unmarshal(data, &native); decode != nil {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, decode), id)
	}
	if native.ID != id {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fastgen generation id mismatch", nil), id)
	}
	if native.Status == "" {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fastgen omitted generation status", nil), id)
	}
	provider.receipt(ctx, &native, headers)
	return &native, headers, nil
}

func (provider *FastgenProvider) complete(ctx *schemas.BifrostContext, key schemas.Key, body []byte, options map[string]any) (*FastgenGenerationResponse, map[string]string, *schemas.BifrostError) {
	var native *FastgenGenerationResponse
	var headers map[string]string
	var err *schemas.BifrostError
	if resume, exists := options["resume_task_id"]; exists {
		id, ok := resume.(string)
		if !ok || id == "" {
			return nil, nil, invalidInput("resume_task_id must be a nonempty string")
		}
		native, headers, err = provider.retrieve(ctx, key, id)
	} else {
		native, headers, err = provider.submit(ctx, key, body)
	}
	if err != nil {
		return nil, nil, err
	}
	id := native.ID
	deadline := time.NewTimer(time.Duration(provider.networkConfig.DefaultRequestTimeoutInSeconds) * time.Second)
	defer deadline.Stop()
	for {
		switch native.Status {
		case "succeeded":
			return native, headers, nil
		case "failed":
			return nil, nil, recoveryRequired(&schemas.BifrostError{StatusCode: schemas.Ptr(502), Error: &schemas.ErrorField{Message: "fastgen generation failed: " + string(native.Error)}}, id)
		case "", "queued", "running":
		default:
			return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("unknown fastgen generation status: "+native.Status, nil), id)
		}
		timer := time.NewTimer(provider.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fastgen polling cancelled", ctx.Err()), id)
		case <-deadline.C:
			timer.Stop()
			return nil, nil, recoveryRequired(providerUtils.NewBifrostTimeoutError("fastgen polling timed out", nil), id)
		case <-timer.C:
		}
		native, headers, err = provider.retrieve(ctx, key, id)
		if err != nil {
			return nil, nil, err
		}
	}
}

func (provider *FastgenProvider) request(ctx *schemas.BifrostContext, key schemas.Key, method, path string, body []byte, paid bool) ([]byte, map[string]string, *schemas.BifrostError) {
	if method == http.MethodPost && len(body) == 0 {
		return nil, nil, invalidInput("fastgen requires a buffered native request body")
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + path)
	req.Header.SetMethod(method)
	req.Header.SetContentType("application/json")
	req.Header.Set("X-API-Key", key.Value.GetValue())
	req.SetBody(body)
	_, err, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if err != nil {
		if paid {
			err = recoveryRequired(err, "")
		}
		return nil, nil, err
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		err = parseFastgenError(resp)
		if paid && (resp.StatusCode() >= 500 || resp.StatusCode() == http.StatusRequestTimeout || resp.StatusCode() == http.StatusConflict) {
			err = recoveryRequired(err, "")
		}
		return nil, nil, err
	}
	data, decode := providerUtils.CheckAndDecodeBody(resp)
	if decode != nil {
		err = providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, decode)
		if paid {
			err = recoveryRequired(err, "")
		}
		return nil, nil, err
	}
	headers := providerUtils.ExtractProviderResponseHeaders(resp)
	if headers == nil {
		headers = map[string]string{}
	}
	return append([]byte(nil), data...), headers, nil
}

func (provider *FastgenProvider) download(ctx *schemas.BifrostContext, target string) ([]byte, string, *schemas.BifrostError) {
	if err := validateArtifact(target, provider.networkConfig.BaseURL); err != nil {
		return nil, "", invalidInput(err.Error())
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(target)
	req.Header.SetMethod(http.MethodGet)
	// Artifact URLs are public or signed. Never forward provider credentials to storage.
	_, err, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode() != 200 {
		return nil, "", providerUtils.NewBifrostOperationError(fmt.Sprintf("fastgen artifact download returned HTTP %d", resp.StatusCode()), nil)
	}
	content, decode := providerUtils.CheckAndDecodeBody(resp)
	if decode != nil {
		return nil, "", providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, decode)
	}
	return append([]byte(nil), content...), string(resp.Header.ContentType()), nil
}

func (provider *FastgenProvider) receipt(ctx *schemas.BifrostContext, native *FastgenGenerationResponse, headers map[string]string) {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["x-bf-provider-task-id"] = native.ID
	if native.Usage != nil && native.Usage.Credits != "" {
		headers["x-bf-provider-credits"] = string(native.Usage.Credits)
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, headers)
}

func (provider *FastgenProvider) extra(ctx *schemas.BifrostContext, body []byte, native any, headers map[string]string, start time.Time) schemas.BifrostResponseExtraFields {
	fields := schemas.BifrostResponseExtraFields{Provider: schemas.Fastgen, Latency: time.Since(start).Milliseconds(), RawResponse: native, ProviderResponseHeaders: headers}
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&fields, body)
	}
	return fields
}
