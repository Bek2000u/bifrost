package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fasthttp/router"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configtables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type voiceRouteAccount struct{ base string }

func (a voiceRouteAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Fastgen}, nil
}
func (a voiceRouteAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "native-voice-key", Value: *schemas.NewSecretVar("native-voice-secret"), Models: schemas.WhiteList{"*"}, Weight: 1}}, nil
}
func (a voiceRouteAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	config := &schemas.ProviderConfig{NetworkConfig: schemas.DefaultNetworkConfig, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}
	config.NetworkConfig.BaseURL = a.base
	config.NetworkConfig.AllowPrivateNetwork = true
	return config, nil
}

func TestVoiceRoutesUseNativeAuthAndReadOnlySamples(t *testing.T) {
	SetLogger(&mockLogger{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Errorf("voice read submitted %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v6/voices":
			if r.Header.Get("X-API-Key") != "native-voice-secret" || r.URL.Query().Get("model") != "aistudio/gemini-2.5-flash-preview-tts" {
				t.Error("native credential/model lost")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"id":"Zephyr","name":"Zephyr","provider":"aistudio","models":["aistudio/gemini-2.5-flash-preview-tts"],"preview_urls":{"en":"/sample.wav"}}]`)
		case "/sample.wav":
			if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Error("provider credential leaked to preview download")
			}
			w.Header().Set("Content-Type", "audio/wav")
			fmt.Fprint(w, "RIFF-native-preview")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	logger := bifrost.NewNoOpLogger()
	plugin, err := governance.Init(t.Context(), &governance.Config{IsVkMandatory: new(true)}, logger, nil,
		&configstore.GovernanceConfig{VirtualKeys: []configtables.TableVirtualKey{{
			ID: "voice-vk", Name: "voice-vk", Value: *schemas.NewSecretVar("sk-bf-voices"), IsActive: new(true),
			ProviderConfigs: []configtables.TableVirtualKeyProviderConfig{{Provider: "fastgen", AllowedModels: schemas.WhiteList{"*"}, AllowAllKeys: true}},
		}}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := bifrost.Init(t.Context(), schemas.BifrostConfig{Account: voiceRouteAccount{upstream.URL}, Logger: logger, LLMPlugins: []schemas.LLMPlugin{plugin}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown()
	r := router.New()
	(&CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{EnforceAuthOnInference: true}}}).RegisterRoutes(r)
	get := func(path, key string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI(path)
		if key != "" {
			ctx.Request.Header.Set("Authorization", "Bearer "+key)
		}
		r.Handler(ctx)
		return ctx
	}
	path := "/v1/audio/voices?provider=fastgen&model=aistudio%2Fgemini-2.5-flash-preview-tts"
	denied := get(path, "")
	if denied.Response.StatusCode() < 400 || denied.Response.StatusCode() >= 500 || calls.Load() != 0 {
		t.Fatalf("anonymous voice request = %d; upstream calls=%d", denied.Response.StatusCode(), calls.Load())
	}
	response := get(path, "sk-bf-voices")
	if response.Response.StatusCode() != 200 {
		t.Fatalf("catalog = %d: %s", response.Response.StatusCode(), response.Response.Body())
	}
	var catalog struct {
		Data []struct {
			ID         string `json:"id"`
			PreviewURL string `json:"preview_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Response.Body(), &catalog); err != nil || len(catalog.Data) != 1 || catalog.Data[0].ID != "Zephyr" {
		t.Fatalf("wrong voice catalog: %s", response.Response.Body())
	}
	preview := get(catalog.Data[0].PreviewURL, "sk-bf-voices")
	if preview.Response.StatusCode() != 200 || string(preview.Response.Body()) != "RIFF-native-preview" || !strings.HasPrefix(string(preview.Response.Header.ContentType()), "audio/wav") {
		t.Fatalf("preview = %d: %s", preview.Response.StatusCode(), preview.Response.Body())
	}
}

func TestPrepareVoiceRequestScopesModel(t *testing.T) {
	for _, query := range []string{"model=fastgen/aistudio/gemini", "provider=fastgen&model=aistudio/gemini"} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/v1/audio/voices?" + query)
		req, err := prepareVoiceRequest(ctx)
		if err != nil || req.Provider != schemas.Fastgen || req.Model != "aistudio/gemini" {
			t.Fatalf("%s: %+v %v", query, req, err)
		}
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/v1/audio/voices?provider=voicegen&model=fastgen/model")
	if _, err := prepareVoiceRequest(ctx); err == nil {
		t.Fatal("conflicting provider accepted")
	}
}

func BenchmarkPrepareVoiceRequest(b *testing.B) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/v1/audio/voices?provider=fastgen&model=aistudio%2Fgemini&language=en")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := prepareVoiceRequest(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestNeutralVideoGenerationAcceptsMultipart(t *testing.T) {
	SetLogger(&mockLogger{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		if r.FormValue("model") != "sora-2" || r.FormValue("prompt") != "lake" {
			t.Errorf("wrong native fields: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"original-video","object":"video","model":"sora-2","status":"queued"}`)
	}))
	defer upstream.Close()
	client, err := bifrost.Init(t.Context(), schemas.BifrostConfig{Account: inferenceSetupAccount{url: upstream.URL}, Logger: bifrost.NewNoOpLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{"model": "openai/sora-2", "prompt": "lake", "size": "1280x720", "seconds": "4"} {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(body.Bytes())
	(&CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{}}}).videoGeneration(ctx)
	if ctx.Response.StatusCode() != http.StatusOK {
		t.Fatalf("multipart video = %d, want 200: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestVoiceRoutesValidateProvider(t *testing.T) {
	r := router.New()
	(&CompletionHandler{}).RegisterRoutes(r)
	for _, path := range []string{"/v1/audio/voices", "/v1/audio/voices/source:voice/preview"} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI(path)
		r.Handler(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: %s", path, ctx.Response.StatusCode(), ctx.Response.Body())
		}
	}
}

// TestPrepareSpeechRequestVoiceValidation locks in the transport-layer voice
// requirement and provider-side validation. The transport cannot resolve
// virtual-key aliases, so it cannot tell a voice-less ElevenLabs sound-effect
// model (including an alias that resolves to one) from a text-to-speech model
// that omitted its voice. For ElevenLabs the voice check is therefore deferred
// to each native media provider after alias resolution. Other providers keep
// the handler-level "voice is required" 400.
func TestPrepareSpeechRequestVoiceValidation(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError bool
	}{
		{
			name: "fal music without voice is deferred to provider",
			body: `{"model":"fal/fal-ai/elevenlabs/music","input":"calm instrumental"}`,
		},
		{
			name: "lumean music without voice is deferred to provider",
			body: `{"model":"lumean/music_v2","input":"calm instrumental","duration_seconds":10}`,
		},
		{
			name: "lumean alias without voice is deferred to provider",
			body: `{"model":"lumean/my-sfx","input":"rain","duration_seconds":2}`,
		},
		{
			name: "voicegen resume without voice is deferred to provider",
			body: `{"model":"voicegen/sonic","input":"hello","resume_task_id":"existing-task"}`,
		},
		{
			name: "fastgen resume without voice is deferred to provider",
			body: `{"model":"fastgen/elevenlabs","input":"hello","resume_task_id":"existing-task"}`,
		},
		{
			// Regression: an alias like "my-sfx" -> "eleven_text_to_sound_v2"
			// carries no "eleven_text_to_sound" substring, so a name-based check
			// would wrongly 400 it before the provider could dispatch it.
			name:      "elevenlabs alias without voice is deferred to provider",
			body:      `{"model":"elevenlabs/my-sfx","input":"glass shattering on concrete"}`,
			wantError: false,
		},
		{
			name:      "elevenlabs literal sound model without voice is allowed",
			body:      `{"model":"elevenlabs/eleven_text_to_sound_v2","input":"rain on a tin roof"}`,
			wantError: false,
		},
		{
			name:      "elevenlabs tts without voice is deferred to provider (no transport 400)",
			body:      `{"model":"elevenlabs/eleven_multilingual_v2","input":"hello there"}`,
			wantError: false,
		},
		{
			name:      "non-elevenlabs provider without voice still errors at the transport",
			body:      `{"model":"openai/tts-1","input":"hello there"}`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetBodyString(tt.body)

			_, _, err := prepareSpeechRequest(ctx, nil)
			if tt.wantError && err == nil {
				t.Fatalf("expected voice-required error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantError && err != nil && err.Error() != "voice is required for speech completion" {
				t.Fatalf("error = %q, want %q", err.Error(), "voice is required for speech completion")
			}
		})
	}
}

func TestFalSpeechAttachmentKeepsNativeFormat(t *testing.T) {
	for _, tt := range []struct{ mime, wantMime, filename string }{
		{"audio/wav", "audio/wav", "speech.wav"},
		{"audio/wav; charset=binary", "audio/wav", "speech.wav"},
		{"audio/mpeg", "audio/mpeg", "speech.mp3"},
		{"audio/ogg", "audio/ogg", "speech.ogg"},
		{"audio/flac", "audio/flac", "speech.flac"},
		{"application/json", "audio/mpeg", "speech.mp3"},
		{"", "audio/mpeg", "speech.mp3"},
	} {
		t.Run(tt.mime, func(t *testing.T) {
			gotMime, filename := falSpeechAttachment(map[string]string{"content-type": tt.mime}, "speech.mp3")
			if gotMime != tt.wantMime || filename != tt.filename {
				t.Fatalf("got %s %s, want %s %s", gotMime, filename, tt.wantMime, tt.filename)
			}
		})
	}
}

func BenchmarkFalSpeechAttachment(b *testing.B) {
	headers := map[string]string{"content-type": "audio/wav"}
	b.ReportAllocs()
	for b.Loop() {
		falSpeechAttachment(headers, "speech.mp3")
	}
}
