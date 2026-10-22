package fastgen

import (
	"encoding/base64"
	"fmt"
	"math"

	"github.com/maximhq/bifrost/core/schemas"
)

func ToFastgenSpeechRequest(request *schemas.BifrostSpeechRequest) (*FastgenGenerationRequest, error) {
	if request == nil || request.Input == nil {
		return nil, fmt.Errorf("speech input is required")
	}
	extra := map[string]any{}
	if p := request.Params; p != nil {
		if p.Speed != nil && *p.Speed != 1 || p.ResponseFormat != "" && p.ResponseFormat != "mp3" || p.Instructions != "" || p.LanguageCode != nil || len(p.PronunciationDictionaryLocators) > 0 || p.WithTimestamps != nil && *p.WithTimestamps {
			return nil, fmt.Errorf("unsupported fastgen speech option")
		}
		for key, value := range p.ExtraParams {
			extra[key] = value
		}
		if voice := p.VoiceConfig; voice != nil {
			if voice.Voice != nil && len(voice.MultiVoiceConfig) > 0 {
				return nil, fmt.Errorf("voice and multi-voice are mutually exclusive")
			}
			if voice.Voice != nil {
				extra["voice"] = *voice.Voice
			}
			if len(voice.MultiVoiceConfig) > 0 {
				if len(voice.MultiVoiceConfig) != 2 {
					return nil, fmt.Errorf("fastgen dialogue requires exactly two voices")
				}
				speakers := map[string]string{}
				for _, v := range voice.MultiVoiceConfig {
					if v.Speaker == "" || v.Voice == "" || speakers[v.Speaker] != "" {
						return nil, fmt.Errorf("invalid dialogue voice")
					}
					speakers[v.Speaker] = v.Voice
				}
				extra["speaker_voices"] = speakers
			}
		}
	}
	return generationRequest(request.Model, "audio", request.Input.Input, nil, extra)
}

func ToFastgenTranscriptionRequest(request *schemas.BifrostTranscriptionRequest) (*FastgenGenerationRequest, error) {
	if request == nil || request.Input == nil || len(request.Input.File) == 0 || len(request.Input.File) > 25*1024*1024 {
		return nil, fmt.Errorf("fastgen transcription requires an MP3 of at most 25 MiB")
	}
	extra := map[string]any{}
	if p := request.Params; p != nil {
		if p.Prompt != nil || p.Temperature != nil || len(p.Include) > 0 || p.ResponseFormat != nil && *p.ResponseFormat != "json" && *p.ResponseFormat != "verbose_json" || p.Format != nil && *p.Format != "mp3" && *p.Format != "audio/mpeg" {
			return nil, fmt.Errorf("unsupported fastgen transcription option")
		}
		for key, value := range p.ExtraParams {
			extra[key] = value
		}
		if p.Language != nil {
			extra["language_codes"] = []string{*p.Language}
		}
	}
	return generationRequest(request.Model, "text", "", []any{"data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(request.Input.File)}, extra)
}

func ToBifrostTranscriptionResponse(native *FastgenGenerationResponse, requireWords bool) (*schemas.BifrostTranscriptionResponse, error) {
	if native.Status != "succeeded" || len(native.Results) != 1 || native.Results[0].Type != "text" || native.Results[0].Text == "" {
		return nil, fmt.Errorf("fastgen did not return one final transcription")
	}
	result := &schemas.BifrostTranscriptionResponse{Text: native.Results[0].Text}
	t := native.Results[0].Transcription
	if requireWords && (t == nil || !t.WordTimestamps || len(t.Segments) == 0 || len(t.Segments) > 20000) {
		return nil, fmt.Errorf("fastgen omitted requested word timestamps")
	}
	if t != nil {
		result.Language = t.Language
		result.Duration = t.DurationSeconds
		if t.DurationSeconds != nil && (!validTime(*t.DurationSeconds)) {
			return nil, fmt.Errorf("invalid transcription duration")
		}
		if t.DurationSeconds != nil {
			result.Usage = &schemas.TranscriptionUsage{Type: "duration", Seconds: t.DurationSeconds}
		}
		for index, segment := range t.Segments {
			if requireWords && len(segment.Words) == 0 {
				return nil, fmt.Errorf("empty timestamp segment")
			}
			for _, word := range segment.Words {
				if word.Text == "" || !validTime(word.StartSeconds) || !validTime(word.EndSeconds) || word.EndSeconds < word.StartSeconds || len(result.Words) >= 20000 {
					return nil, fmt.Errorf("invalid transcription word")
				}
				speaker := word.Speaker
				if speaker == nil {
					speaker = segment.Speaker
				}
				result.Words = append(result.Words, schemas.TranscriptionWord{Word: word.Text, Start: word.StartSeconds, End: word.EndSeconds, Speaker: speaker})
			}
			start, end := segment.StartSeconds, segment.EndSeconds
			if len(segment.Words) > 0 {
				if start == nil {
					start = &segment.Words[0].StartSeconds
				}
				if end == nil {
					end = &segment.Words[len(segment.Words)-1].EndSeconds
				}
			}
			if start != nil && end != nil {
				if !validTime(*start) || !validTime(*end) || *end < *start {
					return nil, fmt.Errorf("invalid transcript segment")
				}
				result.Segments = append(result.Segments, schemas.TranscriptionSegment{ID: index, Text: segment.Text, Start: *start, End: *end})
			}
		}
	}
	result.ExtraFields.RawResponse = native
	return result, nil
}

func validTime(value float64) bool { return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) }
