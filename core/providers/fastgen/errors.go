package fastgen

import (
	"fmt"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseFastgenError(resp *fasthttp.Response) *schemas.BifrostError {
	var native FastgenError
	err := providerUtils.HandleProviderAPIError(resp, &native)
	if native.Message != "" {
		err.Error.Message = native.Message
	}
	if native.Code != "" {
		err.Error.Code = &native.Code
	}
	if len(native.Error) > 0 {
		var message string
		if sonic.Unmarshal(native.Error, &message) == nil && message != "" {
			err.Error.Message = message
		}
	}
	return err
}

func invalidInput(message string) *schemas.BifrostError {
	return &schemas.BifrostError{IsBifrostError: true, StatusCode: schemas.Ptr(400), AllowFallbacks: schemas.Ptr(false), Error: &schemas.ErrorField{Type: schemas.Ptr("invalid_request_error"), Message: message}}
}

// recoveryRequired stops core retry/key rotation as well as fallbacks: the upstream
// may have accepted a paid request, so its receipt must be reconciled, never resubmitted.
func recoveryRequired(err *schemas.BifrostError, id string) *schemas.BifrostError {
	if err == nil {
		err = providerUtils.NewBifrostOperationError("fastgen receipt is invalid", nil)
	}
	err.IsBifrostError = true
	err.AllowFallbacks = schemas.Ptr(false)
	if err.Error == nil {
		err.Error = &schemas.ErrorField{}
	}
	err.Error.Code = schemas.Ptr("fastgen_reconciliation_required")
	if id != "" {
		err.Error.Message = fmt.Sprintf("fastgen job %s requires reconciliation: %s", id, err.Error.Message)
	}
	return err
}
