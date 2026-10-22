// Package fal implements fal.ai's native queued media API.
package fal

import (
	"encoding/base64"
	"fmt"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

type FalProvider struct {
	client              *fasthttp.Client
	streamingClient     *fasthttp.Client
	networkConfig       schemas.NetworkConfig
	logger              schemas.Logger
	sendBackRawRequest  bool
	sendBackRawResponse bool
	pollInterval        time.Duration
	storageBaseURL      string
	platformBaseURL     string
}

var _ schemas.Provider = (*FalProvider)(nil)

func NewFalProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*FalProvider, error) {
	config.CheckAndSetDefaults()
	timeout := time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds) * time.Second
	client := &fasthttp.Client{ReadTimeout: timeout, WriteTimeout: timeout, MaxConnsPerHost: config.NetworkConfig.MaxConnsPerHost, MaxIdleConnDuration: time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds) * time.Second, MaxConnWaitTimeout: timeout, MaxConnDuration: time.Duration(schemas.DefaultMaxConnDurationInSeconds) * time.Second, ConnPoolStrategy: fasthttp.FIFO, MaxIdemponentCallAttempts: 1}
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	// ConfigureDialer's stale-connection callback must not repeat a paid POST.
	client.MaxIdemponentCallAttempts = 1
	storageBaseURL := "https://rest.fal.ai"
	platformBaseURL := "https://api.fal.ai/v1"
	if config.NetworkConfig.BaseURL == "" || config.NetworkConfig.BaseURL == "https://fal.run" {
		config.NetworkConfig.BaseURL = "https://queue.fal.run"
	} else if config.NetworkConfig.BaseURL != "https://queue.fal.run" {
		storageBaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")
		platformBaseURL = storageBaseURL + "/v1"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")
	return &FalProvider{client: client, streamingClient: providerUtils.BuildStreamingClient(client), networkConfig: config.NetworkConfig, logger: logger, sendBackRawRequest: config.SendBackRawRequest, sendBackRawResponse: config.SendBackRawResponse, pollInterval: time.Second, storageBaseURL: storageBaseURL, platformBaseURL: platformBaseURL}, nil
}
func (provider *FalProvider) GetProviderKey() schemas.ModelProvider { return schemas.Fal }

func (provider *FalProvider) ImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("image request is required")
	}
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFalImageGenerationRequest(request))
	})
	if err != nil {
		return nil, err
	}
	var options map[string]any
	format := "url"
	if request.Params != nil {
		options = request.Params.ExtraParams
		if request.Params.ResponseFormat != nil {
			format = *request.Params.ResponseFormat
		}
	}
	return provider.image(ctx, key, request.Model, body, options, format)
}
func (provider *FalProvider) ImageEdit(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("image edit request is required")
	}
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFalImageEditRequest(request))
	})
	if err != nil {
		return nil, err
	}
	var options map[string]any
	format := "url"
	if request.Params != nil {
		options = request.Params.ExtraParams
		if request.Params.ResponseFormat != nil {
			format = *request.Params.ResponseFormat
		}
	}
	return provider.image(ctx, key, request.Model, body, options, format)
}
func (provider *FalProvider) image(ctx *schemas.BifrostContext, key schemas.Key, model string, body []byte, options map[string]any, format string) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	start := time.Now()
	native, id, headers, err := provider.complete(ctx, key, model, body, options)
	if err != nil {
		return nil, err
	}
	result, conversion := ToBifrostImageGenerationResponse(native, id, model)
	if conversion != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(conversion.Error(), nil), id)
	}
	if format == "b64_json" {
		for index := range result.Data {
			if result.Data[index].URL == "" {
				continue
			}
			content, _, err := provider.download(ctx, result.Data[index].URL)
			if err != nil {
				return nil, recoveryRequired(err, id)
			}
			result.Data[index] = schemas.ImageData{B64JSON: base64.StdEncoding.EncodeToString(content)}
		}
	}
	cost, _, events := provider.billing(ctx, key, model, id, headers)
	if cost != nil {
		result.Usage = &schemas.ImageUsage{Cost: cost}
		native.BillingEvents = events
	}
	result.ExtraFields = provider.extra(ctx, body, native, headers, start)
	return result, nil
}

func (provider *FalProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("speech request is required")
	}
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFalSpeechRequest(request))
	})
	if err != nil {
		return nil, err
	}
	var options map[string]any
	if request.Params != nil {
		options = request.Params.ExtraParams
	}
	start := time.Now()
	native, id, headers, err := provider.complete(ctx, key, request.Model, body, options)
	if err != nil {
		return nil, err
	}
	target := native.AudioURL
	if native.Audio != nil {
		target = native.Audio.URL
	}
	if target == "" {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal audio URL is missing", nil), id)
	}
	content, contentType, err := provider.download(ctx, target)
	if err != nil {
		return nil, recoveryRequired(err, id)
	}
	if contentType != "" {
		headers["content-type"] = contentType
	}
	if native.Audio != nil && native.Audio.ContentType != "" {
		headers["content-type"] = native.Audio.ContentType
	}
	var usage *schemas.SpeechUsage
	if !audioGeneration(request.Model) && request.Input != nil {
		usage = &schemas.SpeechUsage{InputChars: utf8.RuneCountInString(request.Input.Input)}
	}
	cost, billedSeconds, events := provider.billing(ctx, key, request.Model, id, headers)
	duration := nativeDuration(native)
	if duration == nil {
		duration = billedSeconds
	}
	if cost != nil || duration != nil {
		if usage == nil {
			usage = &schemas.SpeechUsage{}
		}
		usage.Cost = cost
	}
	if duration != nil {
		headers["x-bf-fal-audio-seconds"] = strconv.FormatFloat(*duration, 'f', -1, 64)
		if math.Trunc(*duration) == *duration && *duration < float64(int(^uint(0)>>1)) {
			usage.AudioSeconds = int(*duration)
		}
	}
	native.BillingEvents = events
	return &schemas.BifrostSpeechResponse{Audio: content, Usage: usage, ExtraFields: provider.extra(ctx, body, native, headers, start)}, nil
}

func (provider *FalProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("transcription request is required")
	}
	var options map[string]any
	if request.Params != nil {
		options = request.Params.ExtraParams
	}
	start := time.Now()
	audioURL, _ := options["audio_url"].(string)
	_, raw := providerUtils.CheckAndGetRawRequestBody(ctx, request)
	if audioURL == "" && !raw && options["resume_task_id"] == nil {
		var err *schemas.BifrostError
		audioURL, err = provider.uploadAudio(ctx, key, request)
		if err != nil {
			return nil, err
		}
	}
	if options["resume_task_id"] != nil && audioURL == "" {
		audioURL = "resumed-request"
	}
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFalTranscriptionRequest(request, audioURL))
	})
	if err != nil {
		return nil, err
	}
	native, id, headers, err := provider.complete(ctx, key, request.Model, body, options)
	if err != nil {
		return nil, err
	}
	result, conversion := ToBifrostTranscriptionResponse(native)
	if conversion != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(conversion.Error(), nil), id)
	}
	cost, billedSeconds, events := provider.billing(ctx, key, request.Model, id, headers)
	if result.Usage == nil && (cost != nil || billedSeconds != nil) {
		result.Usage = &schemas.TranscriptionUsage{}
	}
	if result.Usage != nil {
		result.Usage.Cost = cost
		if billedSeconds != nil {
			result.Usage.Type = "duration"
			result.Usage.Seconds = billedSeconds
		}
	}
	native.BillingEvents = events
	result.ExtraFields = provider.extra(ctx, body, native, headers, start)
	return result, nil
}

func (provider *FalProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("video request is required")
	}
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return asRequest(ToFalVideoGenerationRequest(request))
	})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	native, headers, err := provider.submit(ctx, key, request.Model, body)
	if err != nil {
		return nil, err
	}
	result, conversion := ToBifrostVideoGenerationResponse(native, request.Model)
	if conversion != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(conversion.Error(), nil), native.RequestID)
	}
	result.ExtraFields = provider.extra(ctx, body, native, headers, start)
	return result, nil
}
func (provider *FalProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("video id is required")
	}
	reference, conversion := decodeJobID(request.ID)
	if conversion != nil {
		return nil, providerUtils.NewBifrostBadRequestError(conversion.Error())
	}
	start := time.Now()
	native, headers, err := provider.status(ctx, key, reference.Model, reference.RequestID)
	if err != nil {
		return nil, err
	}
	result, conversion := ToBifrostVideoGenerationResponse(native, reference.Model)
	if conversion != nil {
		return nil, recoveryRequired(providerUtils.NewBifrostOperationError(conversion.Error(), nil), reference.RequestID)
	}
	if result.Status == schemas.VideoStatusCompleted {
		output, outputHeaders, err := provider.result(ctx, key, reference.Model, reference.RequestID)
		if err != nil {
			if err.StatusCode == nil || (*err.StatusCode != 400 && *err.StatusCode != 422) {
				return nil, err
			}
			result.Status = schemas.VideoStatusFailed
			result.Error = &schemas.VideoCreateError{Code: "fal_generation_failed", Message: err.Error.Message}
		} else {
			target := output.VideoURL
			mime := "video/mp4"
			if output.Video != nil {
				target = output.Video.URL
				if output.Video.ContentType != "" {
					mime = output.Video.ContentType
				}
			}
			if target == "" {
				return nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal video URL is missing", nil), reference.RequestID)
			}
			result.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: &target, ContentType: mime}}
			for k, v := range outputHeaders {
				headers[k] = v
			}
			if output.Video != nil && output.Video.Duration != nil {
				result.Seconds = schemas.Ptr(strconv.FormatFloat(*output.Video.Duration, 'f', -1, 64))
			}
		}
	}
	if result.Status == schemas.VideoStatusCompleted || result.Status == schemas.VideoStatusFailed {
		cost, _, _ := provider.billing(ctx, key, reference.Model, reference.RequestID, headers)
		if cost != nil {
			result.Usage = &schemas.VideoUsage{Cost: cost}
		}
	}
	result.ExtraFields = provider.extra(ctx, nil, native, headers, start)
	return result, nil
}
func (provider *FalProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("video download request is required")
	}
	if request.Variant != nil && *request.Variant != schemas.VideoDownloadVariantVideo {
		return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDownloadRequest, schemas.Fal)
	}
	video, err := provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Fal, ID: request.ID})
	if err != nil {
		return nil, err
	}
	if video.Status != schemas.VideoStatusCompleted || len(video.Videos) == 0 {
		return nil, providerUtils.NewBifrostBadRequestError("fal video is not ready")
	}
	content, mime, err := provider.download(ctx, *video.Videos[0].URL)
	if err != nil {
		return nil, err
	}
	return &schemas.BifrostVideoDownloadResponse{VideoID: video.ID, Content: content, ContentType: mime, ExtraFields: video.ExtraFields}, nil
}

func (provider *FalProvider) submit(ctx *schemas.BifrostContext, key schemas.Key, model string, body []byte) (*FalQueueResponse, map[string]string, *schemas.BifrostError) {
	path, _, conversion := endpointPaths(model)
	if conversion != nil {
		return nil, nil, providerUtils.NewBifrostBadRequestError(conversion.Error())
	}
	if len(body) == 0 {
		return nil, nil, providerUtils.NewBifrostBadRequestError("fal requires a buffered native request body")
	}
	data, headers, err := provider.request(ctx, key, http.MethodPost, provider.networkConfig.BaseURL+path, body, "application/json", true, true)
	if err != nil {
		return nil, nil, err
	}
	var native FalQueueResponse
	if conversion := sonic.Unmarshal(data, &native); conversion != nil {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, conversion), "")
	}
	if native.RequestID == "" {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal omitted request_id", nil), "")
	}
	provider.receipt(ctx, native.RequestID, headers)
	return &native, headers, nil
}
func (provider *FalProvider) status(ctx *schemas.BifrostContext, key schemas.Key, model, id string) (*FalQueueResponse, map[string]string, *schemas.BifrostError) {
	path, err := jobPath(model, id)
	if err != nil {
		return nil, nil, providerUtils.NewBifrostBadRequestError(err.Error())
	}
	data, headers, bErr := provider.request(ctx, key, http.MethodGet, provider.networkConfig.BaseURL+path+"/status", nil, "application/json", true, false)
	if bErr != nil {
		return nil, nil, recoveryRequired(bErr, id)
	}
	var native FalQueueResponse
	if err := sonic.Unmarshal(data, &native); err != nil {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err), id)
	}
	if native.RequestID != "" && native.RequestID != id {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal request_id mismatch", nil), id)
	}
	if native.Status == "" {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal omitted queue status", nil), id)
	}
	native.RequestID = id
	provider.receipt(ctx, id, headers)
	return &native, headers, nil
}
func (provider *FalProvider) result(ctx *schemas.BifrostContext, key schemas.Key, model, id string) (*FalResult, map[string]string, *schemas.BifrostError) {
	path, conversion := jobPath(model, id)
	if conversion != nil {
		return nil, nil, providerUtils.NewBifrostBadRequestError(conversion.Error())
	}
	data, headers, err := provider.request(ctx, key, http.MethodGet, provider.networkConfig.BaseURL+path, nil, "application/json", true, false)
	if err != nil {
		return nil, nil, recoveryRequired(err, id)
	}
	var native FalResult
	if conversion := sonic.Unmarshal(data, &native); conversion != nil {
		return nil, nil, recoveryRequired(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, conversion), id)
	}
	provider.receipt(ctx, id, headers)
	return &native, headers, nil
}
func (provider *FalProvider) complete(ctx *schemas.BifrostContext, key schemas.Key, model string, body []byte, options map[string]any) (*FalResult, string, map[string]string, *schemas.BifrostError) {
	id := ""
	if resume, exists := options["resume_task_id"]; exists {
		var ok bool
		id, ok = resume.(string)
		if !ok || id == "" {
			return nil, "", nil, providerUtils.NewBifrostBadRequestError("resume_task_id must be a nonempty string")
		}
	} else {
		native, _, err := provider.submit(ctx, key, model, body)
		if err != nil {
			return nil, "", nil, err
		}
		id = native.RequestID
	}
	deadline := time.NewTimer(time.Duration(provider.networkConfig.DefaultRequestTimeoutInSeconds) * time.Second)
	defer deadline.Stop()
	for {
		state, _, err := provider.status(ctx, key, model, id)
		if err != nil {
			return nil, id, nil, err
		}
		if state.Error != "" {
			return nil, id, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal generation failed: "+state.Error, nil), id)
		}
		switch state.Status {
		case "COMPLETED":
			native, headers, err := provider.result(ctx, key, model, id)
			return native, id, headers, err
		case "IN_QUEUE", "IN_PROGRESS":
		default:
			return nil, id, nil, recoveryRequired(providerUtils.NewBifrostOperationError("unknown fal queue status: "+state.Status, nil), id)
		}
		timer := time.NewTimer(provider.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, id, nil, recoveryRequired(providerUtils.NewBifrostOperationError("fal polling cancelled", ctx.Err()), id)
		case <-deadline.C:
			timer.Stop()
			return nil, id, nil, recoveryRequired(providerUtils.NewBifrostTimeoutError("fal polling timed out", nil), id)
		case <-timer.C:
		}
	}
}

func jobPath(model, id string) (string, error) {
	_, base, err := endpointPaths(model)
	if err != nil {
		return "", err
	}
	escaped, bErr := providerUtils.EscapeResourceID(id, "fal request_id")
	if bErr != nil {
		return "", fmt.Errorf("invalid fal request_id")
	}
	return base + "/requests/" + escaped, nil
}

func (provider *FalProvider) uploadAudio(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (string, *schemas.BifrostError) {
	if request.Input == nil || len(request.Input.File) == 0 {
		return "", providerUtils.NewBifrostBadRequestError("fal transcription requires file bytes or audio_url")
	}
	filename := request.Input.Filename
	if filename == "" {
		filename = "audio.mp3"
	}
	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType == "" {
		contentType = http.DetectContentType(request.Input.File)
	}
	if request.Params != nil && request.Params.Format != nil {
		format := *request.Params.Format
		if strings.Contains(format, "/") {
			contentType = format
		} else if detected := mime.TypeByExtension("." + format); detected != "" {
			contentType = detected
		}
	}
	body, conversion := providerUtils.MarshalSorted(FalUploadRequest{ContentType: contentType, FileName: filename})
	if conversion != nil {
		return "", providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, conversion)
	}
	data, _, err := provider.request(ctx, key, http.MethodPost, provider.storageBaseURL+"/storage/upload/initiate?storage_type=fal-cdn-v3", body, "application/json", true, false)
	if err != nil {
		return "", err
	}
	var upload FalUploadResponse
	if conversion := sonic.Unmarshal(data, &upload); conversion != nil {
		return "", providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, conversion)
	}
	if upload.FileURL == "" || downloadURL(upload.UploadURL) != nil || downloadURL(upload.FileURL) != nil {
		return "", providerUtils.NewBifrostOperationError("fal upload receipt is invalid", nil)
	}
	_, _, err = provider.request(ctx, schemas.Key{}, http.MethodPut, upload.UploadURL, request.Input.File, contentType, false, false)
	if err != nil {
		return "", err
	}
	return upload.FileURL, nil
}

func (provider *FalProvider) request(ctx *schemas.BifrostContext, key schemas.Key, method, target string, body []byte, contentType string, authenticate, paid bool) ([]byte, map[string]string, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	if authenticate {
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.Header.Set("Authorization", "Key "+key.Value.GetValue())
	}
	req.SetRequestURI(target)
	req.Header.SetMethod(method)
	if contentType != "" {
		req.Header.SetContentType(contentType)
	}
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
		err = parseFalError(resp)
		if paid && (resp.StatusCode() >= 500 || resp.StatusCode() == 408 || resp.StatusCode() == 409) {
			err = recoveryRequired(err, "")
		}
		return nil, nil, err
	}
	data, conversion := providerUtils.CheckAndDecodeBody(resp)
	if conversion != nil {
		err = providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, conversion)
		if paid {
			err = recoveryRequired(err, "")
		}
		return nil, nil, err
	}
	headers := providerUtils.ExtractProviderResponseHeaders(resp)
	if headers == nil {
		headers = map[string]string{}
	}
	if !authenticate && len(resp.Header.ContentType()) > 0 {
		headers["content-type"] = string(resp.Header.ContentType())
	}
	return append([]byte(nil), data...), headers, nil
}
func (provider *FalProvider) download(ctx *schemas.BifrostContext, target string) ([]byte, string, *schemas.BifrostError) {
	if err := downloadURL(target); err != nil {
		return nil, "", providerUtils.NewBifrostOperationError(err.Error(), nil)
	}
	data, headers, err := provider.request(ctx, schemas.Key{}, http.MethodGet, target, nil, "", false, false)
	return data, headers["content-type"], err
}
func (provider *FalProvider) receipt(ctx *schemas.BifrostContext, id string, headers map[string]string) {
	headers["x-bf-provider-task-id"] = id
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, headers)
}
func (provider *FalProvider) extra(ctx *schemas.BifrostContext, body []byte, native any, headers map[string]string, start time.Time) schemas.BifrostResponseExtraFields {
	fields := schemas.BifrostResponseExtraFields{Provider: schemas.Fal, RawResponse: native, ProviderResponseHeaders: headers, Latency: time.Since(start).Milliseconds()}
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&fields, body)
	}
	return fields
}
