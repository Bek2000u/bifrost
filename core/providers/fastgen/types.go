package fastgen

import (
	"encoding/json"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
)

type FastgenGenerationRequest struct {
	Model       string         `json:"model"`
	Prompt      string         `json:"prompt,omitempty"`
	Inputs      []any          `json:"inputs"`
	ExtraParams map[string]any `json:"-"`
}

// Validated native options are part of this provider's request regardless of the
// gateway's arbitrary-extra-parameter passthrough setting.
func (r *FastgenGenerationRequest) GetExtraParams() map[string]any { return nil }

func (r *FastgenGenerationRequest) MarshalJSON() ([]byte, error) {
	type wire FastgenGenerationRequest
	body, err := providerUtils.MarshalSorted((*wire)(r))
	if err != nil {
		return nil, err
	}
	return providerUtils.MergeExtraParamsIntoJSON(body, r.ExtraParams)
}

type FastgenPromptRequest struct {
	UserPrompt string `json:"user_prompt"`
}

func (r *FastgenPromptRequest) GetExtraParams() map[string]any { return nil }

type FastgenPromptResponse struct {
	Success       *bool               `json:"success,omitempty"`
	OperationID   string              `json:"operation_id"`
	GeneratedText *string             `json:"generated_text"`
	Usage         *FastgenPromptUsage `json:"usage"`
}
type FastgenPromptUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
}
type FastgenGenerationResponse struct {
	ID      string          `json:"id"`
	Model   string          `json:"model,omitempty"`
	Status  string          `json:"status"`
	Results []FastgenResult `json:"results,omitempty"`
	Usage   *FastgenUsage   `json:"usage,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}
type FastgenUsage struct {
	Credits json.Number `json:"credits"`
}
type FastgenResult struct {
	Type          string             `json:"type"`
	Text          string             `json:"text,omitempty"`
	DownloadURL   string             `json:"download_url,omitempty"`
	Data          string             `json:"data,omitempty"`
	MIMEType      string             `json:"mime_type,omitempty"`
	Transcription *FastgenTranscript `json:"transcription,omitempty"`
}
type FastgenTranscript struct {
	Language        *string                    `json:"language,omitempty"`
	DurationSeconds *float64                   `json:"duration_seconds,omitempty"`
	WordTimestamps  bool                       `json:"word_timestamps"`
	Segments        []FastgenTranscriptSegment `json:"segments,omitempty"`
}
type FastgenTranscriptSegment struct {
	Text         string                  `json:"text"`
	StartSeconds *float64                `json:"start_seconds,omitempty"`
	EndSeconds   *float64                `json:"end_seconds,omitempty"`
	Speaker      *string                 `json:"speaker,omitempty"`
	Words        []FastgenTranscriptWord `json:"words,omitempty"`
}
type FastgenTranscriptWord struct {
	Text         string  `json:"text"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Speaker      *string `json:"speaker,omitempty"`
}
type FastgenModel struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Available       *bool  `json:"available,omitempty"`
	InputTokenLimit *int   `json:"input_token_limit,omitempty"`
}
type FastgenError struct {
	Message string          `json:"message,omitempty"`
	Code    string          `json:"code,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type FastgenVoice struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Language    string            `json:"language"`
	Gender      string            `json:"gender"`
	Provider    string            `json:"provider"`
	Models      []string          `json:"models"`
	PreviewURL  string            `json:"preview_url"`
	PreviewURLs map[string]string `json:"preview_urls"`
}

type fastgenModelSpec struct {
	kind      string
	maxInputs int
	options   []string
}
