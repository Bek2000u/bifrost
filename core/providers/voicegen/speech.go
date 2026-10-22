package voicegen

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// ToVoicegenSpeechRequest preserves the stable client ID across retries and keeps
// a source-prefixed voice identifier intact.
func ToVoicegenSpeechRequest(request *schemas.BifrostSpeechRequest, requestID string) (*VoicegenSpeechRequest, error) {
	if request == nil || request.Input == nil || strings.TrimSpace(request.Input.Input) == "" || utf8.RuneCountInString(request.Input.Input) > 1000000 || request.Model == "" || requestID == "" {
		return nil, fmt.Errorf("voicegen requires model, text and a stable request ID")
	}
	params := request.Params
	if params == nil || params.VoiceConfig == nil || params.VoiceConfig.Voice == nil || len(params.VoiceConfig.MultiVoiceConfig) > 0 {
		return nil, fmt.Errorf("voicegen requires one voice")
	}
	voice := strings.TrimSpace(*params.VoiceConfig.Voice)
	if voice == "" || len(voice) > 512 {
		return nil, fmt.Errorf("invalid voicegen voice")
	}
	if params.ResponseFormat != "" && params.ResponseFormat != "mp3" {
		return nil, fmt.Errorf("voicegen supports mp3 output")
	}
	if params.Instructions != "" || params.LanguageCode != nil || len(params.PronunciationDictionaryLocators) > 0 || params.OptimizeStreamingLatency != nil || params.EnableLogging != nil {
		return nil, fmt.Errorf("unsupported voicegen speech parameters")
	}
	source, preset := "auto", ""
	for k, v := range params.ExtraParams {
		switch k {
		case "voice_source":
			value, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("voice_source must be a string")
			}
			source = value
		case "settings_preset":
			value, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("settings_preset must be a string")
			}
			preset = value
		case "resume_task_id":
		default:
			return nil, fmt.Errorf("unsupported voicegen speech parameter: %s", k)
		}
	}
	if source != "auto" && source != "cartesia" && source != "elevenlabs" {
		return nil, fmt.Errorf("invalid voice_source")
	}
	if prefix, _, ok := strings.Cut(voice, ":"); ok && (prefix == "cartesia" || prefix == "elevenlabs") && source != "auto" && prefix != source {
		return nil, fmt.Errorf("voice prefix conflicts with voice_source")
	}
	if preset != "" && preset != "standard" && preset != "stable" && preset != "expressive" && preset != "fast" {
		return nil, fmt.Errorf("invalid settings_preset")
	}
	if preset != "" && params.Speed != nil {
		return nil, fmt.Errorf("settings_preset and speed cannot be combined")
	}
	mac := hmac.New(sha256.New, []byte("voicegen/client-task-id/v1"))
	_, _ = mac.Write([]byte(requestID))
	sum := mac.Sum(nil)
	encoded := hex.EncodeToString(sum)
	clientID := fmt.Sprintf("%s-%s-8%s-%x%s-%s", encoded[:8], encoded[8:12], encoded[13:16], (sum[8]>>4&3)|8, encoded[17:20], encoded[20:32])
	result := &VoicegenSpeechRequest{Text: request.Input.Input, VoiceID: voice, VoiceSource: source, ModelID: request.Model,
		VoiceEngine: "cartesia", ClientTaskID: clientID, AutoStart: true, IncludeTimestamps: true, TimestampsGranularity: "word", SettingsPreset: preset}
	if preset == "" {
		speed := 1.0
		if params.Speed != nil {
			speed = *params.Speed
		}
		if !((speed >= .6 && speed <= 1.5) || (speed >= 60 && speed <= 150)) {
			return nil, fmt.Errorf("voicegen speed is outside its supported range")
		}
		if speed > 2 {
			speed /= 100
		}
		result.VoiceEngineSettings = &VoicegenSettings{Speed: speed}
	}
	return result, nil
}

func (provider *VoicegenProvider) catalog(ctx context.Context, key schemas.Key) ([]VoicegenModel, *schemas.BifrostError) {
	var response VoicegenModelsResponse
	if err := provider.json(ctx, key, "GET", "/api/v1/client/models", nil, &response); err != nil {
		return nil, err
	}
	if response.Models == nil {
		return nil, providerUtils.NewBifrostOperationError("invalid voicegen models response", nil)
	}
	for _, model := range response.Models {
		if model.ID == "" {
			return nil, providerUtils.NewBifrostOperationError("invalid voicegen model", nil)
		}
	}
	return response.Models, nil
}

func (provider *VoicegenProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("speech request is required")
	}
	if _, raw := providerUtils.CheckAndGetRawRequestBody(ctx, request); raw {
		return nil, providerUtils.NewBifrostBadRequestError("raw voicegen speech requests are not supported")
	}
	if request.Params != nil {
		if value, ok := request.Params.ExtraParams["resume_task_id"]; ok {
			id, valid := value.(string)
			if !valid || strings.TrimSpace(id) == "" {
				return nil, providerUtils.NewBifrostBadRequestError("resume_task_id must be a nonempty string")
			}
			return provider.RetrieveSpeech(ctx, key, request.Model, id)
		}
	}
	requestID, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string)
	wire, convErr := ToVoicegenSpeechRequest(request, requestID)
	if convErr != nil {
		return nil, providerUtils.NewBifrostBadRequestError(convErr.Error())
	}
	limited, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	start := time.Now()
	var submitted VoicegenTaskResponse
	if err := provider.json(limited, key, "POST", "/api/v1/client/tasks", wire, &submitted); err != nil {
		err.IsBifrostError = true
		err.AllowFallbacks = schemas.Ptr(false)
		err.ExtraFields.RawResponse = &VoicegenReceipt{ClientTaskID: wire.ClientTaskID, Status: "submission_unknown"}
		err.Error.Message += "; submission outcome unknown; client_task_id=" + wire.ClientTaskID
		return nil, err
	}
	task := submitted.Task
	if task == nil {
		task = &submitted.VoicegenTask
	}
	if task.TaskID == "" {
		return nil, provider.pendingError(request.Model, "", wire.ClientTaskID, "voicegen accepted a request without a task ID; reconcile before submitting again")
	}
	provider.taskHeader(ctx, task.TaskID)
	response, err := provider.completeSpeech(limited, key, request.Model, task.TaskID, wire.ClientTaskID)
	if err != nil {
		return nil, err
	}
	response.ExtraFields.Latency = time.Since(start).Milliseconds()
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, response.ExtraFields.ProviderResponseHeaders)
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		body, _ := providerUtils.MarshalSorted(wire)
		providerUtils.ParseAndSetRawRequest(&response.ExtraFields, body)
	}
	return response, nil
}

// RetrieveSpeech resumes a previously submitted native task without creating one.
func (provider *VoicegenProvider) RetrieveSpeech(ctx *schemas.BifrostContext, key schemas.Key, model, id string) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if strings.TrimSpace(id) == "" || len(id) > 512 {
		return nil, providerUtils.NewBifrostBadRequestError("invalid voicegen task ID")
	}
	provider.taskHeader(ctx, id)
	limited, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	response, err := provider.completeSpeech(limited, key, model, id, "")
	if response != nil {
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, response.ExtraFields.ProviderResponseHeaders)
	}
	return response, err
}

func (provider *VoicegenProvider) completeSpeech(ctx context.Context, key schemas.Key, model, id, clientID string) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	path := "/api/v1/client/tasks/" + url.PathEscape(id)
	for {
		var wire VoicegenTaskResponse
		if err := provider.json(ctx, key, "GET", path, nil, &wire); err != nil {
			return nil, provider.pendingError(model, id, clientID, "voicegen task retrieval failed; resume the existing task")
		}
		task := wire.Task
		if task == nil {
			task = &wire.VoicegenTask
		}
		if task.TaskID != id {
			return nil, provider.pendingError(model, id, clientID, "voicegen task ID mismatch")
		}
		switch task.Status {
		case "done":
			if !task.TimestampsComplete {
				break
			}
			target := task.PublicDownloadURL
			if target == "" {
				target = task.DownloadURL
			}
			authenticate := false
			if target == "" {
				target = provider.networkConfig.BaseURL + path + "/download"
				authenticate = true
			}
			if !provider.artifactURL(target) {
				return nil, provider.pendingError(model, id, clientID, "invalid voicegen artifact origin")
			}
			base, _ := url.Parse(provider.networkConfig.BaseURL)
			artifact, _ := url.Parse(target)
			if strings.HasPrefix(artifact.Path, "/api/v1/client/tasks/") {
				if artifact.Host != base.Host || artifact.Scheme != base.Scheme || artifact.Path != path+"/download" {
					return nil, provider.pendingError(model, id, clientID, "voicegen artifact belongs to another task")
				}
				authenticate = task.PublicDownloadURL == ""
			}
			folder := "/downloads/tasks/" + url.PathEscape(id)
			if strings.HasPrefix(artifact.Path, "/downloads/tasks/") && artifact.Path != folder && !strings.HasPrefix(artifact.Path, folder+"/") {
				return nil, provider.pendingError(model, id, clientID, "voicegen artifact belongs to another task")
			}
			if task.TimestampsURL == "" || task.TimestampsFormat != "srt" || task.TimestampsGranularity != "word" || !provider.artifactURL(task.TimestampsURL) {
				return nil, provider.pendingError(model, id, clientID, "invalid voicegen timestamps origin")
			}
			timestamps, _ := url.Parse(task.TimestampsURL)
			if strings.HasPrefix(timestamps.Path, "/api/v1/client/tasks/") || (strings.HasPrefix(timestamps.Path, "/downloads/tasks/") && timestamps.Path != folder && !strings.HasPrefix(timestamps.Path, folder+"/")) {
				return nil, provider.pendingError(model, id, clientID, "voicegen timestamps belong to another task")
			}
			audio, err := provider.do(ctx, key, "GET", target, nil, authenticate)
			if err != nil {
				return nil, provider.pendingError(model, id, clientID, "voicegen audio download failed; resume the existing task")
			}
			if len(audio) == 0 {
				return nil, provider.pendingError(model, id, clientID, "voicegen returned empty audio")
			}
			result := ToBifrostSpeechResponse(task, audio, clientID)
			if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
				result.ExtraFields.RawResponse = &wire
			}
			return result, nil
		case "error", "cancelled":
			err := provider.pendingError(model, id, clientID, "voicegen task "+task.Status)
			err.ExtraFields.RawResponse = &VoicegenReceipt{TaskID: id, ClientTaskID: clientID, Status: task.Status}
			return nil, err
		case "queued", "running":
		default:
			return nil, provider.pendingError(model, id, clientID, "unknown voicegen task status; resume the existing task")
		}
		if !provider.wait(ctx) {
			return nil, provider.pendingError(model, id, clientID, "voicegen task is still pending; resume with resume_task_id")
		}
	}
}

func ToBifrostSpeechResponse(task *VoicegenTask, audio []byte, clientID string) *schemas.BifrostSpeechResponse {
	receipt := &VoicegenReceipt{TaskID: task.TaskID, ClientTaskID: clientID, Status: task.Status, TimestampsURL: task.TimestampsURL, Characters: task.ResultCharsCount}
	response := &schemas.BifrostSpeechResponse{Audio: audio, ExtraFields: schemas.BifrostResponseExtraFields{RawResponse: receipt, ProviderResponseHeaders: map[string]string{"x-bf-provider-task-id": task.TaskID}}}
	if task.ResultCharsCount != nil && *task.ResultCharsCount >= 0 && *task.ResultCharsCount <= 1000000 {
		response.Usage = &schemas.SpeechUsage{InputChars: *task.ResultCharsCount}
		response.ExtraFields.ProviderResponseHeaders["x-bf-provider-characters"] = strconv.Itoa(*task.ResultCharsCount)
	}
	return response
}
