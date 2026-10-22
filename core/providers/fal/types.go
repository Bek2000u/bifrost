package fal

import (
	"encoding/json"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
)

// FalRequest is the model-native JSON object, without an OpenAI wrapper.
type FalRequest struct{ Fields map[string]any }

func (r *FalRequest) GetExtraParams() map[string]any { return nil }
func (r *FalRequest) MarshalJSON() ([]byte, error)   { return providerUtils.MarshalSorted(r.Fields) }

type FalQueueResponse struct {
	RequestID     string `json:"request_id"`
	Status        string `json:"status"`
	ResponseURL   string `json:"response_url,omitempty"`
	StatusURL     string `json:"status_url,omitempty"`
	QueuePosition *int   `json:"queue_position,omitempty"`
	Error         string `json:"error,omitempty"`
	ErrorType     string `json:"error_type,omitempty"`
}
type FalFile struct {
	URL         string   `json:"url"`
	ContentType string   `json:"content_type,omitempty"`
	Duration    *float64 `json:"duration,omitempty"`
}
type FalImage struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
}

func (image *FalImage) UnmarshalJSON(data []byte) error {
	var target string
	if err := sonic.Unmarshal(data, &target); err == nil {
		image.URL = target
		return nil
	}
	type wire FalImage
	return sonic.Unmarshal(data, (*wire)(image))
}

type FalResult struct {
	Images            []FalImage           `json:"images,omitempty"`
	Image             *FalImage            `json:"image,omitempty"`
	Video             *FalFile             `json:"video,omitempty"`
	VideoURL          string               `json:"video_url,omitempty"`
	Audio             *FalFile             `json:"audio,omitempty"`
	AudioURL          string               `json:"audio_url,omitempty"`
	Text              *string              `json:"text,omitempty"`
	Language          *string              `json:"language,omitempty"`
	InferredLanguages []string             `json:"inferred_languages,omitempty"`
	Languages         []string             `json:"languages,omitempty"`
	Chunks            []FalTranscriptChunk `json:"chunks,omitempty"`
	Duration          *float64             `json:"duration,omitempty"`
	DurationSeconds   *float64             `json:"duration_seconds,omitempty"`
	BillingEvents     []FalBillingEvent    `json:"billing_events,omitempty"`
}
type FalTranscriptChunk struct {
	Text      string    `json:"text"`
	Timestamp []float64 `json:"timestamp,omitempty"`
	Speaker   *string   `json:"speaker,omitempty"`
}
type FalUploadRequest struct {
	ContentType string `json:"content_type"`
	FileName    string `json:"file_name"`
}
type FalUploadResponse struct {
	UploadURL string `json:"upload_url"`
	FileURL   string `json:"file_url"`
}
type FalError struct {
	Message string          `json:"message,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}
type FalJobReference struct {
	Model     string `json:"model"`
	RequestID string `json:"request_id"`
}

type FalModel struct {
	EndpointID string           `json:"endpoint_id"`
	Metadata   FalModelMetadata `json:"metadata"`
}
type FalModelMetadata struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Status      string `json:"status"`
}
type FalModelsPage struct {
	Models     []FalModel `json:"models"`
	NextCursor string     `json:"next_cursor"`
	HasMore    *bool      `json:"has_more"`
}
type FalBillingEvent struct {
	RequestID   string      `json:"request_id"`
	EndpointID  string      `json:"endpoint_id"`
	Quantity    json.Number `json:"quantity"`
	Unit        string      `json:"unit"`
	CostTotal   json.Number `json:"cost_total"`
	CostNanoUSD json.Number `json:"cost_estimate_nano_usd,omitempty"`
}
type FalBillingPage struct {
	Events     []FalBillingEvent `json:"billing_events"`
	NextCursor string            `json:"next_cursor"`
	HasMore    *bool             `json:"has_more"`
}
