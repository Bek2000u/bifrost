package fastgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func fixtureProvider(t *testing.T, handler http.HandlerFunc) (*FastgenProvider, *schemas.BifrostContext) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewFastgenProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true, DefaultRequestTimeoutInSeconds: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider.pollInterval = time.Millisecond
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	return provider, ctx
}
func fixtureKey(value string) schemas.Key { return schemas.Key{Value: schemas.SecretVar{Val: value}} }
func promptRequest(text string) *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{Provider: schemas.Fastgen, Model: "prompt-generator", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &text}}}}
}
func imageRequest() *schemas.BifrostImageGenerationRequest {
	return &schemas.BifrostImageGenerationRequest{Provider: schemas.Fastgen, Model: "flower-image", Input: &schemas.ImageGenerationInput{Prompt: "a flower"}}
}

func TestPromptNativeWireAndUsage(t *testing.T) {
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v6/prompts/generate" || r.Header.Get("X-API-Key") != "tenant-one" {
			t.Errorf("wrong native request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 1 || body["user_prompt"] != "Привет" {
			t.Errorf("unexpected body: %#v", body)
		}
		fmt.Fprint(w, `{"success":true,"operation_id":"prompt-1","generated_text":"A prompt","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
	})
	result, err := provider.ChatCompletion(ctx, fixtureKey("tenant-one"), promptRequest("Привет"))
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "prompt-1" || result.Usage.TotalTokens != 8 || *result.Choices[0].Message.Content.ContentStr != "A prompt" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := provider.ChatCompletionStream(ctx, nil, nil, fixtureKey("tenant-one"), promptRequest("text")); err == nil {
		t.Fatal("streaming must not be simulated")
	}
}

func TestImagePollReceiptAndResume(t *testing.T) {
	var submissions atomic.Int32
	var polls atomic.Int32
	var serverURL string
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			submissions.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "flower-image" || body["prompt"] != "a flower" {
				t.Errorf("unexpected body: %#v", body)
			}
			fmt.Fprint(w, `{"id":"job-1","status":"queued"}`)
			return
		}
		if r.URL.Path != "/api/v6/generations/job-1" {
			t.Errorf("unexpected status URL %s", r.URL.Path)
		}
		if polls.Add(1) == 1 {
			fmt.Fprint(w, `{"id":"job-1","status":"running"}`)
			return
		}
		fmt.Fprintf(w, `{"id":"job-1","status":"succeeded","results":[{"type":"image","download_url":%q}],"usage":{"credits":1}}`, serverURL+"/asset.png")
	})
	serverURL = provider.networkConfig.BaseURL
	result, err := provider.ImageGeneration(ctx, fixtureKey("key"), imageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "job-1" || len(result.Data) != 1 || result.ExtraFields.ProviderResponseHeaders["x-bf-provider-task-id"] != "job-1" || result.ExtraFields.ProviderResponseHeaders["x-bf-provider-credits"] != "1" {
		t.Fatalf("receipt missing: %#v", result)
	}
	if result.Usage != nil && result.Usage.Cost != nil {
		t.Fatal("provider credits must not become USD")
	}
	resume := imageRequest()
	resume.Params = &schemas.ImageGenerationParameters{ExtraParams: map[string]any{"resume_task_id": "job-1"}}
	if _, err := provider.ImageGeneration(ctx, fixtureKey("key"), resume); err != nil {
		t.Fatal(err)
	}
	if submissions.Load() != 1 {
		t.Fatalf("resume resubmitted %d paid requests", submissions.Load())
	}
}

func TestAcceptedFailuresStopRetriesAndRetainTaskID(t *testing.T) {
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			fmt.Fprint(w, `{"id":"accepted-1"}`)
			return
		}
		w.WriteHeader(503)
		fmt.Fprint(w, `{"message":"temporary failure"}`)
	})
	_, err := provider.ImageGeneration(ctx, fixtureKey("key"), imageRequest())
	if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || !strings.Contains(err.Error.Message, "accepted-1") {
		t.Fatalf("unsafe accepted failure: %#v", err)
	}
	headers, _ := ctx.Value(schemas.BifrostContextKeyProviderResponseHeaders).(map[string]string)
	if headers["x-bf-provider-task-id"] != "accepted-1" {
		t.Fatal("accepted id was lost")
	}
}

func TestVideoSubmitRetrieveAndDownloadNoCredentialLeak(t *testing.T) {
	var serverURL string
	var submissions atomic.Int32
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v6/generations":
			submissions.Add(1)
			fmt.Fprint(w, `{"id":"video-1","status":"queued"}`)
		case "/api/v6/generations/video-1":
			fmt.Fprintf(w, `{"id":"video-1","model":"flow-video-fast","status":"succeeded","results":[{"type":"video","download_url":%q,"mime_type":"video/mp4"}]}`, serverURL+"/asset.mp4")
		case "/asset.mp4":
			if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Error("provider credentials leaked to media URL")
			}
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprint(w, "video-bytes")
		default:
			t.Errorf("unknown path %s", r.URL.Path)
		}
	})
	serverURL = provider.networkConfig.BaseURL
	video, err := provider.VideoGeneration(ctx, fixtureKey("secret"), &schemas.BifrostVideoGenerationRequest{Model: "flow-video-fast", Input: &schemas.VideoGenerationInput{Prompt: "a scene"}})
	if err != nil {
		t.Fatal(err)
	}
	if video.Status != schemas.VideoStatusQueued || providerUtils.StripVideoIDProviderSuffix(video.ID, schemas.Fastgen) != "video-1" {
		t.Fatalf("unexpected video: %#v", video)
	}
	completed, retrieveErr := provider.VideoRetrieve(ctx, fixtureKey("secret"), &schemas.BifrostVideoRetrieveRequest{ID: video.ID})
	if retrieveErr != nil || completed.Model != "flow-video-fast" || completed.Status != schemas.VideoStatusCompleted || len(completed.Videos) != 1 || completed.Seconds != nil || completed.Size != "" || completed.Usage != nil {
		t.Fatalf("video receipt must retain native model and leave absent dimensions/cost unknown: %#v %#v", completed, retrieveErr)
	}
	content, err := provider.VideoDownload(ctx, fixtureKey("secret"), &schemas.BifrostVideoDownloadRequest{ID: video.ID})
	if err != nil {
		t.Fatal(err)
	}
	if string(content.Content) != "video-bytes" || content.ContentType != "video/mp4" || submissions.Load() != 1 {
		t.Fatalf("unexpected download: %#v", content)
	}
}

func TestTenantKeysDoNotMix(t *testing.T) {
	provider, _ := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body FastgenPromptRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("X-API-Key") != body.UserPrompt {
			t.Errorf("cross-tenant key %q for %q", r.Header.Get("X-API-Key"), body.UserPrompt)
		}
		fmt.Fprint(w, `{"operation_id":"ok","generated_text":"ok","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	var wg sync.WaitGroup
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", index)
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			if _, err := provider.ChatCompletion(ctx, fixtureKey(tenant), promptRequest(tenant)); err != nil {
				t.Error(err)
			}
		}(index)
	}
	wg.Wait()
}

func TestNativeConvertersRejectUnsupportedInputs(t *testing.T) {
	if _, err := ToFastgenChatRequest(promptRequest(strings.Repeat("я", 10000))); err != nil {
		t.Fatal(err)
	}
	if _, err := ToFastgenChatRequest(promptRequest(strings.Repeat("я", 10001))); err == nil {
		t.Fatal("oversized prompt accepted")
	}
	if _, err := generationRequest("flower-image", "image", "prompt", []any{"https://example.com/image.png"}, nil); err == nil {
		t.Fatal("unsupported remote reference accepted")
	}
	if _, err := generationRequest("flow-video-quality", "video", "prompt", []any{"data:image/png;base64,eA=="}, nil); err == nil {
		t.Fatal("quality model needs keyframes")
	}
	if _, err := generationRequest("flower-image", "image", "prompt", nil, map[string]any{"seed": 2}); err == nil {
		t.Fatal("unsupported per-model option accepted")
	}
	if err := validateArtifact("https://evil.example/file", "https://api.fast-gen.ai"); err == nil {
		t.Fatal("unknown media host accepted")
	}
}

func TestGenerationOptionsAreAlwaysOnNativeWire(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	request := &schemas.BifrostSpeechRequest{Model: "aistudio/gemini-2.5-flash-preview-tts", Input: &schemas.SpeechInput{Input: "hello"}, Params: &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}}}
	body, err := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) { return ToFastgenSpeechRequest(request) })
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]any
	if err := json.Unmarshal(body, &native); err != nil {
		t.Fatal(err)
	}
	if native["voice"] != "Kore" {
		t.Fatalf("native voice dropped when extra passthrough is disabled: %s", body)
	}
}

func TestTranscriptionWordsAndProviderCredits(t *testing.T) {
	native := &FastgenGenerationResponse{ID: "transcript-1", Status: "succeeded", Results: []FastgenResult{{Type: "text", Text: "hello", Transcription: &FastgenTranscript{WordTimestamps: true, Segments: []FastgenTranscriptSegment{{Text: "hello", Words: []FastgenTranscriptWord{{Text: "hello", StartSeconds: 0, EndSeconds: .5}}}}}}}}
	result, err := ToBifrostTranscriptionResponse(native, true)
	if err != nil || len(result.Words) != 1 || result.Words[0].End != .5 {
		t.Fatalf("transcript conversion: %#v %v", result, err)
	}
	native.Results[0].Transcription.Segments[0].Words[0].EndSeconds = -1
	if _, err := ToBifrostTranscriptionResponse(native, true); err == nil {
		t.Fatal("invalid timestamps accepted")
	}
}

func TestTranscriptionUsesOnlyReportedDurationForUsage(t *testing.T) {
	for _, test := range []struct {
		name    string
		seconds *float64
	}{
		{"fractional", schemas.Ptr(0.5)},
		{"zero", schemas.Ptr(0.0)},
		{"absent", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			native := &FastgenGenerationResponse{ID: "transcript-usage", Status: "succeeded", Usage: &FastgenUsage{Credits: "10"}, Results: []FastgenResult{{Type: "text", Text: "hello", Transcription: &FastgenTranscript{DurationSeconds: test.seconds}}}}
			result, err := ToBifrostTranscriptionResponse(native, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.seconds == nil {
				if result.Usage != nil {
					t.Fatal("missing native duration must remain unknown")
				}
				return
			}
			if result.Usage == nil || result.Usage.Type != "duration" || result.Usage.Seconds == nil || *result.Usage.Seconds != *test.seconds {
				t.Fatalf("reported transcription duration missing from usage: %#v", result.Usage)
			}
			if result.Usage.Cost != nil {
				t.Fatal("provider credits must not become USD")
			}
		})
	}
}

func TestTextAndResponsesUseNativePromptContract(t *testing.T) {
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"operation_id":"prompt-2","generated_text":"expanded","usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	})
	text, err := provider.TextCompletion(ctx, fixtureKey("key"), &schemas.BifrostTextCompletionRequest{Model: "prompt-generator", Input: &schemas.TextCompletionInput{PromptStr: schemas.Ptr("hello")}})
	if err != nil || text.ID != "prompt-2" || *text.Choices[0].Text != "expanded" {
		t.Fatalf("text result: %#v %#v", text, err)
	}
	responses, err := provider.Responses(ctx, fixtureKey("key"), promptRequest("hello").ToResponsesRequest())
	if err != nil || responses.ID == nil || *responses.ID != "prompt-2" {
		t.Fatalf("responses result: %#v %#v", responses, err)
	}
	if _, err := provider.Responses(ctx, fixtureKey("key"), &schemas.BifrostResponsesRequest{Model: "prompt-generator", Params: &schemas.ResponsesParameters{PreviousResponseID: schemas.Ptr("old")}}); err == nil || err.StatusCode == nil || *err.StatusCode != 400 {
		t.Fatal("unsupported previous response must fail before submit")
	}
}

func TestSpeechNativeWireAndResume(t *testing.T) {
	var serverURL string
	var submissions atomic.Int32
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v6/generations":
			submissions.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "aistudio/gemini-2.5-flash-preview-tts" || body["prompt"] != "Привет" || body["voice"] != "Kore" {
				t.Errorf("wrong speech wire: %#v", body)
			}
			fmt.Fprint(w, `{"id":"speech-1"}`)
		case "/api/v6/generations/speech-1":
			fmt.Fprintf(w, `{"id":"speech-1","status":"succeeded","results":[{"type":"audio","download_url":%q}],"usage":{"credits":10}}`, serverURL+"/audio.mp3")
		case "/audio.mp3":
			if r.Header.Get("X-API-Key") != "" {
				t.Error("audio download leaked provider key")
			}
			fmt.Fprint(w, "audio")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	serverURL = provider.networkConfig.BaseURL
	request := &schemas.BifrostSpeechRequest{Model: "aistudio/gemini-2.5-flash-preview-tts", Input: &schemas.SpeechInput{Input: "Привет"}, Params: &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}}}
	result, err := provider.Speech(ctx, fixtureKey("private"), request)
	if err != nil || string(result.Audio) != "audio" || result.Usage.InputChars != 6 || result.ExtraFields.ProviderResponseHeaders["x-bf-provider-task-id"] != "speech-1" {
		t.Fatalf("speech result: %#v %#v", result, err)
	}
	request.Params.ExtraParams = map[string]any{"resume_task_id": "speech-1"}
	if _, err := provider.Speech(ctx, fixtureKey("private"), request); err != nil {
		t.Fatal(err)
	}
	if submissions.Load() != 1 {
		t.Fatal("speech resume resubmitted")
	}
}

func TestTranscriptionNativeWireAndResume(t *testing.T) {
	var submissions atomic.Int32
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			submissions.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] != nil || body["word_timestamps"] != true || body["diarization"] != true || body["smart"] != false || body["inputs"].([]any)[0] != "data:audio/mpeg;base64,SUQz" {
				t.Errorf("unexpected transcription body: %#v", body)
			}
			fmt.Fprint(w, `{"id":"transcription-1"}`)
			return
		}
		fmt.Fprint(w, `{"id":"transcription-1","status":"succeeded","results":[{"type":"text","text":"hello","transcription":{"language":"en","duration_seconds":0.5,"word_timestamps":true,"segments":[{"text":"hello","speaker":"speaker-1","words":[{"text":"hello","start_seconds":0,"end_seconds":0.5}]}]}}],"usage":{"credits":10}}`)
	})
	request := &schemas.BifrostTranscriptionRequest{Model: "aistudio/gemini-3.5-transcribe", Input: &schemas.TranscriptionInput{File: []byte("ID3"), Filename: "audio.mp3"}}
	result, err := provider.Transcription(ctx, fixtureKey("private"), request)
	if err != nil || result.Text != "hello" || len(result.Words) != 1 || *result.Words[0].Speaker != "speaker-1" || result.Usage == nil || result.Usage.Seconds == nil || *result.Usage.Seconds != .5 || result.Usage.Cost != nil {
		t.Fatalf("transcript result: %#v %#v", result, err)
	}
	request.Params = &schemas.TranscriptionParameters{ExtraParams: map[string]any{"resume_task_id": "transcription-1"}}
	if _, err := provider.Transcription(ctx, fixtureKey("private"), request); err != nil {
		t.Fatal(err)
	}
	if submissions.Load() != 1 {
		t.Fatal("transcription resume resubmitted")
	}
}

func TestImageEditAndBase64Result(t *testing.T) {
	var serverURL string
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/picture" {
			fmt.Fprint(w, "png")
			return
		}
		if r.Method == "POST" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body["inputs"].([]any)) != 1 {
				t.Error("edit image missing")
			}
			fmt.Fprint(w, `{"id":"edit-1"}`)
			return
		}
		fmt.Fprintf(w, `{"id":"edit-1","status":"succeeded","results":[{"type":"image","download_url":%q}]}`, serverURL+"/picture")
	})
	serverURL = provider.networkConfig.BaseURL
	request := &schemas.BifrostImageEditRequest{Model: "flower-image", Input: &schemas.ImageEditInput{Prompt: "edit flower", Images: []schemas.ImageInput{{URL: "data:image/png;base64,cG5n"}}}, Params: &schemas.ImageEditParameters{ResponseFormat: schemas.Ptr("b64_json")}}
	result, err := provider.ImageEdit(ctx, fixtureKey("key"), request)
	if err != nil || result.Data[0].B64JSON != "cG5n" || result.Data[0].URL != "" {
		t.Fatalf("edit result: %#v %#v", result, err)
	}
}

func TestUnknownSubmissionAndMalformedReceiptAreTerminal(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{{"server error", 503, `{"message":"unavailable"}`}, {"timeout", 408, `{"message":"timeout"}`}, {"conflict", 409, `{"message":"processing"}`}, {"missing id", 200, `{}`}, {"invalid JSON", 200, `broken`}} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			})
			_, err := provider.VideoGeneration(ctx, fixtureKey("key"), &schemas.BifrostVideoGenerationRequest{Model: "flow-video-fast", Input: &schemas.VideoGenerationInput{Prompt: "scene"}})
			if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || requests.Load() != 1 {
				t.Fatalf("unsafe submission: %#v", err)
			}
		})
	}
}

func TestStaleConnectionDoesNotRepeatPaidPOST(t *testing.T) {
	var submissions atomic.Int32
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `[]`)
			return
		}
		submissions.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	// Prime a live keepalive connection so the next request exercises the stale
	// connection retry callback after the server has consumed the complete POST.
	if _, _, err := provider.request(ctx, fixtureKey("key"), http.MethodGet, "/api/v6/models", nil, false); err != nil {
		t.Fatal(err)
	}
	_, err := provider.VideoGeneration(ctx, fixtureKey("key"), &schemas.BifrostVideoGenerationRequest{Model: "flow-video-fast", Input: &schemas.VideoGenerationInput{Prompt: "scene"}})
	if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || submissions.Load() != 1 {
		t.Fatalf("paid POST was repeated after response loss: submissions=%d error=%#v", submissions.Load(), err)
	}
}

func TestModelDiscoveryUsesKeyFilters(t *testing.T) {
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v6/models" || r.Header.Get("X-API-Key") != "catalog" {
			t.Errorf("wrong models request")
		}
		fmt.Fprint(w, `[{"id":"flower-image","display_name":"Flower"},{"id":"disabled","available":false}]`)
	})
	key := fixtureKey("catalog")
	result, err := provider.ListModels(ctx, []schemas.Key{key}, &schemas.BifrostListModelsRequest{Unfiltered: true})
	if err != nil || len(result.Data) != 1 || result.Data[0].ID != "fastgen/flower-image" {
		t.Fatalf("models: %#v %#v", result, err)
	}
}

func BenchmarkFastgenImageConverter(b *testing.B) {
	request := imageRequest()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToFastgenImageGenerationRequest(request); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkFastgenPromptConverter(b *testing.B) {
	request := promptRequest("Expand this scene")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToFastgenChatRequest(request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFastgenTranscriptionConverter(b *testing.B) {
	native := &FastgenGenerationResponse{ID: "transcript-usage", Status: "succeeded", Results: []FastgenResult{{Type: "text", Text: "hello", Transcription: &FastgenTranscript{DurationSeconds: schemas.Ptr(0.5)}}}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToBifrostTranscriptionResponse(native, false); err != nil {
			b.Fatal(err)
		}
	}
}

func TestFastgenVoiceCatalogAndExistingPreview(t *testing.T) {
	const model = "aistudio/gemini-2.5-flash-preview-tts"
	var catalogs, previews atomic.Int32
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatalf("voice discovery/preview must never submit generation: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v6/voices":
			catalogs.Add(1)
			if r.Header.Get("X-API-Key") != "voices-key" || r.URL.Query().Get("model") != model || r.URL.Query().Get("url") != "" {
				t.Errorf("wrong voice catalog auth/query: %s", r.URL)
			}
			fmt.Fprintf(w, `[{"id":"Kore","name":"Kore","provider":"aistudio","models":[%q],"language":"en","gender":"female","preview_urls":{"en":"/samples/kore.mp3"}},{"id":"other","name":"Other","provider":"other","models":[%q]}]`, model, model)
		case "/samples/kore.mp3":
			previews.Add(1)
			if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Private") != "" {
				t.Error("provider/context credentials leaked to public preview")
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "existing-sample")
		default:
			t.Errorf("unexpected voice route %s", r.URL)
		}
	})
	ctx.SetValue(schemas.BifrostContextKeyExtraHeaders, map[string]string{"X-Private": "private"})
	request := &schemas.BifrostPassthroughRequest{Provider: schemas.Fastgen, Model: model, Method: "GET", Path: "/v1/audio/voices", RawQuery: "language=en&url=http://untrusted.test/private"}
	response, err := provider.Passthrough(ctx, fixtureKey("voices-key"), request)
	if err != nil {
		t.Fatalf("voice catalog: %#v", err)
	}
	var result struct {
		Object string `json:"object"`
		Data   []struct {
			ID, Name, Language, Gender string
			PreviewURL                 string `json:"preview_url"`
		} `json:"data"`
	}
	if json.Unmarshal(response.Body, &result) != nil || response.StatusCode != 200 || result.Object != "list" || len(result.Data) != 1 || result.Data[0].ID != "Kore" || result.Data[0].Language != "en" || result.Data[0].Gender != "female" || !strings.HasPrefix(result.Data[0].PreviewURL, "/v1/audio/voices/Kore/preview?") || !strings.Contains(result.Data[0].PreviewURL, "provider=fastgen") {
		t.Fatalf("wrong normalized voice catalog: %s", response.Body)
	}
	request.Path = "/v1/audio/voices/Kore/preview"
	response, err = provider.Passthrough(ctx, fixtureKey("voices-key"), request)
	if err != nil || string(response.Body) != "existing-sample" || response.Headers["Content-Type"] != "audio/mpeg" || catalogs.Load() != 2 || previews.Load() != 1 {
		t.Fatalf("preview did not use catalog sample: %+v %+v", response, err)
	}
	request.Path = "/v1/audio/voices/missing/preview"
	if _, err = provider.Passthrough(ctx, fixtureKey("voices-key"), request); err == nil || err.StatusCode == nil || *err.StatusCode != 404 || previews.Load() != 1 {
		t.Fatal("missing voice must not generate a preview")
	}
}

func TestFastgenVoiceGuardsAndUntrustedSample(t *testing.T) {
	var requests atomic.Int32
	provider, ctx := fixtureProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/v6/voices" || r.Method != "GET" {
			t.Errorf("unexpected sample fetch/submit: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `[{"id":"Kore","name":"Kore","provider":"aistudio","models":["speech-model"],"preview_url":"http://untrusted.test/private"}]`)
	})
	for _, request := range []*schemas.BifrostPassthroughRequest{
		{Method: "GET", Path: "/v1/audio/voices", RawQuery: "cursor=not-a-page", Model: "speech-model"},
		{Method: "POST", Path: "/v1/audio/voices", Model: "speech-model"},
		{Method: "GET", Path: "/v1/audio/voices/..%2Fprivate/preview", Model: "speech-model"},
		{Method: "GET", Path: "/v1/audio/voices"},
	} {
		if _, err := provider.Passthrough(ctx, fixtureKey("key"), request); err == nil || requests.Load() != 0 {
			t.Fatalf("invalid voice request reached upstream: %+v", request)
		}
	}
	request := &schemas.BifrostPassthroughRequest{Method: "GET", Path: "/v1/audio/voices/Kore/preview", Model: "speech-model", UpstreamURL: "http://untrusted.test/private", RawQuery: "url=http://untrusted.test/private"}
	if _, err := provider.Passthrough(ctx, fixtureKey("key"), request); err == nil || err.StatusCode == nil || *err.StatusCode != 404 || requests.Load() != 1 {
		t.Fatalf("untrusted sample should remain unavailable: %+v", err)
	}
}

func BenchmarkFastgenVoiceCatalogConversion(b *testing.B) {
	rows := []FastgenVoice{{ID: "Kore", Name: "Kore", Provider: "aistudio", Models: []string{"speech-model"}, PreviewURL: "https://storage.fast-gen.ai/preview.mp3"}}
	options := providerUtils.VoiceCatalogOptions{Model: "speech-model", Source: "aistudio", Page: 1}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := ToBifrostVoiceCatalog(rows, options, "https://api.fast-gen.ai"); err != nil {
			b.Fatal(err)
		}
	}
}
