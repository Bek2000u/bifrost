package voicegen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func testRequest() *schemas.BifrostSpeechRequest {
	return &schemas.BifrostSpeechRequest{Provider: schemas.Voicegen, Model: "sonic-3.6", Input: &schemas.SpeechInput{Input: "Hello"},
		Params: &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("cartesia:voice-a")}}}
}

func fixtureProvider(t *testing.T, handler http.Handler) (*VoicegenProvider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewVoicegenProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider.pollInterval = time.Millisecond
	return provider, server
}

func fixtureContext(id string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyRequestID, id)
	return ctx
}

func TestVoicegenSpeechContract(t *testing.T) {
	var posts, polls, downloads atomic.Int32
	var server *httptest.Server
	provider, s := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/client/tasks":
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer tenant-one" {
				t.Error("incorrect submission authentication")
				w.WriteHeader(400)
				return
			}
			posts.Add(1)
			var body VoicegenSpeechRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ClientTaskID == "" || body.VoiceID != "cartesia:voice-a" || body.VoiceSource != "auto" || !body.IncludeTimestamps || body.TimestampsGranularity != "word" || body.VoiceEngineSettings.Speed != 1 || body.SplitIntoChunkFiles || body.SplitIntoParagraphFiles {
				t.Errorf("incorrect native request: %+v", body)
			}
			fmt.Fprint(w, `{"task":{"task_id":"job-one","status":"queued"}}`)
		case "/api/v1/client/tasks/job-one":
			if r.Header.Get("Authorization") != "Bearer tenant-one" {
				t.Error("incorrect poll authentication")
			}
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"task":{"task_id":"job-one","status":"done","timestamps_complete":false}}`)
				return
			}
			fmt.Fprintf(w, `{"task":{"task_id":"job-one","status":"done","timestamps_complete":true,"timestamps_format":"srt","timestamps_granularity":"word","timestamps_url":%q,"result_chars_count":4}}`, server.URL+"/timings.srt")
		case "/api/v1/client/tasks/job-one/download":
			if r.Header.Get("Authorization") != "Bearer tenant-one" {
				t.Error("missing private download authentication")
			}
			downloads.Add(1)
			fmt.Fprint(w, "audio-one")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	server = s
	ctx := fixtureContext("request-one")
	response, err := provider.Speech(ctx, schemas.Key{Value: schemas.SecretVar{Val: "tenant-one"}}, testRequest())
	if err != nil {
		t.Fatalf("speech: %+v", err)
	}
	if string(response.Audio) != "audio-one" || response.Usage == nil || response.Usage.InputChars != 4 || response.Usage.Cost != nil {
		t.Fatalf("incorrect response: %+v", response)
	}
	if posts.Load() != 1 || polls.Load() != 2 || downloads.Load() != 1 {
		t.Fatalf("unexpected counts: post=%d poll=%d download=%d", posts.Load(), polls.Load(), downloads.Load())
	}
	if response.ExtraFields.ProviderResponseHeaders["x-bf-provider-task-id"] != "job-one" {
		t.Fatal("task ID header missing")
	}
	if response.ExtraFields.ProviderResponseHeaders["x-bf-provider-characters"] != "4" {
		t.Fatal("actual billed characters header missing")
	}
}

func TestVoicegenPendingResumeDoesNotResubmit(t *testing.T) {
	var posts atomic.Int32
	var ready atomic.Bool
	var server *httptest.Server
	provider, s := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/client/tasks":
			posts.Add(1)
			fmt.Fprint(w, `{"task":{"task_id":"existing","status":"queued"}}`)
		case "/api/v1/client/tasks/existing":
			if !ready.Load() {
				fmt.Fprint(w, `{"task":{"task_id":"existing","status":"running"}}`)
				return
			}
			fmt.Fprintf(w, `{"task":{"task_id":"existing","status":"done","timestamps_complete":true,"timestamps_format":"srt","timestamps_granularity":"word","timestamps_url":%q,"public_download_url":%q}}`, server.URL+"/timestamps", server.URL+"/public-audio")
		case "/public-audio":
			if r.Header.Get("Authorization") != "" {
				t.Error("credentials leaked to public artifact")
			}
			fmt.Fprint(w, "audio")
		default:
			w.WriteHeader(404)
		}
	}))
	server = s
	provider.timeout = 15 * time.Millisecond
	_, err := provider.Speech(fixtureContext("request-two"), schemas.Key{}, testRequest())
	if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || !strings.Contains(err.Error.Message, "task_id=existing") {
		t.Fatalf("pending outcome lost: %+v", err)
	}
	ready.Store(true)
	provider.timeout = time.Second
	request := &schemas.BifrostSpeechRequest{Model: "sonic-3.6", Params: &schemas.SpeechParameters{ExtraParams: map[string]interface{}{"resume_task_id": "existing"}}}
	response, err := provider.Speech(fixtureContext("resume"), schemas.Key{}, request)
	if err != nil || string(response.Audio) != "audio" || posts.Load() != 1 {
		t.Fatalf("resume must use existing task: %+v posts=%d", err, posts.Load())
	}
}

func TestVoicegenUnknownSubmissionBlocksRetry(t *testing.T) {
	var calls atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		fmt.Fprint(w, `{"message":"upstream unavailable"}`)
	}))
	_, err := provider.Speech(fixtureContext("stable-id"), schemas.Key{}, testRequest())
	if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || !strings.Contains(err.Error.Message, "client_task_id=") || calls.Load() != 1 {
		t.Fatalf("submission retry guard missing: %+v calls=%d", err, calls.Load())
	}
}

func TestVoicegenBYOKIsolation(t *testing.T) {
	var server *httptest.Server
	provider, s := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == "/api/v1/client/tasks" {
			fmt.Fprintf(w, `{"task":{"task_id":%q}}`, key)
			return
		}
		if r.URL.Path == "/api/v1/client/tasks/"+key {
			fmt.Fprintf(w, `{"task":{"task_id":%q,"status":"done","timestamps_complete":true,"timestamps_format":"srt","timestamps_granularity":"word","timestamps_url":%q}}`, key, server.URL+"/timestamp")
			return
		}
		if r.URL.Path == "/api/v1/client/tasks/"+key+"/download" {
			fmt.Fprint(w, key)
			return
		}
		t.Error("tenant credentials/task path mismatch")
		w.WriteHeader(401)
	}))
	server = s
	var wg sync.WaitGroup
	for _, tenant := range []string{"tenant-one", "tenant-two"} {
		wg.Add(1)
		go func(tenant string) {
			defer wg.Done()
			response, err := provider.Speech(fixtureContext(tenant), schemas.Key{Value: schemas.SecretVar{Val: tenant}}, testRequest())
			if err != nil || string(response.Audio) != tenant {
				t.Errorf("tenant isolation failed: %s %+v", tenant, err)
			}
		}(tenant)
	}
	wg.Wait()
}

func TestVoicegenConversionValidation(t *testing.T) {
	wire, err := ToVoicegenSpeechRequest(testRequest(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	repeat, _ := ToVoicegenSpeechRequest(testRequest(), "stable")
	other, _ := ToVoicegenSpeechRequest(testRequest(), "other")
	if wire.ClientTaskID != repeat.ClientTaskID || wire.ClientTaskID == other.ClientTaskID {
		t.Fatal("unstable client task ID")
	}
	for _, mutate := range []func(*schemas.BifrostSpeechRequest){
		func(r *schemas.BifrostSpeechRequest) { r.Params.Speed = schemas.Ptr(2.0) },
		func(r *schemas.BifrostSpeechRequest) { r.Params.ResponseFormat = "wav" },
		func(r *schemas.BifrostSpeechRequest) {
			r.Params.ExtraParams = map[string]interface{}{"voice_source": "elevenlabs"}
		},
		func(r *schemas.BifrostSpeechRequest) {
			r.Params.ExtraParams = map[string]interface{}{"settings_preset": "stable"}
			r.Params.Speed = schemas.Ptr(1.0)
		},
	} {
		r := testRequest()
		mutate(r)
		if _, err := ToVoicegenSpeechRequest(r, "stable"); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	r := testRequest()
	r.Params.Speed = schemas.Ptr(125.0)
	wire, err = ToVoicegenSpeechRequest(r, "stable")
	if err != nil || wire.VoiceEngineSettings.Speed != 1.25 {
		t.Fatal("percentage speed conversion failed")
	}
}

func BenchmarkVoicegenSpeechConversion(b *testing.B) {
	request := testRequest()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToVoicegenSpeechRequest(request, "benchmark-request"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestVoicegenVoicePagesAndAuthorizedPreview(t *testing.T) {
	var previews atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer voice-key" {
			t.Errorf("wrong native voice method/auth")
		}
		switch r.URL.Path {
		case "/api/v1/client/cartesia/voices":
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "100" || r.URL.Query().Get("category") != "all" {
				t.Errorf("wrong cartesia voice pagination: %s", r.URL)
			}
			fmt.Fprint(w, `{"voices":[{"voice_id":"voice-a","name":"A","language":"en","gender":"female"}],"page":2,"total_pages":3}`)
		case "/api/v1/client/elevenlabs/voices/voice-b/preview":
			previews.Add(1)
			w.Header().Set("Content-Type", "audio/wav")
			fmt.Fprint(w, "authorized-preview")
		case "/api/v1/client/elevenlabs/voices":
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("language") != "en" {
				t.Errorf("wrong elevenlabs voice query: %s", r.URL)
			}
			fmt.Fprint(w, `{"voices":[{"voice_id":"voice-b","name":"B","labels":{"language":"en","gender":"male"}}],"page":1,"total_pages":1}`)
		default:
			t.Errorf("unexpected native voice route: %s", r.URL)
		}
	}))
	key := schemas.Key{Value: schemas.SecretVar{Val: "voice-key"}}
	request := &schemas.BifrostPassthroughRequest{Provider: schemas.Voicegen, Model: "sonic-3.6", Method: "GET", Path: "/v1/audio/voices", RawQuery: "source=cartesia&cursor=2&language=en"}
	response, err := provider.Passthrough(fixtureContext("voices"), key, request)
	if err != nil {
		t.Fatalf("voice catalog: %#v", err)
	}
	var result struct {
		Data []struct {
			ID         string
			PreviewURL string `json:"preview_url"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
	}
	if json.Unmarshal(response.Body, &result) != nil || len(result.Data) != 1 || result.Data[0].ID != "cartesia:voice-a" || result.NextCursor != "3" || !strings.Contains(result.Data[0].PreviewURL, "cursor=2") || !strings.Contains(result.Data[0].PreviewURL, "source=cartesia") {
		t.Fatalf("wrong source/pagination metadata: %s", response.Body)
	}
	request.RawQuery = "source=elevenlabs&language=en"
	response, err = provider.Passthrough(fixtureContext("eleven-voices"), key, request)
	if err != nil || !strings.Contains(string(response.Body), `"id":"elevenlabs:voice-b"`) || !strings.Contains(string(response.Body), `"gender":"male"`) {
		t.Fatalf("elevenlabs voice labels: %+v %+v", response, err)
	}
	request.Path, request.RawQuery = "/v1/audio/voices/elevenlabs:voice-b/preview", "source=elevenlabs"
	response, err = provider.Passthrough(fixtureContext("preview"), key, request)
	if err != nil || string(response.Body) != "authorized-preview" || response.Headers["Content-Type"] != "audio/wav" || previews.Load() != 1 {
		t.Fatalf("native preview: %+v %+v", response, err)
	}
	request.RawQuery = "source=cartesia"
	if _, err = provider.Passthrough(fixtureContext("bad"), key, request); err == nil || err.StatusCode == nil || *err.StatusCode != 400 || previews.Load() != 1 {
		t.Fatal("source prefix conflict must fail before native call")
	}
}

func TestVoicegenVoicePaginationAndCancellation(t *testing.T) {
	var requests atomic.Int32
	provider, _ := fixtureProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"voices":[],"page":2,"total_pages":3}`)
	}))
	request := &schemas.BifrostPassthroughRequest{Method: "GET", Path: "/v1/audio/voices"}
	if _, err := provider.Passthrough(fixtureContext("pages"), schemas.Key{}, request); err == nil || err.StatusCode == nil || *err.StatusCode != 502 {
		t.Fatal("mismatched native page accepted")
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Passthrough(schemas.NewBifrostContext(parent, time.Time{}), schemas.Key{}, request); err == nil || requests.Load() != 1 {
		t.Fatal("cancelled catalog reached upstream")
	}
}

func BenchmarkVoicegenVoiceCatalogConversion(b *testing.B) {
	native := &VoicegenVoicesResponse{Voices: []VoicegenVoice{{VoiceID: "voice-a", Name: "A", Language: "en"}}, Page: 1, TotalPages: 2}
	options := providerUtils.VoiceCatalogOptions{Model: "sonic-3.6", Source: "cartesia", Page: 1}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToBifrostVoiceCatalog(native, options); err != nil {
			b.Fatal(err)
		}
	}
}
