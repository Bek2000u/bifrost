package fal

import (
	"encoding/base64"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
)

func ToFalImageGenerationRequest(request *schemas.BifrostImageGenerationRequest) (*FalRequest, error) {
	if request == nil {
		return nil, fmt.Errorf("image request is required")
	}
	if _, _, err := endpointPaths(request.Model); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	n := 1
	if p := request.Params; p != nil {
		if p.Size != nil {
			setImageSize(fields, *p.Size)
		}
		if len(p.InputImages) > 0 {
			imageReferences(fields, request.Model, p.InputImages)
		}
		if p.Seed != nil {
			fields["seed"] = *p.Seed
		}
		if p.NegativePrompt != nil {
			fields["negative_prompt"] = *p.NegativePrompt
		}
		if p.NumInferenceSteps != nil {
			fields["num_inference_steps"] = *p.NumInferenceSteps
		}
		if p.AspectRatio != nil {
			fields["aspect_ratio"] = *p.AspectRatio
		}
		if p.OutputFormat != nil {
			fields["output_format"] = *p.OutputFormat
		}
		if p.Quality != nil {
			fields["quality"] = *p.Quality
		}
		for key, value := range nativeOptions(p.ExtraParams) {
			fields[key] = value
		}
		if p.N != nil {
			n = *p.N
		}
	}
	if request.Input != nil && request.Input.Prompt != "" {
		fields["prompt"] = request.Input.Prompt
	}
	fields["num_images"] = n
	return &FalRequest{Fields: fields}, nil
}
func ToFalImageEditRequest(request *schemas.BifrostImageEditRequest) (*FalRequest, error) {
	if request == nil || request.Input == nil {
		return nil, fmt.Errorf("image edit input is required")
	}
	generation := &schemas.BifrostImageGenerationRequest{Model: request.Model, Input: &schemas.ImageGenerationInput{Prompt: request.Input.Prompt}, Params: &schemas.ImageGenerationParameters{}}
	for _, image := range request.Input.Images {
		generation.Params.InputImages = append(generation.Params.InputImages, imageURL(image))
	}
	if p := request.Params; p != nil {
		generation.Params.N = p.N
		generation.Params.Seed = p.Seed
		generation.Params.Size = p.Size
		generation.Params.AspectRatio = p.AspectRatio
		generation.Params.NegativePrompt = p.NegativePrompt
		generation.Params.NumInferenceSteps = p.NumInferenceSteps
		generation.Params.OutputFormat = p.OutputFormat
		generation.Params.Quality = p.Quality
		generation.Params.ExtraParams = p.ExtraParams
	}
	converted, err := ToFalImageGenerationRequest(generation)
	if err != nil {
		return nil, err
	}
	if request.Params != nil && len(request.Params.Mask) > 0 {
		if _, overridden := converted.Fields["mask_url"]; !overridden {
			converted.Fields["mask_url"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(request.Params.Mask)
		}
	}
	return converted, nil
}
func ToBifrostImageGenerationResponse(native *FalResult, id, model string) (*schemas.BifrostImageGenerationResponse, error) {
	images := native.Images
	if len(images) == 0 && native.Image != nil {
		images = []FalImage{*native.Image}
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("fal result did not contain images")
	}
	response := &schemas.BifrostImageGenerationResponse{ID: id, Model: model, Data: make([]schemas.ImageData, 0, len(images))}
	for _, image := range images {
		if image.URL == "" {
			return nil, fmt.Errorf("fal image URL is missing")
		}
		data := schemas.ImageData{URL: image.URL}
		if media, encoded, ok := stringsCutDataURL(image.URL); ok {
			if media == "" {
				return nil, fmt.Errorf("invalid fal image data URL")
			}
			data = schemas.ImageData{B64JSON: encoded}
		}
		response.Data = append(response.Data, data)
	}
	return response, nil
}
