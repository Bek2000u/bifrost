package fastgen

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
)

func asRequest(body providerUtils.RequestBodyWithExtraParams, err error) (providerUtils.RequestBodyWithExtraParams, error) {
	if err != nil {
		return nil, providerUtils.InvalidRequestErrorf("%s", err.Error())
	}
	return body, nil
}

func generationRequest(model, kind, prompt string, inputs []any, extra map[string]any) (*FastgenGenerationRequest, error) {
	spec, exists := modelSpecs[model]
	if !exists || spec.kind != kind {
		return nil, fmt.Errorf("fastgen model %q does not support %s", model, kind)
	}
	options := make(map[string]any, len(extra))
	for key, value := range extra {
		if key == "resume_task_id" {
			if id, ok := value.(string); !ok || id == "" {
				return nil, fmt.Errorf("resume_task_id must be a nonempty string")
			}
			continue
		}
		if key == "inputs" {
			if len(inputs) > 0 {
				return nil, fmt.Errorf("inputs specified twice")
			}
			switch references := value.(type) {
			case []string:
				for _, ref := range references {
					inputs = append(inputs, ref)
				}
			case []any:
				inputs = append(inputs, references...)
			default:
				return nil, fmt.Errorf("inputs must be an array")
			}
			continue
		}
		if !slices.Contains(spec.options, key) {
			return nil, fmt.Errorf("fastgen model %q does not accept %s", model, key)
		}
		options[key] = value
	}
	if inputs == nil {
		inputs = []any{}
	}
	if len(inputs) > spec.maxInputs || kind == "text" && (prompt != "" || len(inputs) != 1) || kind != "text" && strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("invalid fastgen prompt or input count")
	}
	if kind == "audio" && utf8.RuneCountInString(prompt) > 250000 {
		return nil, fmt.Errorf("fastgen speech accepts at most 250000 characters")
	}
	videos := 0
	for _, reference := range inputs {
		var value string
		switch ref := reference.(type) {
		case string:
			value = ref
		case map[string]any:
			filename, ok := ref["filename"].(string)
			if !ok || filename == "" || len(filename) > 255 || len(ref) != 2 {
				return nil, fmt.Errorf("invalid fastgen named input")
			}
			value, _ = ref["input"].(string)
		default:
			return nil, fmt.Errorf("invalid fastgen input")
		}
		mediaType, encoded, ok := strings.Cut(value, ";base64,")
		if !ok || !strings.HasPrefix(mediaType, "data:") {
			return nil, fmt.Errorf("fastgen inputs require base64 data URLs")
		}
		mediaType = strings.TrimPrefix(mediaType, "data:")
		limit := 5 * 1024 * 1024
		allowed := slices.Contains([]string{"image/png", "image/jpeg", "image/webp"}, mediaType)
		if kind == "text" {
			limit = 25 * 1024 * 1024
			allowed = mediaType == "audio/mpeg"
		} else if strings.HasPrefix(mediaType, "video/") {
			limit = 1024 * 1024 * 1024
			allowed = model == "flow-video-omni-flash" && slices.Contains([]string{"video/mp4", "video/webm", "video/quicktime"}, mediaType)
			videos++
		}
		if !allowed || encoded == "" || len(encoded)%4 != 0 || base64.StdEncoding.DecodedLen(len(encoded)) > limit+2 {
			return nil, fmt.Errorf("invalid fastgen reference media")
		}
		// Validate without decoding a second copy of potentially large references.
		for index, char := range encoded {
			if !(char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '+' || char == '/' || char == '=' && index >= len(encoded)-2) {
				return nil, fmt.Errorf("invalid fastgen base64 reference")
			}
			if char == '=' && index == len(encoded)-2 && encoded[index+1] != '=' {
				return nil, fmt.Errorf("invalid fastgen base64 padding")
			}
		}
	}
	keyframes, _ := options["keyframes"].(bool)
	if keyframes && (len(inputs) == 0 || len(inputs) > 2) || model == "flow-video-quality" && len(inputs) > 0 && !keyframes || videos > 0 && (videos != 1 || len(inputs) > 6 || keyframes || options["duration_seconds"] != nil) {
		return nil, fmt.Errorf("unsupported fastgen reference combination")
	}
	if value, ok := options["aspect_ratio"]; ok {
		ratio, ok := value.(string)
		left, right, split := strings.Cut(ratio, ":")
		w, e1 := strconv.Atoi(left)
		h, e2 := strconv.Atoi(right)
		if !ok || !split || e1 != nil || e2 != nil || w <= 0 || h <= 0 {
			return nil, fmt.Errorf("invalid aspect_ratio")
		}
	}
	for _, name := range []string{"keyframes", "ultra", "word_timestamps", "diarization", "smart"} {
		if value, ok := options[name]; ok {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be boolean", name)
			}
		}
	}
	for _, name := range []string{"voice", "scene", "sample_context"} {
		if value, ok := options[name]; ok {
			if _, ok := value.(string); !ok {
				return nil, fmt.Errorf("%s must be a string", name)
			}
		}
	}
	if value, ok := options["speaker_voices"]; ok {
		speakers := map[string]string{}
		switch voices := value.(type) {
		case map[string]string:
			speakers = voices
		case map[string]any:
			for speaker, voice := range voices {
				v, ok := voice.(string)
				if !ok {
					return nil, fmt.Errorf("speaker voice must be a string")
				}
				speakers[speaker] = v
			}
		default:
			return nil, fmt.Errorf("speaker_voices must be an object")
		}
		if len(speakers) != 2 {
			return nil, fmt.Errorf("speaker_voices requires exactly two voices")
		}
		for speaker, voice := range speakers {
			if len(speaker) < 1 || len(speaker) > 100 || len(voice) < 1 || len(voice) > 100 {
				return nil, fmt.Errorf("invalid speaker voice")
			}
		}
	}
	for _, name := range []string{"language_codes", "custom_vocabulary"} {
		if value, ok := options[name]; ok {
			limit, width := 10, 32
			if name == "custom_vocabulary" {
				limit, width = 100, 200
			}
			values, err := stringArray(value)
			if err != nil || len(values) > limit {
				return nil, fmt.Errorf("invalid %s", name)
			}
			for _, item := range values {
				if item == "" || utf8.RuneCountInString(item) > width {
					return nil, fmt.Errorf("invalid %s item", name)
				}
			}
		}
	}
	if value, ok := options["seed"]; ok {
		n, ok := number(value)
		if !ok || n < 0 || n > 2147483647 || n != math.Trunc(n) {
			return nil, fmt.Errorf("invalid seed")
		}
	}
	if value, ok := options["temperature"]; ok {
		n, ok := number(value)
		if !ok || n < 0 || n > 2 {
			return nil, fmt.Errorf("invalid temperature")
		}
	}
	if value, ok := options["duration_seconds"]; ok {
		n, ok := number(value)
		if !ok || !slices.Contains([]float64{4, 6, 8, 10}, n) {
			return nil, fmt.Errorf("invalid duration_seconds")
		}
	}
	if value, ok := options["quality"]; ok && value != "fast" && value != "thinking" {
		return nil, fmt.Errorf("invalid quality")
	}
	if value, ok := options["generation_config"]; ok {
		encoded, err := providerUtils.MarshalSorted(value)
		if err != nil {
			return nil, err
		}
		var config struct {
			Upscale struct {
				Type string `json:"type"`
			} `json:"upscale"`
		}
		if sonic.Unmarshal(encoded, &config) != nil {
			return nil, fmt.Errorf("invalid generation_config")
		}
		valid := kind == "image" && strings.HasPrefix(model, "nano-banana-") && config.Upscale.Type == "2x" || (model == "flow-video-ultra-lite" || model == "flow-video-fast" && options["ultra"] == true) && config.Upscale.Type == "1080p"
		if !valid {
			return nil, fmt.Errorf("unsupported fastgen upscale")
		}
	}
	if kind == "text" {
		if options["word_timestamps"] == nil {
			options["word_timestamps"] = true
		}
		if options["diarization"] == nil {
			options["diarization"] = true
		}
		if options["smart"] == nil {
			options["smart"] = false
		}
	}
	return &FastgenGenerationRequest{Model: model, Prompt: prompt, Inputs: inputs, ExtraParams: options}, nil
}

func number(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	default:
		return 0, false
	}
}

func stringArray(value any) ([]string, error) {
	switch values := value.(type) {
	case []string:
		return values, nil
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			item, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("expected string array")
			}
			result = append(result, item)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("expected string array")
	}
}

func validateArtifact(target, base string) error {
	u, err := url.Parse(target)
	origin, _ := url.Parse(base)
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return fmt.Errorf("invalid fastgen artifact URL")
	}
	if origin != nil && u.Scheme == origin.Scheme && strings.EqualFold(u.Host, origin.Host) {
		return nil
	}
	if u.Scheme == "https" && (u.Host == "storage.fast-gen.ai" || u.Host == "s3.fast-gen.ai") {
		return nil
	}
	return fmt.Errorf("unrecognized fastgen artifact origin")
}

func resultURL(response *FastgenGenerationResponse, kind, base string) (string, error) {
	if response.Status != "succeeded" || len(response.Results) != 1 || response.Results[0].Type != kind {
		return "", fmt.Errorf("fastgen did not return one final %s result", kind)
	}
	result := response.Results[0]
	target := result.DownloadURL
	if target == "" {
		target = result.Data
	}
	return target, validateArtifact(target, base)
}
