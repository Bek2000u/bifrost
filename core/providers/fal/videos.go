package fal

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

func ToFalVideoGenerationRequest(request *schemas.BifrostVideoGenerationRequest) (*FalRequest, error) {
	if request == nil {
		return nil, fmt.Errorf("video request is required")
	}
	if _, _, err := endpointPaths(request.Model); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if request.Input != nil {
		if request.Input.InputReference != nil {
			fields["image_url"] = *request.Input.InputReference
		}
		if request.Input.VideoURI != nil {
			fields["video_url"] = *request.Input.VideoURI
		}
	}
	if p := request.Params; p != nil {
		if aspect, resolution, ok := strings.Cut(p.Size, "_"); ok {
			fields["aspect_ratio"] = aspect
			fields["resolution"] = resolution
		} else if strings.Contains(p.Size, ":") {
			fields["aspect_ratio"] = p.Size
		} else if p.Size != "" {
			fields["resolution"] = p.Size
		}
		if p.Seed != nil {
			fields["seed"] = *p.Seed
		}
		if p.NegativePrompt != nil {
			fields["negative_prompt"] = *p.NegativePrompt
		}
		if p.Audio != nil {
			fields["generate_audio"] = *p.Audio
		}
		for key, value := range nativeOptions(p.ExtraParams) {
			fields[key] = value
		}
		if p.Seconds != nil {
			duration := *p.Seconds
			if (strings.HasPrefix(request.Model, "fal-ai/veo3") || request.Model == "fal-ai/luma-dream-machine/ray-2") && !strings.HasSuffix(duration, "s") {
				duration += "s"
			}
			fields["duration"] = duration
		}
	}
	if request.Input != nil && request.Input.Prompt != "" {
		fields["prompt"] = request.Input.Prompt
	}
	return &FalRequest{Fields: fields}, nil
}

func ToBifrostVideoGenerationResponse(native *FalQueueResponse, model string) (*schemas.BifrostVideoGenerationResponse, error) {
	if native.RequestID == "" {
		return nil, fmt.Errorf("fal omitted request_id")
	}
	response := &schemas.BifrostVideoGenerationResponse{ID: encodeJobID(model, native.RequestID), Model: model, Object: "video"}
	switch native.Status {
	case "", "IN_QUEUE":
		response.Status = schemas.VideoStatusQueued
	case "IN_PROGRESS":
		response.Status = schemas.VideoStatusInProgress
	case "COMPLETED":
		response.Status = schemas.VideoStatusCompleted
	default:
		return nil, fmt.Errorf("unknown fal queue status %q", native.Status)
	}
	if native.Error != "" {
		response.Status = schemas.VideoStatusFailed
		response.Error = &schemas.VideoCreateError{Code: native.ErrorType, Message: native.Error}
	}
	return response, nil
}
