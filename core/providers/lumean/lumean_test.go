package lumean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func testRequest() *schemas.BifrostSpeechRequest {
	return &schemas.BifrostSpeechRequest{Provider: schemas.Lumean, Model: "eleven_multilingual_v2", Input: &schemas.SpeechInput{Input: "Hello"},
		Params: &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("voice-a")},
			ExtraParams: map[string]interface{}{"template_id": "11111111-1111-4111-8111-111111111111"}}}
}
func fixtureContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), time.Time{})
}
func fixtureProvider(t *testing.T, handler http.Handler) (*LumeanProvider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewLumeanProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider.pollInterval = time.Millisecond
	return provider, server
}

func TestLumeanSpeechContractAndSignedURLRenewal(t *testing.T) {
	var posts, storage atomic.Int32
	var server *httptest.Server
	provider, s := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orders":
			if r.Header.Get("X-API-KEY") != "tenant-key" {
				t.Error("wrong submission key")
			}
			posts.Add(1)
			var body LumeanSpeechRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.TemplateID != "11111111-1111-4111-8111-111111111111" || body.InputText != "Hello" || body.ConfigOverride.TTSSettings.ModelID != "eleven_multilingual_v2" || body.ConfigOverride.TTSSettings.VoiceID != "voice-a" {
				t.Errorf("incorrect native order: %+v", body)
			}
			fmt.Fprint(w, `{"data":{"id":"order-one","status":"created"}}`)
		case "/orders/order-one":
			if r.Method != "GET" || r.Header.Get("X-API-KEY") != "tenant-key" {
				t.Error("wrong retrieval authentication")
			}
			fmt.Fprint(w, `{"data":{"id":"order-one","status":"completed","tokens_spent":1.25,"result":{"files":["orders/one/audio.mp3"],"service_files":["orders/one/alignment.json"]}}}`)
		case "/storage/url":
			if r.Method != "POST" || r.Header.Get("X-API-KEY") != "tenant-key" {
				t.Error("wrong storage authentication")
			}
			var body LumeanStorageRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Path != "orders/one/audio.mp3" || body.Download {
				t.Errorf("invalid storage body: %+v", body)
			}
			n := storage.Add(1)
			fmt.Fprintf(w, `{"data":{"url":%q}}`, fmt.Sprintf("%s/audio?signature=%d", server.URL, n))
		case "/audio":
			if r.Header.Get("X-API-KEY") != "" || r.Header.Get("Authorization") != "" {
				t.Error("provider credentials leaked to artifact")
			}
			fmt.Fprint(w, "audio")
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	server = s
	key := schemas.Key{Value: schemas.SecretVar{Val: "tenant-key"}}
	response, err := provider.Speech(fixtureContext(), key, testRequest())
	if err != nil {
		t.Fatalf("speech: %+v", err)
	}
	receipt := response.ExtraFields.RawResponse.(*LumeanReceipt)
	if string(response.Audio) != "audio" || response.Usage != nil || receipt.ProviderCredits.String() != "1.25" {
		t.Fatalf("incorrect response: %+v", response)
	}
	if response.ExtraFields.ProviderResponseHeaders["x-bf-provider-task-id"] != "order-one" {
		t.Fatal("missing native order header")
	}
	if response.ExtraFields.ProviderResponseHeaders["x-bf-provider-credits"] != "1.25" {
		t.Fatal("native provider credits header missing")
	}
	resumed, err := provider.RetrieveSpeech(fixtureContext(), key, "eleven_multilingual_v2", "order-one")
	if err != nil || string(resumed.Audio) != "audio" || posts.Load() != 1 || storage.Load() != 2 {
		t.Fatalf("resume must renew storage URL without resubmit: %+v post=%d storage=%d", err, posts.Load(), storage.Load())
	}
}

func TestLumeanPendingConfirmationAndUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"pending", `{"data":{"id":"order","status":"pending"}}`},
		{"confirmation", `{"confirmation_required":true,"data":{"id":"order","status":"pending"}}`},
		{"mismatch", `{"data":{"id":"other","status":"completed"}}`},
		{"failed", `{"data":{"id":"order","status":"failed","tokens_spent":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
					fmt.Fprint(w, `{"data":{"id":"order"}}`)
					return
				}
				fmt.Fprint(w, tc.reply)
			}))
			provider.timeout = 15 * time.Millisecond
			_, err := provider.Speech(fixtureContext(), schemas.Key{}, testRequest())
			if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || !strings.Contains(err.Error.Message, "task_id=order") || posts.Load() != 1 {
				t.Fatalf("invalid recovery behavior: %+v posts=%d", err, posts.Load())
			}
		})
	}
}

func TestLumeanUnknownSubmissionDoesNotRetry(t *testing.T) {
	var posts atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(503)
		fmt.Fprint(w, `{"message":"unavailable"}`)
	}))
	_, err := provider.Speech(fixtureContext(), schemas.Key{}, testRequest())
	if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || posts.Load() != 1 || !strings.Contains(err.Error.Message, "outcome unknown") {
		t.Fatalf("submission outcome unsafe: %+v", err)
	}
}

func TestLumeanInvalidArtifactDoesNotDownload(t *testing.T) {
	var downloads atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orders/order":
			fmt.Fprint(w, `{"data":{"id":"order","status":"completed","result":{"files":["order/audio.mp3"]}}}`)
		case "/storage/url":
			fmt.Fprint(w, `{"data":{"url":"https://untrusted.example/audio.mp3"}}`)
		default:
			downloads.Add(1)
			w.WriteHeader(400)
		}
	}))
	_, err := provider.RetrieveSpeech(fixtureContext(), schemas.Key{}, "eleven_multilingual_v2", "order")
	if err == nil || downloads.Load() != 0 {
		t.Fatalf("untrusted artifact accepted: %+v", err)
	}
	for _, path := range []string{"", "../audio.mp3", "/audio.mp3", "x\\audio.mp3", "x/../audio.mp3", "https://host/audio"} {
		if validStoragePath(path) {
			t.Errorf("invalid storage path accepted: %q", path)
		}
	}
}

func TestLumeanNativeAudioConversions(t *testing.T) {
	for _, model := range []string{"music_v2", "eleven_text_to_sound_v2"} {
		request := &schemas.BifrostSpeechRequest{Model: model, Input: &schemas.SpeechInput{Input: "Quiet ocean waves"}, Params: &schemas.SpeechParameters{ExtraParams: map[string]interface{}{"duration_seconds": 12.5}}}
		wire, err := ToLumeanSpeechRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if model == "music_v2" && (wire.TaskType != "music" || *wire.TaskData.MusicLengthMs != 12500 || !*wire.TaskData.ForceInstrumental || *wire.TaskData.NVariants != 1) {
			t.Fatalf("incorrect music body: %+v", wire)
		}
		if model != "music_v2" && (wire.TaskType != "sfx" || *wire.TaskData.DurationSeconds != 12.5 || *wire.TaskData.Loop || wire.TaskData.OutputFormat != "mp3_44100_192") {
			t.Fatalf("incorrect SFX body: %+v", wire)
		}
	}
	for _, mutate := range []func(*schemas.BifrostSpeechRequest){
		func(r *schemas.BifrostSpeechRequest) { r.Params.ExtraParams["template_id"] = "not-uuid" },
		func(r *schemas.BifrostSpeechRequest) { r.Params.ExtraParams["public_owner_id"] = "../owner" },
		func(r *schemas.BifrostSpeechRequest) { r.Params.Speed = schemas.Ptr(1.0) },
		func(r *schemas.BifrostSpeechRequest) { r.Params.ResponseFormat = "wav" },
	} {
		r := testRequest()
		mutate(r)
		if _, err := ToLumeanSpeechRequest(r); err == nil {
			t.Fatal("invalid TTS input accepted")
		}
	}
}

func TestLumeanModelCatalogUsesBothServices(t *testing.T) {
	var queries atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/templates/config-options" {
			t.Error("incorrect catalog endpoint")
		}
		if service := r.URL.Query().Get("service"); service != "cartesia" && service != "elevenlabs" {
			t.Error("missing service")
		}
		queries.Add(1)
		fmt.Fprint(w, `{"success":true,"data":{"tts_settings":{"model_id":{"options":["shared-model"]}}}}`)
	}))
	models, err := provider.catalog(context.Background(), schemas.Key{})
	if err != nil || len(models) != 3 || queries.Load() != 2 {
		t.Fatalf("invalid merged catalog: %+v %+v", models, err)
	}
}

func BenchmarkLumeanSpeechConversion(b *testing.B) {
	request := testRequest()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToLumeanSpeechRequest(request); err != nil {
			b.Fatal(err)
		}
	}
}

func TestLumeanVoicePagesAndExistingPreview(t *testing.T) {
	var downloads atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("preview must not submit an order")
		}
		switch r.URL.Path {
		case "/voices/elevenlabs/library":
			if r.Header.Get("X-API-KEY") != "voice-key" || r.URL.Query().Get("page") != "1" || r.URL.Query().Get("page_size") != "100" || r.URL.Query().Get("required_languages") != "en" {
				t.Errorf("wrong elevenlabs pagination/auth: %s", r.URL)
			}
			fmt.Fprint(w, `{"success":true,"data":{"voices":[{"voice_id":"voice-a","name":"A","language":"en","gender":"female","preview_url":"/samples/a.mp3"}],"has_more":true}}`)
		case "/voices/cartesia":
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("language") != "en" {
				t.Errorf("wrong cartesia page: %s", r.URL)
			}
			fmt.Fprint(w, `{"success":true,"data":{"items":[{"voice_id":"voice-c","name":"C","preview_url":"http://untrusted.test/private"}],"current_page":2,"last_page":2}}`)
		case "/samples/a.mp3":
			downloads.Add(1)
			if r.Header.Get("X-API-KEY") != "" || r.Header.Get("Authorization") != "" {
				t.Error("native key leaked to public sample")
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "existing-preview")
		default:
			t.Errorf("unexpected voice route: %s", r.URL)
		}
	}))
	key := schemas.Key{Value: schemas.SecretVar{Val: "voice-key"}}
	request := &schemas.BifrostPassthroughRequest{Provider: schemas.Lumean, Model: "eleven_multilingual_v2", Method: "GET", Path: "/v1/audio/voices", RawQuery: "cursor=2&language=en"}
	response, err := provider.Passthrough(fixtureContext(), key, request)
	if err != nil {
		t.Fatalf("voices: %#v", err)
	}
	var result struct {
		Data []struct {
			ID         string
			PreviewURL string `json:"preview_url"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
	}
	if json.Unmarshal(response.Body, &result) != nil || len(result.Data) != 1 || result.Data[0].ID != "voice-a" || result.NextCursor != "3" || !strings.Contains(result.Data[0].PreviewURL, "cursor=2") {
		t.Fatalf("wrong normalized page: %s", response.Body)
	}
	request.Path = "/v1/audio/voices/voice-a/preview"
	response, err = provider.Passthrough(fixtureContext(), key, request)
	if err != nil || string(response.Body) != "existing-preview" || response.Headers["Content-Type"] != "audio/mpeg" || downloads.Load() != 1 {
		t.Fatalf("preview: %+v %+v", response, err)
	}
	request.Model, request.Path = "sonic-3.6", "/v1/audio/voices"
	response, err = provider.Passthrough(fixtureContext(), key, request)
	if err != nil {
		t.Fatal(err)
	}
	result.Data, result.NextCursor = nil, ""
	if json.Unmarshal(response.Body, &result) != nil || result.NextCursor != "" || len(result.Data) != 1 || result.Data[0].PreviewURL != "" {
		t.Fatalf("untrusted sample must not become a preview URL: %s", response.Body)
	}
	request.Path = "/v1/audio/voices/voice-c/preview"
	if _, err = provider.Passthrough(fixtureContext(), key, request); err == nil || err.StatusCode == nil || *err.StatusCode != 404 || downloads.Load() != 1 {
		t.Fatal("untrusted sample must not be fetched")
	}
}

func TestLumeanVoicePaginationAndCancellation(t *testing.T) {
	var requests atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"success":true,"data":{"voices":[]}}`)
	}))
	request := &schemas.BifrostPassthroughRequest{Method: "GET", Path: "/v1/audio/voices", Model: "eleven_multilingual_v2"}
	if _, err := provider.Passthrough(fixtureContext(), schemas.Key{}, request); err == nil || err.StatusCode == nil || *err.StatusCode != 502 {
		t.Fatal("missing native pagination accepted")
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Passthrough(schemas.NewBifrostContext(parent, time.Time{}), schemas.Key{}, request); err == nil || requests.Load() != 1 {
		t.Fatal("cancelled catalog reached upstream")
	}
}

func BenchmarkLumeanVoiceCatalogConversion(b *testing.B) {
	provider := &LumeanProvider{networkConfig: schemas.NetworkConfig{BaseURL: "https://api.lumean.app/api/public"}}
	native := &LumeanVoicesResponse{Data: LumeanVoicesPage{Voices: []LumeanVoice{{VoiceID: "voice-a", Name: "A", PreviewURL: "https://storage.lumean.app/sample.mp3"}}, HasMore: schemas.Ptr(false)}}
	options := providerUtils.VoiceCatalogOptions{Model: "eleven_multilingual_v2", Source: "elevenlabs", Page: 1}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := provider.ToBifrostVoiceCatalog(native, options); err != nil {
			b.Fatal(err)
		}
	}
}
