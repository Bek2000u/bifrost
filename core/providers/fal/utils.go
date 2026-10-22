package fal

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func endpointPaths(model string) (string, string, error) {
	parts := strings.Split(model, "/")
	baseParts := 2
	if len(parts) > 0 && (parts[0] == "workflows" || parts[0] == "comfy") {
		baseParts = 3
	}
	if len(parts) < baseParts {
		return "", "", fmt.Errorf("fal model must use its native owner/app endpoint id")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "?#%\\:\x00\r\n") {
			return "", "", fmt.Errorf("invalid fal endpoint id")
		}
	}
	return "/" + strings.Join(parts, "/"), "/" + strings.Join(parts[:baseParts], "/"), nil
}

func encodeJobID(model, id string) string {
	data, _ := providerUtils.MarshalSorted(FalJobReference{Model: model, RequestID: id})
	return providerUtils.AddVideoIDProviderSuffix(base64.RawURLEncoding.EncodeToString(data), schemas.Fal)
}
func decodeJobID(id string) (FalJobReference, error) {
	var reference FalJobReference
	decoded, err := base64.RawURLEncoding.DecodeString(providerUtils.StripVideoIDProviderSuffix(id, schemas.Fal))
	if err != nil {
		return reference, fmt.Errorf("invalid fal video id")
	}
	if err = sonic.Unmarshal(decoded, &reference); err != nil || reference.RequestID == "" {
		return reference, fmt.Errorf("invalid fal video reference")
	}
	_, _, err = endpointPaths(reference.Model)
	return reference, err
}

func nativeOptions(extra map[string]any) map[string]any {
	fields := make(map[string]any, len(extra)+5)
	for key, value := range extra {
		if key != "resume_task_id" {
			fields[key] = value
		}
	}
	return fields
}
func asRequest(request *FalRequest, err error) (providerUtils.RequestBodyWithExtraParams, error) {
	if err != nil {
		return nil, providerUtils.InvalidRequestErrorf("%s", err.Error())
	}
	return request, nil
}
func imageURL(image schemas.ImageInput) string {
	if image.URL != "" {
		return image.URL
	}
	return "data:" + http.DetectContentType(image.Image) + ";base64," + base64.StdEncoding.EncodeToString(image.Image)
}
func imageReferences(fields map[string]any, model string, images []string) {
	listInput := model == "fal-ai/nano-banana/edit" || model == "fal-ai/nano-banana-2/edit" || model == "fal-ai/nano-banana-pro/edit" || model == "fal-ai/flux-pro/kontext/multi" || model == "fal-ai/flux-pro/kontext/max/multi"
	if len(images) == 1 && !listInput {
		fields["image_url"] = images[0]
	} else if len(images) > 1 {
		fields["image_urls"] = images
	} else if len(images) == 1 {
		fields["image_urls"] = images
	}
}
func setImageSize(fields map[string]any, size string) {
	if size == "" || size == "auto" {
		return
	}
	if width, height, ok := strings.Cut(size, "x"); ok {
		w, e1 := strconv.Atoi(width)
		h, e2 := strconv.Atoi(height)
		if e1 == nil && e2 == nil {
			fields["image_size"] = map[string]int{"width": w, "height": h}
			return
		}
	}
	if aspect, resolution, ok := strings.Cut(size, "_"); ok && strings.Contains(aspect, ":") {
		fields["aspect_ratio"] = aspect
		fields["resolution"] = resolution
	} else if strings.Contains(size, ":") {
		fields["aspect_ratio"] = size
	} else {
		fields["image_size"] = size
	}
}

func downloadURL(target string) error {
	parsed, err := url.Parse(target)
	if err != nil || parsed.User != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("invalid fal media URL")
	}
	return nil
}

func stringsCutDataURL(value string) (string, string, bool) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", false
	}
	header, encoded, ok := strings.Cut(value, ";base64,")
	if !ok || !strings.HasPrefix(header, "data:image/") || encoded == "" {
		return "", "", true
	}
	return strings.TrimPrefix(header, "data:"), encoded, true
}
