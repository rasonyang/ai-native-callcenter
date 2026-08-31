// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"net/http"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
)

// The server implements the generated contract interface: an operation added
// to docs/openapi.json fails this build until the server grows its method.
// This is the compile-time half of spec-first; `make api-check` is the other.
//
// Each operation is implemented under its contract name in the file that owns
// its subject — extensions and queues in catalog_handlers.go, CDRs and reports
// in ledger_handlers.go, and so on. There are no adapters in between.
var _ api.ServerInterface = (*Server)(nil)

// apiWrapper mounts contract operations on the route table with the generated
// parameter binding in front, so path, query and header parameters arrive
// parsed and validated and no handler reads a raw parameter.
func (s *Server) apiWrapper() *api.ServerInterfaceWrapper {
	return &api.ServerInterfaceWrapper{
		Handler:          s,
		ErrorHandlerFunc: writeParamError,
		// The contract's own authorization, applied to every operation
		// mounted through the wrapper. It sits here rather than beside each
		// route because it reads the route: this is the first moment chi has
		// resolved which operation was matched, which is what lets the
		// generated table answer instead of a hand-placed guard.
		HandlerMiddlewares: []api.MiddlewareFunc{s.enforceContract},
	}
}

// writeParamError translates generated binding failures into the standard
// envelope: 400 VALIDATION_FAILED naming the parameter that was rejected.
func writeParamError(w http.ResponseWriter, _ *http.Request, err error) {
	field := ""
	var invalidFormat *api.InvalidParamFormatError
	var requiredParam *api.RequiredParamError
	var requiredHeader *api.RequiredHeaderError
	var unmarshaling *api.UnmarshalingParamError
	var tooMany *api.TooManyValuesForParamError
	switch {
	case errors.As(err, &invalidFormat):
		field = invalidFormat.ParamName
	case errors.As(err, &requiredParam):
		field = requiredParam.ParamName
	case errors.As(err, &requiredHeader):
		field = requiredHeader.ParamName
	case errors.As(err, &unmarshaling):
		field = unmarshaling.ParamName
	case errors.As(err, &tooMany):
		field = tooMany.ParamName
	}
	var params map[string]any
	if field != "" {
		params = map[string]any{"field": field}
	}
	writeError(w, http.StatusBadRequest, CodeValidationFailed, "invalid identifier", params)
}
