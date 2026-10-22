package fal

import (
	"fmt"
	"math"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

func ToFalSpeechRequest(request *schemas.BifrostSpeechRequest) (*FalRequest, error) {
	if request == nil || request.Input == nil {
		return nil, fmt.Errorf("speech input is required")
	}
	if _, _, err := endpointPaths(request.Model); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if p := request.Params; p != nil {
		fields = nativeOptions(p.ExtraParams)
		if p.VoiceConfig != nil && p.VoiceConfig.Voice != nil {
			fields["voice"] = *p.VoiceConfig.Voice
		}
		if p.Speed != nil {
			fields["speed"] = *p.Speed
		}
		if p.LanguageCode != nil {
			fields["language"] = *p.LanguageCode
		}
	}
	fields["prompt"] = request.Input.Input
	if !audioGeneration(request.Model) {
		fields["text"] = request.Input.Input
	}
	return &FalRequest{Fields: fields}, nil
}
func audioGeneration(model string) bool {
	return strings.Contains(model, "music") || strings.Contains(model, "sound-effects") || strings.Contains(model, "text-to-audio") || strings.Contains(model, "diffrhythm") || strings.Contains(model, "stable-audio")
}

func ToFalTranscriptionRequest(request *schemas.BifrostTranscriptionRequest, audioURL string) (*FalRequest, error) {
	if request == nil {
		return nil, fmt.Errorf("transcription request is required")
	}
	if _, _, err := endpointPaths(request.Model); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if p := request.Params; p != nil {
		fields = nativeOptions(p.ExtraParams)
		if p.Language != nil {
			fields["language"] = *p.Language
		}
		if p.Prompt != nil {
			fields["prompt"] = *p.Prompt
		}
	}
	if audioURL != "" {
		fields["audio_url"] = audioURL
	}
	if _, exists := fields["audio_url"]; !exists {
		return nil, fmt.Errorf("fal transcription requires audio file bytes or audio_url")
	}
	return &FalRequest{Fields: fields}, nil
}

func ToBifrostTranscriptionResponse(native *FalResult) (*schemas.BifrostTranscriptionResponse, error) {
	if native.Text == nil {
		return nil, fmt.Errorf("fal transcription omitted text")
	}
	response := &schemas.BifrostTranscriptionResponse{Text: *native.Text, Language: native.Language}
	if duration := nativeDuration(native); duration != nil {
		response.Duration = duration
		response.Usage = &schemas.TranscriptionUsage{Type: "duration", Seconds: duration}
	}
	if response.Language == nil && len(native.InferredLanguages) > 0 {
		response.Language = &native.InferredLanguages[0]
	}
	if response.Language == nil && len(native.Languages) > 0 {
		response.Language = &native.Languages[0]
	}
	for index, chunk := range native.Chunks {
		if len(chunk.Timestamp) < 2 {
			continue
		}
		start, end := chunk.Timestamp[0], chunk.Timestamp[1]
		if start < 0 || end < start || math.IsNaN(start) || math.IsNaN(end) || math.IsInf(start, 0) || math.IsInf(end, 0) {
			return nil, fmt.Errorf("invalid fal transcription timestamps")
		}
		response.Segments = append(response.Segments, schemas.TranscriptionSegment{ID: index, Start: start, End: end, Text: chunk.Text})
		if chunk.Speaker != nil {
			response.DiarizedSegments = append(response.DiarizedSegments, schemas.TranscriptionDiarizedSegment{ID: fmt.Sprint(index), Type: "transcript.text.segment", Speaker: *chunk.Speaker, Start: start, End: end, Text: chunk.Text})
		}
	}
	return response, nil
}
