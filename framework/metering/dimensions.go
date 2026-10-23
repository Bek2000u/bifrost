package metering

import (
	"github.com/maximhq/bifrost/core/schemas"
	"strconv"
)

// captureDimensions copies only explicit pricing knobs, never prompts, URLs,
// input images or arbitrary provider parameters.
func captureDimensions(req *schemas.BifrostRequest, out map[string]string) {
	set := func(name string, value *string) {
		if value != nil {
			out[name] = *value
		}
	}
	if req.ImageGenerationRequest != nil && req.ImageGenerationRequest.Params != nil {
		p := req.ImageGenerationRequest.Params
		set("size", p.Size)
		set("quality", p.Quality)
		set("aspect_ratio", p.AspectRatio)
		set("operation_type", p.Type)
		if p.N != nil {
			out["requested_count"] = strconv.Itoa(*p.N)
		}
	}
	if req.VideoGenerationRequest != nil && req.VideoGenerationRequest.Params != nil {
		p := req.VideoGenerationRequest.Params
		set("requested_seconds", p.Seconds)
		set("operation_type", p.Type)
		if p.Size != "" {
			out["size"] = p.Size
		}
		if p.Audio != nil {
			out["audio"] = strconv.FormatBool(*p.Audio)
		}
	}
	if req.SpeechRequest != nil && req.SpeechRequest.Params != nil {
		p := req.SpeechRequest.Params
		if p.ResponseFormat != "" {
			out["response_format"] = p.ResponseFormat
		}
		if p.Speed != nil {
			out["speed"] = strconv.FormatFloat(*p.Speed, 'f', -1, 64)
		}
		if p.VoiceConfig != nil {
			set("voice", p.VoiceConfig.Voice)
		}
	}
}
