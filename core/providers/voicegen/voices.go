package voicegen

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func (provider *VoicegenProvider) Passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	if request == nil || !providerUtils.IsVoiceCatalogPath(request.Path) {
		return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
	}
	options, id, err := providerUtils.ParseVoiceCatalogRequest(request)
	if err != nil {
		return nil, err
	}
	if prefix, value, found := strings.Cut(id, ":"); found && (prefix == "cartesia" || prefix == "elevenlabs") {
		if options.Source != "" && options.Source != prefix {
			return nil, providerUtils.VoiceCatalogError(400, "voice source conflicts with voice ID")
		}
		options.Source, id = prefix, value
		if !providerUtils.ValidVoiceID(id) {
			return nil, providerUtils.VoiceCatalogError(400, "invalid native voice ID")
		}
	}
	if options.Source == "" {
		options.Source = "cartesia"
		if strings.HasPrefix(options.Model, "elevenlabs:") {
			options.Source = "elevenlabs"
		}
	}
	if options.Source != "cartesia" && options.Source != "elevenlabs" {
		return nil, providerUtils.VoiceCatalogError(400, "invalid voicegen source")
	}
	path := "/api/v1/client/" + options.Source + "/voices"
	if id != "" {
		headers := map[string]string{}
		audio, err := provider.doWithHeaders(ctx, key, http.MethodGet, provider.networkConfig.BaseURL+path+"/"+url.PathEscape(id)+"/preview", nil, true, headers)
		if err != nil {
			return nil, err
		}
		if len(audio) == 0 {
			return nil, providerUtils.VoiceCatalogError(502, "voicegen returned an empty voice preview")
		}
		return providerUtils.VoicePreviewResponse(schemas.Voicegen, request.Path, audio, headers["Content-Type"]), nil
	}
	query := url.Values{"page": {strconv.Itoa(options.Page)}, "page_size": {"100"}}
	if options.Source == "cartesia" {
		query.Set("category", "all")
	} else if options.Language != "" {
		query.Set("language", options.Language)
	}
	var native VoicegenVoicesResponse
	if err := provider.json(ctx, key, http.MethodGet, path+"?"+query.Encode(), nil, &native); err != nil {
		return nil, err
	}
	catalog, err := ToBifrostVoiceCatalog(&native, options)
	if err != nil {
		return nil, err
	}
	return providerUtils.VoiceCatalogResponse(schemas.Voicegen, request.Path, catalog)
}

func ToBifrostVoiceCatalog(native *VoicegenVoicesResponse, options providerUtils.VoiceCatalogOptions) (providerUtils.VoiceCatalog, *schemas.BifrostError) {
	catalog := providerUtils.VoiceCatalog{Object: "list", Data: []providerUtils.VoiceCatalogEntry{}}
	if native.Voices == nil || native.Page != options.Page || native.TotalPages < 0 {
		return catalog, providerUtils.VoiceCatalogError(502, "invalid voicegen catalog pagination")
	}
	for _, row := range native.Voices {
		if !providerUtils.ValidVoiceID(row.VoiceID) {
			return catalog, providerUtils.VoiceCatalogError(502, "voicegen catalog omitted a valid voice ID")
		}
		language, gender := row.Language, row.Gender
		if language == "" {
			language = row.Labels["language"]
		}
		if gender == "" {
			gender = row.Labels["gender"]
		}
		if !providerUtils.VoiceLanguageMatches(language, options.Language, row.SupportedLanguages) {
			continue
		}
		id, name := options.Source+":"+row.VoiceID, row.Name
		if name == "" {
			name = row.VoiceID
		}
		catalog.Data = append(catalog.Data, providerUtils.VoiceCatalogEntry{ID: id, Name: name, Language: language, Gender: gender, PreviewURL: providerUtils.VoicePreviewURL(schemas.Voicegen, options, id)})
	}
	if native.Page < native.TotalPages {
		catalog.NextCursor = strconv.Itoa(native.Page + 1)
	}
	return catalog, nil
}
