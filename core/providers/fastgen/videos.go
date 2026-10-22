package fastgen

import (
	"fmt"
	"strconv"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func ToFastgenVideoGenerationRequest(request *schemas.BifrostVideoGenerationRequest) (*FastgenGenerationRequest, error) {
	if request == nil || request.Input == nil {
		return nil, fmt.Errorf("video prompt is required")
	}
	extra := map[string]any{}
	var inputs []any
	if request.Input.InputReference != nil {
		inputs = append(inputs, *request.Input.InputReference)
	}
	if request.Input.VideoURI != nil {
		inputs = append(inputs, *request.Input.VideoURI)
	}
	if p := request.Params; p != nil {
		if p.NegativePrompt != nil || p.Type != nil || p.OutputFormat != nil || p.Audio != nil || p.UpscaleFactor != nil || p.TargetMegapixels != nil {
			return nil, fmt.Errorf("unsupported fastgen video option")
		}
		for key, value := range p.ExtraParams {
			extra[key] = value
		}
		if p.Seed != nil {
			extra["seed"] = *p.Seed
		}
		if p.Size != "" {
			extra["aspect_ratio"] = strings.Replace(p.Size, "x", ":", 1)
		}
		if p.Seconds != nil {
			seconds, err := strconv.Atoi(*p.Seconds)
			if err != nil {
				return nil, fmt.Errorf("invalid video seconds")
			}
			extra["duration_seconds"] = seconds
		}
	}
	return generationRequest(request.Model, "video", request.Input.Prompt, inputs, extra)
}

func ToBifrostVideoGenerationResponse(native *FastgenGenerationResponse, base string) (*schemas.BifrostVideoGenerationResponse, error) {
	if native.ID == "" {
		return nil, fmt.Errorf("fastgen omitted generation id")
	}
	result := &schemas.BifrostVideoGenerationResponse{ID: providerUtils.AddVideoIDProviderSuffix(native.ID, schemas.Fastgen), Model: native.Model, Object: "video"}
	switch native.Status {
	case "", "queued":
		result.Status = schemas.VideoStatusQueued
	case "running":
		result.Status = schemas.VideoStatusInProgress
	case "failed":
		result.Status = schemas.VideoStatusFailed
		result.Error = &schemas.VideoCreateError{Code: "fastgen_generation_failed", Message: string(native.Error)}
	case "succeeded":
		result.Status = schemas.VideoStatusCompleted
		target, err := resultURL(native, "video", base)
		if err != nil {
			return nil, err
		}
		mime := native.Results[0].MIMEType
		if mime == "" {
			mime = "video/mp4"
		}
		result.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: &target, ContentType: mime}}
	default:
		return nil, fmt.Errorf("unknown fastgen generation status %q", native.Status)
	}
	result.ExtraFields.RawResponse = native
	return result, nil
}
