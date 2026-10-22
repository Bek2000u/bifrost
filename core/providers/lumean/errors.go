package lumean

import (
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseLumeanError(resp *fasthttp.Response) *schemas.BifrostError {
	var wire LumeanError
	err := providerUtils.HandleProviderAPIError(resp, &wire)
	if wire.Message != "" {
		err.Error.Message = wire.Message
	}
	if wire.Error != "" {
		err.Error.Message = wire.Error
	}
	return err
}
