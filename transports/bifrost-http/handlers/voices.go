package handlers

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// Voices are read-only provider metadata and samples. Reuse passthrough so the
// normal provider/key selection and governance hooks also apply to these reads.
func prepareVoiceRequest(ctx *fasthttp.RequestCtx) (*schemas.BifrostPassthroughRequest, error) {
	query, err := url.ParseQuery(string(ctx.URI().QueryString()))
	if err != nil {
		return nil, fmt.Errorf("invalid voice query")
	}
	provider := schemas.ModelProvider(query.Get("provider"))
	model := query.Get("model")
	modelProvider, nativeModel := schemas.ParseModelString(model, "")
	if modelProvider != "" {
		if provider != "" && provider != modelProvider {
			return nil, fmt.Errorf("voice model and provider do not match")
		}
		provider, model = modelProvider, nativeModel
	}
	switch provider {
	case schemas.Fastgen, schemas.Voicegen, schemas.Lumean:
	default:
		return nil, fmt.Errorf("provider must be fastgen, voicegen or lumean")
	}
	query.Del("provider")
	if model != "" {
		query.Set("model", model)
	}
	path := "/v1/audio/voices"
	if id, ok := ctx.UserValue("voice_id").(string); ok && id != "" {
		path += "/" + url.PathEscape(id) + "/preview"
	}
	return &schemas.BifrostPassthroughRequest{
		Provider: provider,
		Model:    model,
		Method:   "GET",
		Path:     path,
		RawQuery: query.Encode(),
	}, nil
}

func (h *CompletionHandler) voices(ctx *fasthttp.RequestCtx) {
	req, err := prepareVoiceRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	bifrostCtx, cancel := lib.ConvertToBifrostContext(ctx, h.config)
	defer cancel()
	if bifrostCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	resp, bifrostErr := h.client.Passthrough(bifrostCtx, req.Provider, req)
	if bifrostErr != nil {
		forwardProviderHeadersFromContext(ctx, bifrostCtx)
		SendBifrostError(ctx, bifrostErr)
		return
	}
	ctx.SetStatusCode(resp.StatusCode)
	for name, value := range resp.Headers {
		if strings.EqualFold(name, "content-type") {
			ctx.Response.Header.SetContentType(value)
		}
	}
	lib.ApplyBifrostResponseHeaders(ctx, bifrostCtx, resp.ExtraFields)
	ctx.Response.SetBody(resp.Body)
}
