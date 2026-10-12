// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"errors"
	"fmt"
)

// The kinds of refusal. Each is one error code of the API (CAMPAIGN_...);
// the transport maps them, this package only says which one it is.
var (
	// ErrNotFound: no such campaign.
	ErrNotFound = errors.New("campaign: not found")
	// ErrValidation: a value is not acceptable; Field and Rule say which.
	ErrValidation = errors.New("campaign: invalid value")
	// ErrInvalidTransition: the operation does not exist in the campaign's
	// current status (start a RUNNING campaign, delete a PAUSED one, import
	// into a CANCELLED one).
	ErrInvalidTransition = errors.New("campaign: not allowed in this status")
	// ErrFieldLocked: the field cannot change in the campaign's current
	// status.
	ErrFieldLocked = errors.New("campaign: field is locked")
	// ErrFlowHasTransfer: the DID's flow can hand the caller to a person, and
	// a campaign call has no way to one.
	ErrFlowHasTransfer = errors.New("campaign: the flow can transfer to a person")
	// ErrDIDNotOutbound: the DID does not allow outbound calls, and a campaign
	// dials from its DID.
	ErrDIDNotOutbound = errors.New("campaign: the DID does not allow outbound calls")
	// ErrRingTimeoutTooLong: the ring timeout is not below the answer window.
	ErrRingTimeoutTooLong = errors.New("campaign: ring timeout is not below the answer window")
	// ErrWindowOutsideLimit: the calling window is not inside the
	// deployment's outer calling hours.
	ErrWindowOutsideLimit = errors.New("campaign: calling window is outside the permitted hours")
)

// Error is a refusal with the detail a client needs to fix the request.
type Error struct {
	Kind error
	// Field is the request property at fault, in the contract's spelling
	// ("callingWindow.ranges[1]"); empty when the refusal is not about one.
	Field string
	// Rule is a stable SCREAMING_SNAKE reason within the kind.
	Rule    string
	Message string
	// Params are extra machine-readable detail (locked fields, the current
	// status).
	Params map[string]any
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

func invalid(field, rule, format string, args ...any) *Error {
	return &Error{Kind: ErrValidation, Field: field, Rule: rule, Message: fmt.Sprintf(format, args...)}
}

// NewNotFound is the refusal for an unknown campaign.
func NewNotFound() *Error { return &Error{Kind: ErrNotFound, Message: "no such campaign"} }

// NewTransitionError is the refusal of an action the status does not offer.
func NewTransitionError(status Status, action string) *Error {
	return &Error{
		Kind: ErrInvalidTransition, Rule: action,
		Message: fmt.Sprintf("%s is not possible while the campaign is %s", action, status),
		Params:  map[string]any{"status": string(status), "action": action},
	}
}

func locked(status Status, fields []string) *Error {
	return &Error{
		Kind: ErrFieldLocked, Rule: "FIELD_LOCKED",
		Message: fmt.Sprintf("these fields cannot change while the campaign is %s: %v", status, fields),
		Params:  map[string]any{"status": string(status), "fields": fields},
	}
}
