package fal

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

func fixture(t *testing.T, handler http.HandlerFunc) (*FalProvider, *schemas.BifrostContext) {
	t.Helper()
	return fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models/billing-events" {
			fmt.Fprint(w, `{"billing_events":[],"has_more":false}`)
			return
		}
		handler(w, r)
	})
}
func fixturePlatform(t *testing.T, handler http.HandlerFunc) (*FalProvider, *schemas.BifrostContext) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewFalProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true, DefaultRequestTimeoutInSeconds: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider.pollInterval = time.Millisecond
	return provider, schemas.NewBifrostContext(context.Background(), time.Time{})
}

func TestLiveCatalogPaginatesAndPreservesKeyAliases(t *testing.T) {
	var pages atomic.Int32
	provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Key catalog-key" {
			t.Errorf("wrong live catalog request %s", r.URL.Path)
		}
		pages.Add(1)
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"models":[{"endpoint_id":"custom/live-a","metadata":{"display_name":"Live A","category":"text-to-image","description":"fresh model"}}],"has_more":true,"next_cursor":"next-page"}`)
		} else if r.URL.Query().Get("cursor") == "next-page" {
			fmt.Fprint(w, `{"models":[{"endpoint_id":"custom/live-b","metadata":{"display_name":"Live B","status":"deprecated"}}],"has_more":false}`)
		} else {
			t.Errorf("unexpected cursor %q", r.URL.RawQuery)
		}
	})
	key := fixtureKey("catalog-key")
	if err := json.Unmarshal([]byte(`{"models":["*"],"aliases":{"friendly":{"model_id":"custom/live-a"}},"blacklisted_models":["custom/live-b"]}`), &key); err != nil {
		t.Fatal(err)
	}
	key.Value = schemas.SecretVar{Val: "catalog-key"}
	result, err := provider.ListModels(ctx, []schemas.Key{key}, &schemas.BifrostListModelsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if pages.Load() != 2 || len(result.Data) != 2 {
		t.Fatalf("catalog was not paginated and filtered: pages=%d rows=%#v", pages.Load(), result.Data)
	}
	var found bool
	for _, row := range result.Data {
		if row.ID == "fal/friendly" {
			found = row.Alias != nil && *row.Alias == "custom/live-a" && row.Name != nil && *row.Name == "Live A"
		}
	}
	if !found {
		t.Fatalf("live model metadata/alias missing: %#v", result.Data)
	}
}

func TestImageActualBillingUsesEveryMatchedPage(t *testing.T) {
	var submits, pages atomic.Int32
	provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fal-ai/flux/dev":
			submits.Add(1)
			fmt.Fprint(w, `{"request_id":"image-actual"}`)
		case "/fal-ai/flux/requests/image-actual/status":
			fmt.Fprint(w, `{"status":"COMPLETED"}`)
		case "/fal-ai/flux/requests/image-actual":
			fmt.Fprint(w, `{"images":[{"url":"https://v3.fal.media/image.png"}]}`)
		case "/v1/models/billing-events":
			if r.Header.Get("Authorization") != "Key selected" || r.URL.Query().Get("source") != "billed" || r.URL.Query().Get("request_id") != "image-actual" || r.URL.Query().Get("endpoint_id") != "fal-ai/flux/dev" {
				t.Errorf("billing not scoped to native receipt: %s", r.URL.RawQuery)
			}
			pages.Add(1)
			if r.URL.Query().Get("cursor") == "" {
				fmt.Fprint(w, `{"billing_events":[{"request_id":"wrong","endpoint_id":"fal-ai/flux/dev","cost_total":99},{"request_id":"image-actual","endpoint_id":"wrong/model","cost_total":99},{"request_id":"image-actual","endpoint_id":"fal-ai/flux/dev","quantity":1,"unit":"image","cost_total":0.1,"cost_estimate_nano_usd":100000000}],"has_more":true,"next_cursor":"page-2"}`)
			} else {
				fmt.Fprint(w, `{"billing_events":[{"request_id":"image-actual","endpoint_id":"fal-ai/flux/dev","quantity":2,"unit":"image","cost_total":0.2,"cost_estimate_nano_usd":200000000}],"has_more":false}`)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	response, err := provider.ImageGeneration(ctx, fixtureKey("selected"), &schemas.BifrostImageGenerationRequest{Model: "fal-ai/flux/dev", Input: &schemas.ImageGenerationInput{Prompt: "image"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage == nil || response.Usage.Cost == nil || response.Usage.Cost.TotalCost != 0.3 || pages.Load() != 2 || submits.Load() != 1 {
		t.Fatalf("actual billing not forwarded: %#v pages=%d submits=%d", response.Usage, pages.Load(), submits.Load())
	}
	if cost := response.Usage.Cost; cost.InputCost != 0 || cost.OutputCost != 0 || cost.AdditionalCost != 0 {
		t.Fatalf("total-only billing invented cost categories: %#v", cost)
	}
}

func TestVideoActualZeroAndBillingPendingStayDistinct(t *testing.T) {
	for _, test := range []struct {
		name, events string
		status       int
		actual       bool
	}{{"actual zero", `{"billing_events":[{"request_id":"vid","endpoint_id":"fal-ai/veo3.1","cost_total":0}],"has_more":false}`, 200, true}, {"pending", `{"billing_events":[],"has_more":false}`, 200, false}, {"forbidden", `{"message":"denied"}`, 403, false}} {
		t.Run(test.name, func(t *testing.T) {
			provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/status"):
					fmt.Fprint(w, `{"status":"COMPLETED"}`)
				case r.URL.Path == "/v1/models/billing-events":
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.events)
				default:
					fmt.Fprint(w, `{"video":{"url":"https://v3.fal.media/video.mp4"}}`)
				}
			})
			response, err := provider.VideoRetrieve(ctx, fixtureKey("key"), &schemas.BifrostVideoRetrieveRequest{ID: encodeJobID("fal-ai/veo3.1", "vid")})
			if err != nil {
				t.Fatal(err)
			}
			hasCost := response.Usage != nil && response.Usage.Cost != nil
			if hasCost != test.actual {
				t.Fatalf("actual zero collapsed into unknown: %#v", response.Usage)
			}
		})
	}
}
func fixtureKey(value string) schemas.Key { return schemas.Key{Value: schemas.SecretVar{Val: value}} }

func TestQueueEndpointPathsAndVideoIDs(t *testing.T) {
	for _, test := range []struct{ model, submit, base string }{{"fal-ai/flux/schnell", "/fal-ai/flux/schnell", "/fal-ai/flux"}, {"workflows/user/movie/v2", "/workflows/user/movie/v2", "/workflows/user/movie"}, {"comfy/user/graph", "/comfy/user/graph", "/comfy/user/graph"}} {
		submit, base, err := endpointPaths(test.model)
		if err != nil || submit != test.submit || base != test.base {
			t.Fatalf("endpoint %q: %q %q %v", test.model, submit, base, err)
		}
		id := encodeJobID(test.model, "native-id")
		reference, err := decodeJobID(id)
		if err != nil || reference.Model != test.model || reference.RequestID != "native-id" {
			t.Fatalf("video identity lost: %#v %v", reference, err)
		}
	}
	if _, _, err := endpointPaths("fal-ai/../../admin"); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}

func TestImageQueueNativeWireReceiptAndResume(t *testing.T) {
	var submissions atomic.Int32
	var polls atomic.Int32
	provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Key tenant" {
			t.Errorf("missing selected key")
		}
		switch r.URL.Path {
		case "/fal-ai/flux/schnell":
			submissions.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] != "flower" || body["num_images"] != float64(1) || body["seed"] != float64(7) {
				t.Errorf("wrong native payload: %#v", body)
			}
			fmt.Fprint(w, `{"request_id":"job-1","status":"IN_QUEUE"}`)
		case "/fal-ai/flux/requests/job-1/status":
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"status":"IN_PROGRESS"}`)
			} else {
				fmt.Fprint(w, `{"status":"COMPLETED"}`)
			}
		case "/fal-ai/flux/requests/job-1":
			fmt.Fprint(w, `{"images":[{"url":"https://v3.fal.media/image.png"}]}`)
		default:
			t.Errorf("wrong queue path %s", r.URL.Path)
		}
	})
	request := &schemas.BifrostImageGenerationRequest{Model: "fal-ai/flux/schnell", Input: &schemas.ImageGenerationInput{Prompt: "flower"}, Params: &schemas.ImageGenerationParameters{Seed: schemas.Ptr(7)}}
	result, err := provider.ImageGeneration(ctx, fixtureKey("tenant"), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "job-1" || result.Data[0].URL != "https://v3.fal.media/image.png" || result.ExtraFields.ProviderResponseHeaders["x-bf-provider-task-id"] != "job-1" {
		t.Fatalf("unexpected result %#v", result)
	}
	request.Params.ExtraParams = map[string]any{"resume_task_id": "job-1"}
	if _, err := provider.ImageGeneration(ctx, fixtureKey("tenant"), request); err != nil {
		t.Fatal(err)
	}
	if submissions.Load() != 1 {
		t.Fatal("resume resubmitted paid inference")
	}
}

func TestVideoQueueLifecycleAndDownload(t *testing.T) {
	var serverURL string
	var submissions atomic.Int32
	provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fal-ai/kling-video/v2.6/pro/text-to-video":
			submissions.Add(1)
			fmt.Fprint(w, `{"request_id":"video-1"}`)
		case "/fal-ai/kling-video/requests/video-1/status":
			fmt.Fprint(w, `{"request_id":"video-1","status":"COMPLETED"}`)
		case "/fal-ai/kling-video/requests/video-1":
			fmt.Fprintf(w, `{"video":{"url":%q,"content_type":"video/mp4"}}`, serverURL+"/asset.mp4")
		case "/asset.mp4":
			if r.Header.Get("Authorization") != "" {
				t.Error("selected key leaked to media URL")
			}
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprint(w, "video")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	serverURL = provider.networkConfig.BaseURL
	request := &schemas.BifrostVideoGenerationRequest{Model: "fal-ai/kling-video/v2.6/pro/text-to-video", Input: &schemas.VideoGenerationInput{Prompt: "scene"}}
	video, err := provider.VideoGeneration(ctx, fixtureKey("secret"), request)
	if err != nil {
		t.Fatal(err)
	}
	if video.Status != schemas.VideoStatusQueued {
		t.Fatalf("submit blocked until completion: %#v", video)
	}
	download, err := provider.VideoDownload(ctx, fixtureKey("secret"), &schemas.BifrostVideoDownloadRequest{ID: video.ID})
	if err != nil {
		t.Fatal(err)
	}
	if string(download.Content) != "video" || download.ContentType != "video/mp4" || submissions.Load() != 1 {
		t.Fatalf("download: %#v", download)
	}
}

func TestSpeechAndAudioUseNativeFields(t *testing.T) {
	for _, model := range []string{"fal-ai/kokoro/american-english", "fal-ai/elevenlabs/music"} {
		t.Run(model, func(t *testing.T) {
			var serverURL string
			provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/audio.wav" {
					if r.Header.Get("Authorization") != "" {
						t.Error("audio URL leaked key")
					}
					w.Header().Set("Content-Type", "audio/wav")
					fmt.Fprint(w, "audio")
					return
				}
				if r.Method == "POST" {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["prompt"] != "hello" || (!audioGeneration(model) && body["text"] != "hello") {
						t.Errorf("wrong audio request %#v", body)
					}
					if audioGeneration(model) && body["music_length_ms"] != float64(5000) {
						t.Error("native music options lost")
					}
					fmt.Fprint(w, `{"request_id":"audio-1"}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/status") {
					fmt.Fprint(w, `{"status":"COMPLETED"}`)
					return
				}
				fmt.Fprintf(w, `{"audio":{"url":%q,"content_type":"audio/wav"}}`, serverURL+"/audio.wav")
			})
			serverURL = provider.networkConfig.BaseURL
			request := &schemas.BifrostSpeechRequest{Model: model, Input: &schemas.SpeechInput{Input: "hello"}, Params: &schemas.SpeechParameters{ExtraParams: map[string]any{"music_length_ms": 5000}}}
			result, err := provider.Speech(ctx, fixtureKey("private"), request)
			if err != nil || string(result.Audio) != "audio" || result.ExtraFields.ProviderResponseHeaders["content-type"] != "audio/wav" {
				t.Fatalf("audio result %#v %#v", result, err)
			}
		})
	}
}

func TestTranscriptionUploadsBytesBeforeInference(t *testing.T) {
	var serverURL string
	var uploads, submissions atomic.Int32
	provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/storage/upload/initiate":
			if r.Header.Get("Authorization") != "Key tenant" || r.URL.Query().Get("storage_type") != "fal-cdn-v3" {
				t.Error("wrong upload initiation")
			}
			fmt.Fprintf(w, `{"upload_url":%q,"file_url":%q}`, serverURL+"/signed-upload", serverURL+"/input.mp3")
		case "/signed-upload":
			uploads.Add(1)
			if r.Method != "PUT" || r.Header.Get("Authorization") != "" {
				t.Error("upload used credentials")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != "audio-bytes" {
				t.Error("upload bytes changed")
			}
			w.WriteHeader(204)
		case "/fal-ai/whisper":
			submissions.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["audio_url"] != serverURL+"/input.mp3" || body["language"] != "en" {
				t.Errorf("STT was not a storage URL: %#v", body)
			}
			fmt.Fprint(w, `{"request_id":"stt-1"}`)
		case "/fal-ai/whisper/requests/stt-1/status":
			fmt.Fprint(w, `{"status":"COMPLETED"}`)
		case "/fal-ai/whisper/requests/stt-1":
			fmt.Fprint(w, `{"text":"hello","inferred_languages":["en"],"chunks":[{"text":"hello","timestamp":[0,1.5]}]}`)
		default:
			t.Errorf("unknown path %s", r.URL.Path)
		}
	})
	serverURL = provider.networkConfig.BaseURL
	request := &schemas.BifrostTranscriptionRequest{Model: "fal-ai/whisper", Input: &schemas.TranscriptionInput{File: []byte("audio-bytes"), Filename: "audio.mp3"}, Params: &schemas.TranscriptionParameters{Language: schemas.Ptr("en")}}
	result, err := provider.Transcription(ctx, fixtureKey("tenant"), request)
	if err != nil || result.Text != "hello" || result.Language == nil || *result.Language != "en" || len(result.Segments) != 1 || result.Segments[0].End != 1.5 {
		t.Fatalf("STT result %#v %#v", result, err)
	}
	request.Params.ExtraParams = map[string]any{"resume_task_id": "stt-1"}
	if _, err := provider.Transcription(ctx, fixtureKey("tenant"), request); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 1 || submissions.Load() != 1 {
		t.Fatal("STT resume repeated upload or paid inference")
	}
}

func TestUnknownSubmissionAndAcceptedErrorsStopRetries(t *testing.T) {
	for _, status := range []int{408, 409, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"detail":"unknown outcome"}`)
			})
			_, err := provider.VideoGeneration(ctx, fixtureKey("key"), &schemas.BifrostVideoGenerationRequest{Model: "fal-ai/veo3.1", Input: &schemas.VideoGenerationInput{Prompt: "scene"}})
			if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks {
				t.Fatalf("unsafe retry: %#v", err)
			}
		})
	}
	provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			fmt.Fprint(w, `{"request_id":"accepted-1"}`)
			return
		}
		w.WriteHeader(503)
		fmt.Fprint(w, `{"message":"later"}`)
	})
	_, err := provider.ImageGeneration(ctx, fixtureKey("key"), &schemas.BifrostImageGenerationRequest{Model: "fal-ai/flux/schnell", Input: &schemas.ImageGenerationInput{Prompt: "scene"}})
	if err == nil || !err.IsBifrostError || err.AllowFallbacks == nil || *err.AllowFallbacks || !strings.Contains(err.Error.Message, "accepted-1") {
		t.Fatalf("accepted id not retained: %#v", err)
	}
}

func TestStaleConnectionNeverRepeatsPaidPOST(t *testing.T) {
	var submissions atomic.Int32
	provider, ctx := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{}`)
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
	if _, _, err := provider.request(ctx, fixtureKey("key"), "GET", provider.networkConfig.BaseURL+"/prime", nil, "", true, false); err != nil {
		t.Fatal(err)
	}
	_, err := provider.VideoGeneration(ctx, fixtureKey("key"), &schemas.BifrostVideoGenerationRequest{Model: "fal-ai/veo3.1", Input: &schemas.VideoGenerationInput{Prompt: "scene"}})
	if err == nil || submissions.Load() != 1 {
		t.Fatalf("duplicate paid submission count=%d err=%#v", submissions.Load(), err)
	}
}

func TestConcurrentTenantCredentialsStayScoped(t *testing.T) {
	provider, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Key "+body["prompt"].(string) {
			t.Error("cross-tenant key")
		}
		fmt.Fprint(w, `{"request_id":"native"}`)
	})
	var wg sync.WaitGroup
	for index := 0; index < 12; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", index)
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			_, err := provider.VideoGeneration(ctx, fixtureKey(tenant), &schemas.BifrostVideoGenerationRequest{Model: "fal-ai/veo3.1", Input: &schemas.VideoGenerationInput{Prompt: tenant}})
			if err != nil {
				t.Error(err)
			}
		}(index)
	}
	wg.Wait()
}

func TestImageEditNativeOverridesAndRawInput(t *testing.T) {
	nano, err := ToFalImageEditRequest(&schemas.BifrostImageEditRequest{Model: "fal-ai/nano-banana/edit", Input: &schemas.ImageEditInput{Images: []schemas.ImageInput{{URL: "https://example.com/reference.png"}}}})
	if err != nil || len(nano.Fields["image_urls"].([]string)) != 1 || nano.Fields["image_url"] != nil {
		t.Fatalf("nano-banana reference must be a native array: %#v %v", nano, err)
	}
	veo, err := ToFalVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{Model: "fal-ai/veo3.1", Params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("8")}})
	if err != nil || veo.Fields["duration"] != "8s" {
		t.Fatalf("veo duration unit: %#v %v", veo, err)
	}
	converted, err := ToFalImageEditRequest(&schemas.BifrostImageEditRequest{Model: "fal-ai/nano-banana/edit", Input: &schemas.ImageEditInput{Prompt: "change", Images: []schemas.ImageInput{{URL: "https://example.com/image.png"}}}, Params: &schemas.ImageEditParameters{Mask: []byte("mask"), ExtraParams: map[string]any{"image_url": "custom-reference"}}})
	if err != nil {
		t.Fatal(err)
	}
	if converted.Fields["image_url"] != "custom-reference" || converted.Fields["mask_url"] != "data:image/png;base64,bWFzaw==" {
		t.Fatalf("native edit mapping: %#v", converted.Fields)
	}
	response, err := ToBifrostImageGenerationResponse(&FalResult{Image: &FalImage{URL: "data:image/png;base64,cGlj"}}, "id", "model")
	if err != nil || response.Data[0].B64JSON != "cGlj" {
		t.Fatal("embedded image mapping")
	}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	request := &schemas.BifrostVideoGenerationRequest{Model: "fal-ai/veo3.1", RawRequestBody: []byte(`{"native":true}`)}
	body, bErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		t.Fatal("raw body converted")
		return nil, nil
	})
	if bErr != nil || string(body) != `{"native":true}` {
		t.Fatal("raw native input changed")
	}
}

func TestLiveCatalogBackfillsConfiguredCustomModels(t *testing.T) {
	var key schemas.Key
	if err := json.Unmarshal([]byte(`{"models":["custom/endpoint"]}`), &key); err != nil {
		t.Fatal(err)
	}
	result := ToBifrostListModelsResponse(nil, key, false)
	if len(result.Data) != 1 || result.Data[0].ID != "fal/custom/endpoint" {
		t.Fatalf("custom endpoint missing: %#v", result.Data)
	}
}

func TestSpeechAndTranscriptionForwardNativeDurationAndActualCost(t *testing.T) {
	for _, operation := range []string{"speech", "transcription"} {
		t.Run(operation, func(t *testing.T) {
			model := "fal-ai/kokoro/american-english"
			if operation == "transcription" {
				model = "fal-ai/whisper"
			}
			var submits atomic.Int32
			var serverURL string
			provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/audio.wav" {
					w.Header().Set("Content-Type", "audio/wav")
					fmt.Fprint(w, "audio")
					return
				}
				if r.URL.Path == "/v1/models/billing-events" {
					fmt.Fprintf(w, `{"billing_events":[{"request_id":"duration-job","endpoint_id":%q,"quantity":1.25,"unit":"second","cost_total":0.005}],"has_more":false}`, model)
					return
				}
				if r.Method == "POST" {
					submits.Add(1)
					fmt.Fprint(w, `{"request_id":"duration-job"}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/status") {
					fmt.Fprint(w, `{"status":"COMPLETED"}`)
					return
				}
				if operation == "speech" {
					fmt.Fprintf(w, `{"audio":{"url":%q,"duration":1.25}}`, serverURL+"/audio.wav")
				} else {
					fmt.Fprint(w, `{"text":"hi","duration":1.25}`)
				}
			})
			serverURL = provider.networkConfig.BaseURL
			if operation == "speech" {
				response, err := provider.Speech(ctx, fixtureKey("key"), &schemas.BifrostSpeechRequest{Model: model, Input: &schemas.SpeechInput{Input: "hello"}})
				if err != nil {
					t.Fatal(err)
				}
				if response.Usage == nil || response.Usage.Cost == nil || response.Usage.Cost.TotalCost != 0.005 || response.Usage.AudioSeconds != 0 || response.Usage.InputChars != 5 || response.ExtraFields.ProviderResponseHeaders["x-bf-fal-audio-seconds"] != "1.25" {
					t.Fatalf("speech usage invented/rounded/missing: %#v", response)
				}
			} else {
				response, err := provider.Transcription(ctx, fixtureKey("key"), &schemas.BifrostTranscriptionRequest{Model: model, Params: &schemas.TranscriptionParameters{ExtraParams: map[string]any{"audio_url": "https://v3.fal.media/source.mp3"}}})
				if err != nil {
					t.Fatal(err)
				}
				if response.Duration == nil || *response.Duration != 1.25 || response.Usage == nil || response.Usage.Seconds == nil || *response.Usage.Seconds != 1.25 || response.Usage.Cost == nil || response.Usage.Cost.TotalCost != 0.005 {
					t.Fatalf("STT exact usage missing: %#v", response)
				}
			}
			if submits.Load() != 1 {
				t.Fatal("billing repeated inference")
			}
		})
	}
}

func TestVideoFailedResultStillReconcilesActualBilling(t *testing.T) {
	provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"status":"COMPLETED"}`)
		case r.URL.Path == "/v1/models/billing-events":
			fmt.Fprint(w, `{"billing_events":[{"request_id":"failed-job","endpoint_id":"fal-ai/veo3.1","cost_total":0}],"has_more":false}`)
		default:
			w.WriteHeader(422)
			fmt.Fprint(w, `{"detail":"generation failed"}`)
		}
	})
	response, err := provider.VideoRetrieve(ctx, fixtureKey("key"), &schemas.BifrostVideoRetrieveRequest{ID: encodeJobID("fal-ai/veo3.1", "failed-job")})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != schemas.VideoStatusFailed || response.Error == nil || response.Usage == nil || response.Usage.Cost == nil || response.Usage.Cost.TotalCost != 0 {
		t.Fatalf("failed video did not reconcile actual zero: %#v", response)
	}
}

func TestBillingFailuresLeaveSuccessfulMediaAndNeverResubmit(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{{"forbidden", `{}`, 403}, {"delayed", `{"billing_events":[],"has_more":false}`, 200}, {"missing final cost", `{"billing_events":[{"request_id":"one","endpoint_id":"fal-ai/flux/dev","quantity":1,"unit":"image"}],"has_more":false}`, 200}, {"negative cost", `{"billing_events":[{"request_id":"one","endpoint_id":"fal-ai/flux/dev","cost_total":-1}],"has_more":false}`, 200}, {"repeated cursor", `{"billing_events":[{"request_id":"one","endpoint_id":"fal-ai/flux/dev","cost_total":1}],"has_more":true,"next_cursor":"same"}`, 200}} {
		t.Run(test.name, func(t *testing.T) {
			var submits atomic.Int32
			provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/models/billing-events":
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.body)
				case r.Method == "POST":
					submits.Add(1)
					fmt.Fprint(w, `{"request_id":"one"}`)
				case strings.HasSuffix(r.URL.Path, "/status"):
					fmt.Fprint(w, `{"status":"COMPLETED"}`)
				default:
					fmt.Fprint(w, `{"images":["https://v3.fal.media/image.png"]}`)
				}
			})
			response, err := provider.ImageGeneration(ctx, fixtureKey("key"), &schemas.BifrostImageGenerationRequest{Model: "fal-ai/flux/dev", Input: &schemas.ImageGenerationInput{Prompt: "image"}})
			if err != nil {
				t.Fatal(err)
			}
			if submits.Load() != 1 || len(response.Data) != 1 || response.Usage != nil && response.Usage.Cost != nil || response.ExtraFields.ProviderResponseHeaders["x-bf-fal-billing-status"] != "pending" {
				t.Fatalf("billing failure changed inference result: %#v", response)
			}
		})
	}
}

func TestCatalogRejectsPaginationCycles(t *testing.T) {
	provider, ctx := fixturePlatform(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[],"has_more":true,"next_cursor":"same"}`)
	})
	_, err := provider.ListModels(ctx, []schemas.Key{fixtureKey("key")}, &schemas.BifrostListModelsRequest{Unfiltered: true})
	if err == nil {
		t.Fatal("cyclic live catalog silently accepted")
	}
}

func BenchmarkFalActualBillingAggregation(b *testing.B) {
	events := []FalBillingEvent{{CostNanoUSD: "100000000", Quantity: "1", Unit: "second"}, {CostNanoUSD: "200000000", Quantity: "0.25", Unit: "second"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := billedUsage(events); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkFalCatalogConversion(b *testing.B) {
	rows := []FalModel{{EndpointID: "fal-ai/flux/dev", Metadata: FalModelMetadata{DisplayName: "Flux Dev", Category: "text-to-image"}}}
	var key schemas.Key
	b.ReportAllocs()
	for b.Loop() {
		_ = ToBifrostListModelsResponse(rows, key, true)
	}
}

func BenchmarkFalImageConverter(b *testing.B) {
	request := &schemas.BifrostImageGenerationRequest{Model: "fal-ai/flux/schnell", Input: &schemas.ImageGenerationInput{Prompt: "a flower"}, Params: &schemas.ImageGenerationParameters{Seed: schemas.Ptr(42)}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToFalImageGenerationRequest(request); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkFalVideoID(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if encodeJobID("fal-ai/veo3.1", "request-id") == "" {
			b.Fatal("missing id")
		}
	}
}
