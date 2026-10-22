package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// An uploaded source is carried as bytes; a referenced one keeps only the ID, with the provider
// suffix Bifrost stamps onto every video ID stripped back off before it reaches OpenAI.
func TestToOpenAIVideoEditRequest_SourceForms(t *testing.T) {
	uploaded, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{
			Prompt: "warm backlight",
			Video:  schemas.VideoInput{Video: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(uploaded.Video.Bytes) == 0 || uploaded.Video.ID != "" {
		t.Fatalf("uploaded source should carry bytes only, got %+v", uploaded.Video)
	}
	if uploaded.Prompt != "warm backlight" || uploaded.Model != "sora-2-pro" {
		t.Fatalf("prompt/model not carried: %+v", uploaded)
	}

	referenced, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Input: &schemas.VideoEditInput{
			Prompt: "warm backlight",
			Video:  schemas.VideoInput{ID: "video_abc123:openai"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if referenced.Video.ID != "video_abc123" {
		t.Fatalf("video id = %q, want the provider suffix stripped", referenced.Video.ID)
	}
	if referenced.Model != "" {
		t.Fatalf("model must stay empty so OpenAI infers it from the source video, got %q", referenced.Model)
	}
}

func TestCustomOpenAIVideoNativeJSONWire(t *testing.T) {
	schemas.RegisterKnownProvider("webchat-ai")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("webchat-ai") })
	for _, size := range []string{"360p", "1080p", "4k"} {
		t.Run(size, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("custom video without reference should use native JSON, got %q", r.Header.Get("Content-Type"))
					fmt.Fprint(w, `{"id":"job","status":"queued"}`)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != "veo-3.1-fast-generate-preview" || body["size"] != size || body["seconds"] != "8" || body["prompt"] != "scene" {
					t.Errorf("native body changed: %#v", body)
				}
				options, ok := body["provider_options"].(map[string]any)
				if !ok || options["aspect"] != "portrait" || options["seed"] != float64(17) {
					t.Errorf("provider_options lost: %#v", body)
				}
				if body["native_flag"] != true {
					t.Errorf("native ExtraParams lost: %#v", body)
				}
				fmt.Fprint(w, `{"id":"job","status":"queued","size":"360p"}`)
			}))
			defer server.Close()
			ingress := &OpenAIVideoGenerationRequest{Model: "webchat-ai/veo-3.1-fast-generate-preview", Prompt: "scene", VideoGenerationParameters: schemas.VideoGenerationParameters{Size: size, Seconds: schemas.Ptr("8")}, ExtraParams: map[string]any{"provider_options": map[string]any{"aspect": "portrait", "seed": 17}, "native_flag": true}}
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			request := ingress.ToBifrostVideoGenerationRequest(ctx)
			response, err := HandleOpenAIVideoGenerationRequest(ctx, &fasthttp.Client{}, server.URL+"/v1/videos", request, schemas.Key{}, nil, nil, "webchat-ai", false, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.ID != "job:webchat-ai" || response.Size != "360p" {
				t.Fatalf("job mapping changed: %#v", response)
			}
		})
	}
}

func TestCustomOpenAIVideoNativeMultipartWire(t *testing.T) {
	schemas.RegisterKnownProvider("webchat-ai")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("webchat-ai") })
	reference := []byte("\x89PNG\r\n\x1a\nreference")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			fmt.Fprint(w, `{"id":"job","status":"queued"}`)
			return
		}
		if r.FormValue("size") != "360p" || r.FormValue("seconds") != "8" || r.FormValue("model") != "veo-3.1-fast-generate-preview" {
			t.Errorf("native form changed: %#v", r.Form)
		}
		var options map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("provider_options")), &options); err != nil || options["aspect"] != "landscape" || options["seed"] != float64(7) {
			t.Errorf("provider_options was not a JSON form field: %q %v", r.FormValue("provider_options"), err)
		}
		if r.FormValue("native_flag") != "true" {
			t.Error("native form extra lost")
		}
		file, header, err := r.FormFile("input_reference")
		if err != nil {
			t.Error(err)
		} else {
			defer file.Close()
			body, _ := io.ReadAll(file)
			if !bytes.Equal(body, reference) || header.Header.Get("Content-Type") != "image/png" {
				t.Error("reference bytes/mime changed")
			}
		}
		fmt.Fprint(w, `{"id":"job","status":"queued"}`)
	}))
	defer server.Close()
	ingress := &OpenAIVideoGenerationRequest{Model: "webchat-ai/veo-3.1-fast-generate-preview", Prompt: "scene", InputReference: reference, VideoGenerationParameters: schemas.VideoGenerationParameters{Size: "360p", Seconds: schemas.Ptr("8")}, ExtraParams: map[string]any{"provider_options": map[string]any{"aspect": "landscape", "seed": 7}, "native_flag": true}}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	request := ingress.ToBifrostVideoGenerationRequest(ctx)
	if _, err := HandleOpenAIVideoGenerationRequest(ctx, &fasthttp.Client{}, server.URL+"/v1/videos", request, schemas.Key{}, nil, nil, "webchat-ai", false, false, nil); err != nil {
		t.Fatal(err)
	}
	if ingress.VideoGenerationParameters.ExtraParams != nil {
		t.Fatal("ingress parameters mutated")
	}
}

func TestOpenAIVideoBuiltinNormalizationStaysMultipart(t *testing.T) {
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure} {
		t.Run(string(provider), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("builtin video stopped using multipart: %v", err)
				}
				if r.FormValue("size") != string(DefaultOpenAIVideoSize) {
					t.Errorf("builtin size not normalized: %q", r.FormValue("size"))
				}
				if r.FormValue("provider_options") != "" {
					t.Error("custom-only extra written to built-in")
				}
				fmt.Fprint(w, `{"id":"job","status":"queued"}`)
			}))
			defer server.Close()
			request := &schemas.BifrostVideoGenerationRequest{Provider: provider, Model: "sora-2", Input: &schemas.VideoGenerationInput{Prompt: "scene"}, Params: &schemas.VideoGenerationParameters{Size: "360p", ExtraParams: map[string]any{"provider_options": map[string]any{"seed": 7}}}}
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			if _, err := HandleOpenAIVideoGenerationRequest(ctx, &fasthttp.Client{}, server.URL, request, schemas.Key{}, nil, nil, provider, false, false, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCustomOpenAIVideoStatusAndDownloadKeepOriginalJob(t *testing.T) {
	var submissions int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos":
			submissions++
			fmt.Fprint(w, `{"id":"original-job","model":"veo-3.1-fast-generate-preview","status":"queued"}`)
		case "/v1/videos/original-job":
			fmt.Fprint(w, `{"id":"original-job","model":"veo-3.1-fast-generate-preview","status":"completed","size":"4k","seconds":"8"}`)
		case "/v1/videos/original-job/content":
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprint(w, "original-mp4")
		default:
			t.Errorf("unexpected lifecycle path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	provider := NewOpenAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true}, CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: "webchat-ai", BaseProviderType: schemas.OpenAI}}, nil)
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	key := schemas.Key{Value: schemas.SecretVar{Val: "fixture"}}
	created, bErr := provider.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{Model: "veo-3.1-fast-generate-preview", Input: &schemas.VideoGenerationInput{Prompt: "scene"}, Params: &schemas.VideoGenerationParameters{Size: "4k"}})
	if bErr != nil {
		t.Fatal(bErr)
	}
	video, bErr := provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{ID: created.ID})
	if bErr != nil {
		t.Fatal(bErr)
	}
	content, bErr := provider.VideoDownload(ctx, key, &schemas.BifrostVideoDownloadRequest{ID: video.ID})
	if bErr != nil {
		t.Fatal(bErr)
	}
	if video.ID != "original-job:webchat-ai" || video.Size != "4k" || video.Seconds == nil || *video.Seconds != "8" || string(content.Content) != "original-mp4" || content.ContentType != "video/mp4" || content.VideoID != video.ID || submissions != 1 {
		t.Fatalf("custom lifecycle changed native job: video=%#v download=%#v submits=%d", video, content, submissions)
	}
}

func TestCustomOpenAIVideoIngressExtraParamsDoNotAliasMergedMap(t *testing.T) {
	ingress := &OpenAIVideoGenerationRequest{Model: "sora-2", VideoGenerationParameters: schemas.VideoGenerationParameters{ExtraParams: map[string]any{"embedded": "kept", "provider_options": "old"}}, ExtraParams: map[string]any{"provider_options": map[string]any{"aspect": "landscape"}}}
	converted := ingress.ToBifrostVideoGenerationRequest(schemas.NewBifrostContext(context.Background(), time.Time{}))
	if converted.Params.ExtraParams["embedded"] != "kept" {
		t.Fatal("embedded extras lost")
	}
	converted.Params.ExtraParams["embedded"] = "changed"
	if ingress.VideoGenerationParameters.ExtraParams["embedded"] != "kept" || ingress.VideoGenerationParameters.ExtraParams["provider_options"] != "old" {
		t.Fatal("merged ingress map mutated")
	}
}

func BenchmarkOpenAIVideoConverterNativeGuard(b *testing.B) {
	request := &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "sora-2", Input: &schemas.VideoGenerationInput{Prompt: "scene"}, Params: &schemas.VideoGenerationParameters{Size: "1280x720", Seconds: schemas.Ptr("8")}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := toOpenAIVideoGenerationRequest(request, customOpenAIVideoProvider(schemas.OpenAI)); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkOpenAIVideoCustomMultipart(b *testing.B) {
	request := &OpenAIVideoGenerationRequest{Model: "veo-3.1-fast-generate-preview", Prompt: "scene", InputReference: []byte("image"), VideoGenerationParameters: schemas.VideoGenerationParameters{Size: "360p", Seconds: schemas.Ptr("8")}, ExtraParams: map[string]any{"provider_options": map[string]any{"aspect": "landscape", "seed": 7}}}
	b.ReportAllocs()
	for b.Loop() {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := parseVideoGenerationFormDataBodyFromRequest(writer, request, "webchat-ai"); err != nil {
			b.Fatal(err)
		}
	}
}

// OpenAI's edit endpoint takes an uploaded file or a video ID. A URL has no equivalent, and it is
// the sole input rather than a droppable parameter, so it is rejected instead of ignored.
func TestToOpenAIVideoEditRequest_RejectsURL(t *testing.T) {
	_, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{
			Prompt: "warm backlight",
			Video:  schemas.VideoInput{URL: "https://example.com/clip.mp4"},
		},
	})
	if err == nil {
		t.Fatal("expected an error for a url source")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Fatalf("error should name the unsupported form, got %q", err.Error())
	}
}

func TestToOpenAIVideoEditRequest_RequiresPromptAndSource(t *testing.T) {
	if _, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{Video: schemas.VideoInput{ID: "video_abc123"}},
	}); err == nil {
		t.Fatal("expected an error when prompt is missing")
	}
	if _, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{Prompt: "warm backlight"},
	}); err == nil {
		t.Fatal("expected an error when the source video is missing")
	}
}

// The multipart body must name the file part "video" and give it a real video content type, since
// OpenAI reads the source from that part.
func TestParseVideoEditFormDataBodyFromRequest(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	bifrostErr := parseVideoEditFormDataBodyFromRequest(writer, &OpenAIVideoEditRequest{
		Prompt: "warm backlight",
		Model:  "sora-2-pro",
		Video:  OpenAIVideoEditVideoInput{Bytes: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
	})
	if bifrostErr != nil {
		t.Fatalf("unexpected error: %v", bifrostErr)
	}

	form, err := multipart.NewReader(&body, writer.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("produced an unreadable multipart body: %v", err)
	}
	if got := form.Value["prompt"]; len(got) != 1 || got[0] != "warm backlight" {
		t.Fatalf("prompt field = %v", got)
	}
	if got := form.Value["model"]; len(got) != 1 || got[0] != "sora-2-pro" {
		t.Fatalf("model field = %v", got)
	}
	videoParts := form.File["video"]
	if len(videoParts) != 1 {
		t.Fatalf("expected exactly one 'video' file part, got %d", len(videoParts))
	}
	if ct := videoParts[0].Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("video part content type = %q, want video/mp4", ct)
	}
	if fn := videoParts[0].Filename; !strings.HasSuffix(fn, ".mp4") {
		t.Fatalf("video part filename = %q, want an .mp4 extension", fn)
	}
}

// A model omitted for a referenced source must not be written as an empty form field.
func TestParseVideoEditFormDataBodyFromRequest_OmitsEmptyModel(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if bifrostErr := parseVideoEditFormDataBodyFromRequest(writer, &OpenAIVideoEditRequest{
		Prompt: "warm backlight",
		Video:  OpenAIVideoEditVideoInput{Bytes: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
	}); bifrostErr != nil {
		t.Fatalf("unexpected error: %v", bifrostErr)
	}
	form, err := multipart.NewReader(&body, writer.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("produced an unreadable multipart body: %v", err)
	}
	if _, ok := form.Value["model"]; ok {
		t.Fatalf("empty model must be omitted, got %v", form.Value["model"])
	}
}

// Inbound OpenAI-format requests split "provider/model" the same way the other video routes do.
func TestToBifrostVideoEditRequest(t *testing.T) {
	out := ToBifrostVideoEditRequest(&OpenAIVideoEditRequest{
		Prompt: "warm backlight",
		Model:  "openai/sora-2-pro",
		Video:  OpenAIVideoEditVideoInput{ID: "video_abc123"},
	})
	if out == nil {
		t.Fatal("expected a converted request")
	}
	if out.Provider != schemas.OpenAI || out.Model != "sora-2-pro" {
		t.Fatalf("provider/model = %q/%q, want openai/sora-2-pro", out.Provider, out.Model)
	}
	if out.Input == nil || out.Input.Video.ID != "video_abc123" || out.Input.Prompt != "warm backlight" {
		t.Fatalf("input not carried: %+v", out.Input)
	}
}

// The official SDKs send no model on the video edit route, so the provider the transport resolved
// from the query parameter, header, or video ID suffix is the only routing signal available.
func TestToBifrostVideoEditRequest_ProviderFallback(t *testing.T) {
	out := ToBifrostVideoEditRequest(&OpenAIVideoEditRequest{
		Prompt:   "make it grayscale",
		Video:    OpenAIVideoEditVideoInput{ID: "vid_123"},
		Provider: schemas.Runware,
	})
	if out.Provider != schemas.Runware {
		t.Fatalf("provider = %q, want runware", out.Provider)
	}
	if out.Model != "" {
		t.Fatalf("model should stay empty when none was sent, got %q", out.Model)
	}

	// An explicit "provider/model" string still wins over the resolved fallback.
	explicit := ToBifrostVideoEditRequest(&OpenAIVideoEditRequest{
		Prompt:   "make it grayscale",
		Video:    OpenAIVideoEditVideoInput{ID: "vid_123"},
		Model:    "replicate/some-model",
		Provider: schemas.Runware,
	})
	if explicit.Provider != schemas.Replicate || explicit.Model != "some-model" {
		t.Fatalf("model string should win, got provider=%q model=%q", explicit.Provider, explicit.Model)
	}
}
