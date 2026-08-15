// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// OutboundService places calls on request.
type OutboundService interface {
	Dial(ctx context.Context, agentExtension, destination string) (uuid.UUID, error)
	DialAI(ctx context.Context, req outbound.AIDialRequest) (uuid.UUID, error)
}

// The dial and create-call operations themselves live in api_server.go
// (DialCall, CreateCall), on the generated contract types.

func writeOutboundError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, outbound.ErrBadNumber):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "not a dialable number", nil)
	case errors.Is(err, outbound.ErrUnknownDID):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "no such DID", nil)
	case errors.Is(err, outbound.ErrFlowless):
		writeError(w, http.StatusConflict, CodeConflict, "the DID has no published flow", nil)
	default:
		writeError(w, http.StatusBadGateway, CodeSwitchDown, "the switch did not take the call", nil)
	}
}
