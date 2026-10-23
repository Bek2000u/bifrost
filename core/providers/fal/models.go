package fal

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

func (provider *FalProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, provider.listModelsByKey)
}
func (provider *FalProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	query := url.Values{"limit": {"100"}}
	for name, value := range request.ExtraParams {
		switch typed := value.(type) {
		case []string:
			for _, item := range typed {
				query.Add(name, item)
			}
		default:
			query.Set(name, fmt.Sprint(value))
		}
	}
	// Find mode avoids walking the entire catalog for a small exact allowlist.
	// Wildcards, regexes, aliases and unfiltered discovery keep the full listing.
	targeted := !request.Unfiltered && len(key.Models) > 0 && len(key.Models) <= 50 && key.Models.IsRestricted() && len(key.Aliases) == 0 && !query.Has("endpoint_id")
	if targeted {
		for _, model := range key.Models {
			if schemas.IsRegexEntry(model) {
				targeted = false
				break
			}
		}
	}
	if targeted {
		for _, model := range key.Models {
			query.Add("endpoint_id", model)
		}
	}
	rows := []FalModel{}
	cursors := map[string]bool{}
	for {
		data, _, err := provider.request(ctx, key, http.MethodGet, provider.platformBaseURL+"/models?"+query.Encode(), nil, "application/json", true, false)
		if err != nil {
			return nil, err
		}
		var page FalModelsPage
		if conversion := sonic.Unmarshal(data, &page); conversion != nil {
			return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, conversion)
		}
		for _, row := range page.Models {
			if row.EndpointID == "" {
				return nil, providerUtils.NewBifrostOperationError("fal catalog omitted endpoint_id", nil)
			}
		}
		rows = append(rows, page.Models...)
		if page.HasMore == nil {
			return nil, providerUtils.NewBifrostOperationError("fal catalog omitted pagination state", nil)
		}
		if !*page.HasMore {
			break
		}
		if page.NextCursor == "" || cursors[page.NextCursor] {
			return nil, providerUtils.NewBifrostOperationError("invalid fal catalog pagination cursor", nil)
		}
		cursors[page.NextCursor] = true
		query.Set("cursor", page.NextCursor)
	}
	return ToBifrostListModelsResponse(rows, key, request.Unfiltered), nil
}
func ToBifrostListModelsResponse(rows []FalModel, key schemas.Key, unfiltered bool) *schemas.BifrostListModelsResponse {
	pipeline := providerUtils.ListModelsPipeline{AllowedModels: key.Models, BlacklistedModels: key.BlacklistedModels, Aliases: key.Aliases, Unfiltered: unfiltered, ProviderKey: schemas.Fal, MatchFns: providerUtils.DefaultMatchFns()}
	response := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	if pipeline.ShouldEarlyExit() {
		return response
	}
	included := map[string]bool{}
	for _, row := range rows {
		for _, result := range pipeline.FilterModel(row.EndpointID) {
			if included[result.ResolvedID] {
				continue
			}
			model := schemas.Model{ID: "fal/" + result.ResolvedID, OwnedBy: schemas.Ptr("fal"), IsDeprecated: row.Metadata.Status == "deprecated"}
			if row.Metadata.DisplayName != "" {
				model.Name = schemas.Ptr(row.Metadata.DisplayName)
			}
			if row.Metadata.Description != "" {
				model.Description = schemas.Ptr(row.Metadata.Description)
			}
			if row.Metadata.Category != "" {
				model.AdditionalAttributes = map[string]string{"category": row.Metadata.Category}
			}
			if result.AliasValue != "" {
				model.Alias = schemas.Ptr(result.AliasValue)
			}
			response.Data = append(response.Data, model)
			included[result.ResolvedID] = true
		}
	}
	response.Data = append(response.Data, pipeline.BackfillModels(included)...)
	return response
}
