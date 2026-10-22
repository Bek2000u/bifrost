package voicegen

type VoicegenSpeechRequest struct {
	Text                    string            `json:"text"`
	VoiceID                 string            `json:"voice_id"`
	VoiceSource             string            `json:"voice_source"`
	ModelID                 string            `json:"model_id"`
	VoiceEngine             string            `json:"voice_engine"`
	ClientTaskID            string            `json:"client_task_id"`
	AutoStart               bool              `json:"auto_start"`
	SplitIntoChunkFiles     bool              `json:"split_into_chunk_files"`
	SplitIntoParagraphFiles bool              `json:"split_into_paragraph_files"`
	IncludeTimestamps       bool              `json:"include_timestamps"`
	TimestampsGranularity   string            `json:"timestamps_granularity"`
	SettingsPreset          string            `json:"settings_preset,omitempty"`
	VoiceEngineSettings     *VoicegenSettings `json:"voice_engine_settings,omitempty"`
}

type VoicegenSettings struct {
	Speed float64 `json:"speed"`
}
type VoicegenTaskResponse struct {
	Task *VoicegenTask `json:"task"`
	VoicegenTask
}
type VoicegenTask struct {
	TaskID                string `json:"task_id"`
	Status                string `json:"status"`
	TimestampsComplete    bool   `json:"timestamps_complete"`
	PublicDownloadURL     string `json:"public_download_url"`
	DownloadURL           string `json:"download_url"`
	TimestampsURL         string `json:"timestamps_url"`
	TimestampsFormat      string `json:"timestamps_format"`
	TimestampsGranularity string `json:"timestamps_granularity"`
	ResultCharsCount      *int   `json:"result_chars_count"`
}
type VoicegenReceipt struct {
	TaskID        string `json:"task_id,omitempty"`
	ClientTaskID  string `json:"client_task_id,omitempty"`
	Status        string `json:"status"`
	TimestampsURL string `json:"timestamps_url,omitempty"`
	Characters    *int   `json:"characters,omitempty"`
}
type VoicegenModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type VoicegenModelsResponse struct {
	Models []VoicegenModel `json:"models"`
}
type VoicegenError struct {
	Message string `json:"message"`
	Error   string `json:"error"`
}

type VoicegenVoice struct {
	VoiceID            string            `json:"voice_id"`
	Name               string            `json:"name"`
	Language           string            `json:"language"`
	Gender             string            `json:"gender"`
	SupportedLanguages []string          `json:"supported_languages"`
	Labels             map[string]string `json:"labels"`
}

type VoicegenVoicesResponse struct {
	Voices     []VoicegenVoice `json:"voices"`
	Page       int             `json:"page"`
	TotalPages int             `json:"total_pages"`
}
