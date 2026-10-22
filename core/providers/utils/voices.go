package utils

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

type VoiceCatalogEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Language   string `json:"language,omitempty"`
	Gender     string `json:"gender,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`
}

type VoiceCatalog struct {
	Object     string              `json:"object"`
	Data       []VoiceCatalogEntry `json:"data"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type VoiceCatalogOptions struct {
	Model, Source, Language, Cursor string
	Page                            int
}

func IsVoiceCatalogPath(path string) bool {
	return path == "/v1/audio/voices" || strings.HasPrefix(path, "/v1/audio/voices/") && strings.HasSuffix(path, "/preview")
}

func VoiceCatalogError(status int, message string) *schemas.BifrostError {
	return &schemas.BifrostError{IsBifrostError: true, StatusCode: schemas.Ptr(status), AllowFallbacks: schemas.Ptr(false), Error: &schemas.ErrorField{Message: message}}
}

func ParseVoiceCatalogRequest(request *schemas.BifrostPassthroughRequest) (VoiceCatalogOptions, string, *schemas.BifrostError) {
	options := VoiceCatalogOptions{Page: 1}
	if request.Method != "GET" {
		return options, "", VoiceCatalogError(405, "voice catalog and preview require GET")
	}
	query, err := url.ParseQuery(request.RawQuery)
	if err != nil {
		return options, "", VoiceCatalogError(400, "invalid voice query")
	}
	options.Model = request.Model
	if options.Model == "" {
		options.Model = query.Get("model")
	}
	options.Source, options.Language, options.Cursor = query.Get("source"), query.Get("language"), query.Get("cursor")
	if options.Cursor != "" {
		options.Page, err = strconv.Atoi(options.Cursor)
		if err != nil || options.Page < 1 {
			return options, "", VoiceCatalogError(400, "voice cursor must be a positive page number")
		}
	}
	var id string
	if request.Path != "/v1/audio/voices" {
		id, err = url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(request.Path, "/v1/audio/voices/"), "/preview"))
		if err != nil || !ValidVoiceID(id) {
			return options, "", VoiceCatalogError(400, "invalid voice ID")
		}
	}
	return options, id, nil
}

func ValidVoiceID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\r\n")
}

func VoicePreviewURL(provider schemas.ModelProvider, options VoiceCatalogOptions, id string) string {
	query := url.Values{"provider": {string(provider)}}
	for key, value := range map[string]string{"model": options.Model, "source": options.Source, "language": options.Language, "cursor": options.Cursor} {
		if value != "" {
			query.Set(key, value)
		}
	}
	return "/v1/audio/voices/" + url.PathEscape(id) + "/preview?" + query.Encode()
}

func VoiceLanguageMatches(language, selected string, supported []string) bool {
	if selected == "" || language == "" || strings.HasPrefix(strings.ToLower(language), strings.ToLower(selected)) {
		return true
	}
	for _, value := range supported {
		if strings.EqualFold(value, selected) {
			return true
		}
	}
	return false
}

func VoiceCatalogResponse(provider schemas.ModelProvider, path string, catalog VoiceCatalog) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	body, err := MarshalSorted(catalog)
	if err != nil {
		return nil, VoiceCatalogError(502, "invalid normalized voice catalog")
	}
	return VoicePreviewResponse(provider, path, body, "application/json"), nil
}

func VoicePreviewResponse(provider schemas.ModelProvider, path string, body []byte, contentType string) *schemas.BifrostPassthroughResponse {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &schemas.BifrostPassthroughResponse{StatusCode: 200, Path: path, Body: body, Headers: map[string]string{"Content-Type": contentType}, ExtraFields: schemas.BifrostResponseExtraFields{Provider: provider}}
}
