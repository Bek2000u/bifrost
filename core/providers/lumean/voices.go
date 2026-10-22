package lumean

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func (provider *LumeanProvider) Passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	if request == nil || !providerUtils.IsVoiceCatalogPath(request.Path) {
		return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
	}
	options, id, err := providerUtils.ParseVoiceCatalogRequest(request)
	if err != nil {
		return nil, err
	}
	if options.Source == "" {
		options.Source = "elevenlabs"
		if strings.HasPrefix(options.Model, "sonic-") {
			options.Source = "cartesia"
		}
	}
	if options.Source != "cartesia" && options.Source != "elevenlabs" {
		return nil, providerUtils.VoiceCatalogError(400, "invalid lumean voice source")
	}
	path := "/voices/elevenlabs/library"
	query := url.Values{"page": {strconv.Itoa(options.Page - 1)}, "page_size": {"100"}}
	if options.Language != "" {
		query.Set("required_languages", options.Language)
	}
	if options.Source == "cartesia" {
		path = "/voices/cartesia"
		query = url.Values{"page": {strconv.Itoa(options.Page)}, "per_page": {"100"}}
		if options.Language != "" {
			query.Set("language", options.Language)
		}
	}
	var native LumeanVoicesResponse
	if err := provider.json(ctx, key, http.MethodGet, path+"?"+query.Encode(), nil, &native); err != nil {
		return nil, err
	}
	catalog, previews, err := provider.ToBifrostVoiceCatalog(&native, options)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return providerUtils.VoiceCatalogResponse(schemas.Lumean, request.Path, catalog)
	}
	target := previews[id]
	if target == "" {
		return nil, providerUtils.VoiceCatalogError(404, "lumean voice preview is unavailable")
	}
	headers := map[string]string{}
	audio, err := provider.doWithHeaders(ctx, key, http.MethodGet, target, nil, false, headers)
	if err != nil {
		return nil, err
	}
	if len(audio) == 0 {
		return nil, providerUtils.VoiceCatalogError(502, "lumean returned an empty voice preview")
	}
	return providerUtils.VoicePreviewResponse(schemas.Lumean, request.Path, audio, headers["Content-Type"]), nil
}

func (provider *LumeanProvider) ToBifrostVoiceCatalog(native *LumeanVoicesResponse, options providerUtils.VoiceCatalogOptions) (providerUtils.VoiceCatalog, map[string]string, *schemas.BifrostError) {
	catalog := providerUtils.VoiceCatalog{Object: "list", Data: []providerUtils.VoiceCatalogEntry{}}
	previews := map[string]string{}
	rows := native.Data.Voices
	hasMore := false
	if native.Success != nil && !*native.Success {
		return catalog, nil, providerUtils.VoiceCatalogError(502, "lumean voice catalog failed")
	}
	if options.Source == "cartesia" {
		rows = native.Data.Items
		if native.Data.CurrentPage != options.Page || native.Data.LastPage < options.Page {
			return catalog, nil, providerUtils.VoiceCatalogError(502, "invalid lumean cartesia pagination")
		}
		hasMore = native.Data.CurrentPage < native.Data.LastPage
	} else {
		if native.Data.HasMore == nil {
			return catalog, nil, providerUtils.VoiceCatalogError(502, "invalid lumean elevenlabs pagination")
		}
		hasMore = *native.Data.HasMore
	}
	if rows == nil {
		return catalog, nil, providerUtils.VoiceCatalogError(502, "lumean voice catalog omitted voices")
	}
	origin, _ := url.Parse(provider.networkConfig.BaseURL)
	for _, row := range rows {
		if !providerUtils.ValidVoiceID(row.VoiceID) {
			return catalog, nil, providerUtils.VoiceCatalogError(502, "lumean voice catalog omitted a valid voice ID")
		}
		language, gender := row.Language, row.Gender
		if language == "" {
			language = row.Labels["language"]
		}
		if gender == "" {
			gender = row.Labels["gender"]
		}
		if !providerUtils.VoiceLanguageMatches(language, options.Language, nil) {
			continue
		}
		name := row.Name
		if name == "" {
			name = row.VoiceID
		}
		voice := providerUtils.VoiceCatalogEntry{ID: row.VoiceID, Name: name, Language: language, Gender: gender}
		if target, err := url.Parse(row.PreviewURL); row.PreviewURL != "" && err == nil && origin != nil {
			resolved := origin.ResolveReference(target).String()
			if provider.artifactURL(resolved) {
				previews[row.VoiceID] = resolved
				voice.PreviewURL = providerUtils.VoicePreviewURL(schemas.Lumean, options, row.VoiceID)
			}
		}
		catalog.Data = append(catalog.Data, voice)
	}
	if hasMore {
		catalog.NextCursor = strconv.Itoa(options.Page + 1)
	}
	return catalog, previews, nil
}
