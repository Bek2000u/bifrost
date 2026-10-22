package fal

import (
	"fmt"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseFalError(response *fasthttp.Response) *schemas.BifrostError {
	var native FalError
	err := providerUtils.HandleProviderAPIError(response, &native)
	if native.Message != "" {
		err.Error.Message = native.Message
	} else if len(native.Detail) > 0 {
		var detail string
		if sonic.Unmarshal(native.Detail, &detail) == nil {
			err.Error.Message = detail
		} else {
			err.Error.Message = string(native.Detail)
		}
	}
	return err
}

func recoveryRequired(err *schemas.BifrostError, id string) *schemas.BifrostError {
	if err == nil {
		err = providerUtils.NewBifrostOperationError("fal receipt is invalid", nil)
	}
	err.IsBifrostError = true
	err.AllowFallbacks = schemas.Ptr(false)
	if err.Error == nil {
		err.Error = &schemas.ErrorField{}
	}
	err.Error.Code = schemas.Ptr("fal_reconciliation_required")
	if id != "" {
		err.Error.Message = fmt.Sprintf("fal request %s requires reconciliation: %s", id, err.Error.Message)
	}
	return err
}
