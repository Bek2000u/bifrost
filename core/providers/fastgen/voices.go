package fastgen

import (
	"net/http"
	"net/url"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func (provider *FastgenProvider) Passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	if request == nil || !providerUtils.IsVoiceCatalogPath(request.Path) {
		return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
	}
	options, id, err := providerUtils.ParseVoiceCatalogRequest(request)
	if err != nil {
		return nil, err
	}
	if options.Model == "" || options.Source != "" && options.Source != "aistudio" {
		return nil, providerUtils.VoiceCatalogError(400, "fastgen voice catalog requires a native model and aistudio source")
	}
	options.Source = "aistudio"
	data, _, err := provider.request(ctx, key, http.MethodGet, "/api/v6/voices?"+url.Values{"model": {options.Model}}.Encode(), nil, false)
	if err != nil {
		return nil, err
	}
	var native []FastgenVoice
	if decode := sonic.Unmarshal(data, &native); decode != nil || native == nil {
		return nil, providerUtils.VoiceCatalogError(502, "invalid fastgen voice catalog")
	}
	catalog, previews, err := ToBifrostVoiceCatalog(native, options, provider.networkConfig.BaseURL)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return providerUtils.VoiceCatalogResponse(schemas.Fastgen, request.Path, catalog)
	}
	target := previews[id]
	if target == "" {
		return nil, providerUtils.VoiceCatalogError(404, "fastgen voice preview is unavailable")
	}
	audio, mime, err := provider.download(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(audio) == 0 {
		return nil, providerUtils.VoiceCatalogError(502, "fastgen returned an empty voice preview")
	}
	return providerUtils.VoicePreviewResponse(schemas.Fastgen, request.Path, audio, mime), nil
}

func ToBifrostVoiceCatalog(rows []FastgenVoice, options providerUtils.VoiceCatalogOptions, base string) (providerUtils.VoiceCatalog, map[string]string, *schemas.BifrostError) {
	catalog := providerUtils.VoiceCatalog{Object: "list", Data: []providerUtils.VoiceCatalogEntry{}}
	previews := map[string]string{}
	origin, _ := url.Parse(base)
	for _, row := range rows {
		if row.Provider != "aistudio" {
			continue
		}
		matched := false
		for _, model := range row.Models {
			if model == options.Model {
				matched = true
				break
			}
		}
		if !matched || !providerUtils.VoiceLanguageMatches(row.Language, options.Language, nil) {
			continue
		}
		if !providerUtils.ValidVoiceID(row.ID) {
			return catalog, nil, providerUtils.VoiceCatalogError(502, "fastgen voice catalog omitted a valid ID")
		}
		name := row.Name
		if name == "" {
			name = row.ID
		}
		voice := providerUtils.VoiceCatalogEntry{ID: row.ID, Name: name, Language: row.Language, Gender: row.Gender}
		sample := row.PreviewURLs["en"]
		if sample == "" {
			sample = row.PreviewURLs["ru"]
		}
		if sample == "" {
			sample = row.PreviewURL
		}
		if target, err := url.Parse(sample); sample != "" && err == nil && origin != nil {
			resolved := origin.ResolveReference(target).String()
			if validateArtifact(resolved, base) == nil {
				previews[row.ID] = resolved
				voice.PreviewURL = providerUtils.VoicePreviewURL(schemas.Fastgen, options, row.ID)
			}
		}
		catalog.Data = append(catalog.Data, voice)
	}
	return catalog, previews, nil
}
