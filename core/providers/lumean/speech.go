package lumean

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

var ownerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ToLumeanSpeechRequest(request *schemas.BifrostSpeechRequest) (*LumeanSpeechRequest, error) {
	if request == nil || request.Input == nil || strings.TrimSpace(request.Input.Input) == "" || utf8.RuneCountInString(request.Input.Input) > 1000000 || request.Model == "" {
		return nil, fmt.Errorf("lumean requires model and input")
	}
	params := request.Params
	if params == nil {
		params = &schemas.SpeechParameters{}
	}
	if params.ResponseFormat != "" && params.ResponseFormat != "mp3" {
		return nil, fmt.Errorf("lumean supports mp3 output")
	}
	if params.Speed != nil || params.Instructions != "" || params.LanguageCode != nil || len(params.PronunciationDictionaryLocators) > 0 || params.OptimizeStreamingLatency != nil || params.EnableLogging != nil {
		return nil, fmt.Errorf("unsupported lumean speech parameters")
	}
	audio := request.Model == "music_v2" || request.Model == "eleven_text_to_sound_v2"
	template, owner := "", ""
	var seconds float64
	for k, v := range params.ExtraParams {
		switch k {
		case "template_id":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("template_id must be a string")
			}
			template = s
		case "public_owner_id":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("public_owner_id must be a string")
			}
			owner = s
		case "duration_seconds":
			switch n := v.(type) {
			case float64:
				seconds = n
			case float32:
				seconds = float64(n)
			case int:
				seconds = float64(n)
			case int64:
				seconds = float64(n)
			case json.Number:
				var err error
				seconds, err = n.Float64()
				if err != nil {
					return nil, fmt.Errorf("invalid duration_seconds")
				}
			default:
				return nil, fmt.Errorf("duration_seconds must be a number")
			}
		case "resume_task_id":
		default:
			return nil, fmt.Errorf("unsupported lumean speech parameter: %s", k)
		}
	}
	if audio {
		if template != "" || owner != "" || (params.VoiceConfig != nil && (params.VoiceConfig.Voice != nil || len(params.VoiceConfig.MultiVoiceConfig) > 0)) {
			return nil, fmt.Errorf("voice/template parameters do not apply to lumean audio generation")
		}
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < .5 || seconds > 30 || (request.Model == "music_v2" && seconds < 10) {
			return nil, fmt.Errorf("invalid lumean audio duration")
		}
		if request.Model == "music_v2" {
			return &LumeanSpeechRequest{TaskType: "music", TaskData: &LumeanTaskData{Prompt: request.Input.Input, NVariants: schemas.Ptr(1), ForceInstrumental: schemas.Ptr(true), MusicLengthMs: schemas.Ptr(int64(math.Round(seconds * 1000))), ModelID: request.Model}}, nil
		}
		return &LumeanSpeechRequest{TaskType: "sfx", TaskData: &LumeanTaskData{Text: request.Input.Input, DurationSeconds: &seconds, Loop: schemas.Ptr(false), OutputFormat: "mp3_44100_192"}}, nil
	}
	if _, err := uuid.Parse(template); err != nil || len(template) != 36 {
		return nil, fmt.Errorf("lumean speech requires a UUID template_id")
	}
	if owner != "" && !ownerIDPattern.MatchString(owner) {
		return nil, fmt.Errorf("invalid public_owner_id")
	}
	if params.VoiceConfig == nil || params.VoiceConfig.Voice == nil || len(params.VoiceConfig.MultiVoiceConfig) > 0 {
		return nil, fmt.Errorf("lumean speech requires one voice")
	}
	voice := strings.TrimSpace(*params.VoiceConfig.Voice)
	if voice == "" || len(voice) > 512 {
		return nil, fmt.Errorf("invalid lumean voice")
	}
	if seconds != 0 {
		return nil, fmt.Errorf("duration_seconds does not apply to lumean TTS")
	}
	return &LumeanSpeechRequest{TemplateID: template, InputText: request.Input.Input, ConfigOverride: &LumeanConfigOverride{TTSSettings: LumeanTTSSettings{ModelID: request.Model, VoiceID: voice, PublicOwnerID: owner}}}, nil
}

func (provider *LumeanProvider) catalog(ctx context.Context, key schemas.Key) ([]LumeanModel, *schemas.BifrostError) {
	models := []LumeanModel{{ID: "music_v2", Name: "Music v2"}, {ID: "eleven_text_to_sound_v2", Name: "Text to Sound v2"}}
	included := map[string]bool{"music_v2": true, "eleven_text_to_sound_v2": true}
	for _, service := range []string{"elevenlabs", "cartesia"} {
		var response LumeanModelsResponse
		if err := provider.json(ctx, key, "GET", "/templates/config-options?service="+service, nil, &response); err != nil {
			return nil, err
		}
		if response.Success != nil && !*response.Success || response.Data.TTSSettings.ModelID.Options == nil {
			return nil, providerUtils.NewBifrostOperationError("invalid lumean models response", nil)
		}
		for _, id := range response.Data.TTSSettings.ModelID.Options {
			if id == "" {
				return nil, providerUtils.NewBifrostOperationError("invalid lumean model", nil)
			}
			if !included[id] {
				models = append(models, LumeanModel{ID: id, Name: id})
				included[id] = true
			}
		}
	}
	return models, nil
}

func (provider *LumeanProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("speech request is required")
	}
	if _, raw := providerUtils.CheckAndGetRawRequestBody(ctx, request); raw {
		return nil, providerUtils.NewBifrostBadRequestError("raw lumean speech requests are not supported")
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
	wire, convErr := ToLumeanSpeechRequest(request)
	if convErr != nil {
		return nil, providerUtils.NewBifrostBadRequestError(convErr.Error())
	}
	limited, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	start := time.Now()
	var submitted LumeanOrderResponse
	if err := provider.json(limited, key, "POST", "/orders", wire, &submitted); err != nil {
		err.IsBifrostError = true
		err.AllowFallbacks = schemas.Ptr(false)
		err.ExtraFields.RawResponse = &LumeanReceipt{Status: "submission_unknown"}
		err.Error.Message += "; order submission outcome unknown; reconcile before submitting again"
		return nil, err
	}
	if submitted.Data.ID == "" {
		return nil, provider.pendingError(request.Model, "", "", "lumean accepted a request without an order ID; reconcile before submitting again")
	}
	provider.taskHeader(ctx, submitted.Data.ID)
	if submitted.ConfirmationRequired || submitted.Data.ConfirmationRequired {
		return nil, provider.pendingError(request.Model, submitted.Data.ID, "", "lumean order requires confirmation; automatic confirmation is disabled")
	}
	response, err := provider.completeSpeech(limited, key, request.Model, submitted.Data.ID)
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

// RetrieveSpeech fetches the native order again, renewing signed storage URLs.
// It never creates or confirms an order.
func (provider *LumeanProvider) RetrieveSpeech(ctx *schemas.BifrostContext, key schemas.Key, model, id string) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if strings.TrimSpace(id) == "" || len(id) > 512 {
		return nil, providerUtils.NewBifrostBadRequestError("invalid lumean order ID")
	}
	provider.taskHeader(ctx, id)
	limited, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	response, err := provider.completeSpeech(limited, key, model, id)
	if response != nil {
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, response.ExtraFields.ProviderResponseHeaders)
	}
	return response, err
}

func validStoragePath(path string) bool {
	if path == "" || len(path) > 4096 || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\:\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (provider *LumeanProvider) completeSpeech(ctx context.Context, key schemas.Key, model, id string) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	for {
		var wire LumeanOrderResponse
		if err := provider.json(ctx, key, "GET", "/orders/"+url.PathEscape(id), nil, &wire); err != nil {
			return nil, provider.pendingError(model, id, "", "lumean order retrieval failed; resume the existing order")
		}
		order := &wire.Data
		if order.ID != id {
			return nil, provider.pendingError(model, id, "", "lumean order ID mismatch")
		}
		if wire.ConfirmationRequired || order.ConfirmationRequired {
			return nil, provider.pendingError(model, id, "", "lumean order requires confirmation; automatic confirmation is disabled")
		}
		switch order.Status {
		case "completed", "result_delivered":
			if order.FilesExpired || len(order.Result.Files) != 1 || !validStoragePath(order.Result.Files[0]) {
				return nil, provider.pendingError(model, id, "", "lumean order has no single valid audio artifact")
			}
			var storage LumeanStorageResponse
			if err := provider.json(ctx, key, "POST", "/storage/url", &LumeanStorageRequest{Path: order.Result.Files[0]}, &storage); err != nil {
				return nil, provider.pendingError(model, id, "", "lumean storage URL retrieval failed; resume the existing order")
			}
			if !provider.artifactURL(storage.Data.URL) {
				return nil, provider.pendingError(model, id, "", "invalid lumean artifact origin")
			}
			audio, err := provider.do(ctx, key, "GET", storage.Data.URL, nil, false)
			if err != nil {
				return nil, provider.pendingError(model, id, "", "lumean audio download failed; resume to renew its storage URL")
			}
			if len(audio) == 0 {
				return nil, provider.pendingError(model, id, "", "lumean returned empty audio")
			}
			response := ToBifrostSpeechResponse(order, audio)
			if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
				response.ExtraFields.RawResponse = &wire
			}
			return response, nil
		case "failed", "cancelled", "compensated":
			err := provider.pendingError(model, id, "", "lumean order "+order.Status)
			err.ExtraFields.RawResponse = &LumeanReceipt{TaskID: id, Status: order.Status, ProviderCredits: order.TokensSpent}
			return nil, err
		case "created", "pending", "in_progress", "partially_completed":
		default:
			return nil, provider.pendingError(model, id, "", "unknown lumean order status; resume the existing order")
		}
		if !provider.wait(ctx) {
			return nil, provider.pendingError(model, id, "", "lumean order is still pending; resume with resume_task_id")
		}
	}
}

func ToBifrostSpeechResponse(order *LumeanOrder, audio []byte) *schemas.BifrostSpeechResponse {
	response := &schemas.BifrostSpeechResponse{Audio: audio, ExtraFields: schemas.BifrostResponseExtraFields{
		ProviderResponseHeaders: map[string]string{"x-bf-provider-task-id": order.ID},
		RawResponse:             &LumeanReceipt{TaskID: order.ID, Status: order.Status, ProviderCredits: order.TokensSpent, ServiceFiles: order.Result.ServiceFiles}}}
	if order.TokensSpent != nil {
		response.ExtraFields.ProviderResponseHeaders["x-bf-provider-credits"] = order.TokensSpent.String()
	}
	return response
}
