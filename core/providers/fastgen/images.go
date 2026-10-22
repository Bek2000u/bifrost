package fastgen

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/maximhq/bifrost/core/schemas"
)

func ToFastgenImageGenerationRequest(request *schemas.BifrostImageGenerationRequest) (*FastgenGenerationRequest, error) {
	if request == nil || request.Input == nil {
		return nil, fmt.Errorf("image prompt is required")
	}
	extra := map[string]any{}
	var inputs []any
	if p := request.Params; p != nil {
		if p.N != nil && *p.N != 1 || p.NegativePrompt != nil || p.NumInferenceSteps != nil || p.Background != nil || p.Moderation != nil || p.OutputCompression != nil || p.OutputFormat != nil || p.Style != nil || p.Type != nil || p.PartialImages != nil {
			return nil, fmt.Errorf("unsupported fastgen image option")
		}
		if p.Size != nil && *p.Size != "auto" {
			return nil, fmt.Errorf("use aspect_ratio for fastgen image dimensions")
		}
		for key, value := range p.ExtraParams {
			extra[key] = value
		}
		if p.AspectRatio != nil {
			extra["aspect_ratio"] = *p.AspectRatio
		}
		if p.Seed != nil {
			extra["seed"] = *p.Seed
		}
		if p.Quality != nil {
			extra["quality"] = *p.Quality
		}
		for _, input := range p.InputImages {
			inputs = append(inputs, input)
		}
		if p.ResponseFormat != nil && *p.ResponseFormat != "url" && *p.ResponseFormat != "b64_json" {
			return nil, fmt.Errorf("unsupported image response_format")
		}
	}
	return generationRequest(request.Model, "image", request.Input.Prompt, inputs, extra)
}

func ToFastgenImageEditRequest(request *schemas.BifrostImageEditRequest) (*FastgenGenerationRequest, error) {
	if request == nil || request.Input == nil || len(request.Input.Images) == 0 {
		return nil, fmt.Errorf("image edit requires reference images")
	}
	inputs := make([]any, 0, len(request.Input.Images))
	for _, input := range request.Input.Images {
		if input.URL != "" {
			inputs = append(inputs, input.URL)
		} else {
			inputs = append(inputs, "data:"+http.DetectContentType(input.Image)+";base64,"+base64.StdEncoding.EncodeToString(input.Image))
		}
	}
	generation := &schemas.BifrostImageGenerationRequest{Model: request.Model, Input: &schemas.ImageGenerationInput{Prompt: request.Input.Prompt}, Params: &schemas.ImageGenerationParameters{}}
	if p := request.Params; p != nil {
		if p.Mask != nil || p.Type != nil || p.UpscaleFactor != nil || p.TargetMegapixels != nil || p.InputFidelity != nil || p.OutputCompression != nil || p.OutputFormat != nil || p.PartialImages != nil || p.Background != nil || p.NegativePrompt != nil || p.NumInferenceSteps != nil {
			return nil, fmt.Errorf("fastgen image edit does not support masks or operation selectors")
		}
		generation.Params.N = p.N
		generation.Params.Size = p.Size
		generation.Params.Seed = p.Seed
		generation.Params.AspectRatio = p.AspectRatio
		generation.Params.Quality = p.Quality
		generation.Params.ResponseFormat = p.ResponseFormat
		generation.Params.ExtraParams = p.ExtraParams
	}
	result, err := ToFastgenImageGenerationRequest(generation)
	if err != nil {
		return nil, err
	}
	return generationRequest(result.Model, "image", result.Prompt, inputs, result.ExtraParams)
}
