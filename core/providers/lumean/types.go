package lumean

import "encoding/json"

type LumeanSpeechRequest struct {
	TemplateID     string                `json:"template_id,omitempty"`
	InputText      string                `json:"input_text,omitempty"`
	ConfigOverride *LumeanConfigOverride `json:"config_override,omitempty"`
	TaskType       string                `json:"task_type,omitempty"`
	TaskData       *LumeanTaskData       `json:"task_data,omitempty"`
}
type LumeanConfigOverride struct {
	TTSSettings LumeanTTSSettings `json:"tts_settings"`
}
type LumeanTTSSettings struct {
	ModelID       string `json:"model_id"`
	VoiceID       string `json:"voice_id"`
	PublicOwnerID string `json:"public_owner_id,omitempty"`
}
type LumeanTaskData struct {
	Prompt            string   `json:"prompt,omitempty"`
	NVariants         *int     `json:"n_variants,omitempty"`
	ForceInstrumental *bool    `json:"force_instrumental,omitempty"`
	MusicLengthMs     *int64   `json:"music_length_ms,omitempty"`
	ModelID           string   `json:"model_id,omitempty"`
	Text              string   `json:"text,omitempty"`
	DurationSeconds   *float64 `json:"duration_seconds,omitempty"`
	Loop              *bool    `json:"loop,omitempty"`
	OutputFormat      string   `json:"output_format,omitempty"`
}
type LumeanOrderResponse struct {
	Success              *bool       `json:"success,omitempty"`
	ConfirmationRequired bool        `json:"confirmation_required"`
	Data                 LumeanOrder `json:"data"`
}
type LumeanOrder struct {
	ID                   string       `json:"id"`
	Status               string       `json:"status"`
	ConfirmationRequired bool         `json:"confirmation_required"`
	FilesExpired         bool         `json:"files_expired"`
	Result               LumeanResult `json:"result"`
	TokensSpent          *json.Number `json:"tokens_spent,omitempty"`
}
type LumeanResult struct {
	Files        []string `json:"files"`
	ServiceFiles []string `json:"service_files"`
}
type LumeanStorageRequest struct {
	Path     string `json:"path"`
	Download bool   `json:"download"`
}
type LumeanStorageResponse struct {
	Data LumeanStorageData `json:"data"`
}
type LumeanStorageData struct {
	URL string `json:"url"`
}
type LumeanReceipt struct {
	TaskID          string       `json:"task_id,omitempty"`
	ClientTaskID    string       `json:"client_task_id,omitempty"`
	Status          string       `json:"status"`
	ProviderCredits *json.Number `json:"provider_credits,omitempty"`
	ServiceFiles    []string     `json:"service_files,omitempty"`
}
type LumeanModel struct {
	ID   string
	Name string
}
type LumeanModelsResponse struct {
	Success *bool           `json:"success,omitempty"`
	Data    LumeanModelData `json:"data"`
}
type LumeanModelData struct {
	TTSSettings LumeanModelSettings `json:"tts_settings"`
}
type LumeanModelSettings struct {
	ModelID LumeanModelOptions `json:"model_id"`
}
type LumeanModelOptions struct {
	Options []string `json:"options"`
}
type LumeanError struct {
	Message string `json:"message"`
	Error   string `json:"error"`
}

type LumeanVoice struct {
	VoiceID    string            `json:"voice_id"`
	Name       string            `json:"name"`
	Language   string            `json:"language"`
	Gender     string            `json:"gender"`
	PreviewURL string            `json:"preview_url"`
	Labels     map[string]string `json:"labels"`
}

type LumeanVoicesResponse struct {
	Success *bool            `json:"success"`
	Data    LumeanVoicesPage `json:"data"`
}

type LumeanVoicesPage struct {
	Voices      []LumeanVoice `json:"voices"`
	Items       []LumeanVoice `json:"items"`
	HasMore     *bool         `json:"has_more"`
	CurrentPage int           `json:"current_page"`
	LastPage    int           `json:"last_page"`
}
