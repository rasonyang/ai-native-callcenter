// SPDX-License-Identifier: Apache-2.0

// Package httpapi exposes the REST and SSE surface and serves the SPA.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrorCode is the machine-readable, translatable failure identifier.
// The frontend renders errors.<CODE>; the backend never localizes.
type ErrorCode string

// Error codes shared by the API surface.
const (
	CodeInvalidCredentials ErrorCode = "INVALID_CREDENTIALS"
	CodeSessionExpired     ErrorCode = "SESSION_EXPIRED"
	CodeForbidden          ErrorCode = "FORBIDDEN"
	// CodeAgentRequired is not a denial. The operation works through an
	// agent identity — a presence change, a leg of a call, an agent's own
	// history — and the credential has none: an administrator who takes no
	// calls, or a key that named no agent. Distinct from FORBIDDEN, which
	// would send the caller looking for a permission to add.
	CodeAgentRequired ErrorCode = "AGENT_REQUIRED"
	// CodeAgentImpersonationNotAllowed refuses X-AICC-Agent-ID on a browser
	// session. An account is bound to at most one agent identity, so the
	// header could only ever mean "act as somebody else".
	CodeAgentImpersonationNotAllowed ErrorCode = "AGENT_IMPERSONATION_NOT_ALLOWED"
	// CodeInsufficientScope names what is missing. FORBIDDEN says only that
	// the answer is no, which leaves a caller unable to tell a capability
	// they were never granted from a rule about this particular row.
	CodeInsufficientScope ErrorCode = "INSUFFICIENT_SCOPE"
	CodeValidationFailed  ErrorCode = "VALIDATION_FAILED"
	// CodeUserDataTooLarge refuses business data rather than truncating it:
	// a screen showing half a customer's details is worse than one saying the
	// request was refused.
	CodeUserDataTooLarge ErrorCode = "USER_DATA_TOO_LARGE"
	CodeNotFound         ErrorCode = "NOT_FOUND"
	// CodeMethodNotAllowed answers the router's own refusal, so that a
	// wrong method lands in the same envelope as everything else. Without
	// it chi replies 405 with an empty body and no content type, and the
	// contract's promise that errors *always* use the envelope is false.
	CodeMethodNotAllowed ErrorCode = "METHOD_NOT_ALLOWED"
	CodeConflict         ErrorCode = "CONFLICT"
	CodeExtensionInUse   ErrorCode = "EXTENSION_IN_USE"
	// CodeExtensionAssignedToAgent refuses to delete an extension somebody
	// works at. Distinct from CodeExtensionInUse, which is a sign-in
	// collision: this one is about the binding, not the session.
	CodeExtensionAssignedToAgent ErrorCode = "EXTENSION_ASSIGNED_TO_AGENT"
	// CodeExtensionPoolExhausted is not a collision: nothing the operator
	// asked for was taken, the deployment has run out of numbers.
	CodeExtensionPoolExhausted ErrorCode = "EXTENSION_POOL_EXHAUSTED"
	// CodeLastAdmin refuses the change that would leave nobody able to
	// administer the product.
	CodeLastAdmin            ErrorCode = "LAST_ADMIN"
	CodeAgentAlreadyLoggedIn ErrorCode = "AGENT_ALREADY_LOGGED_IN"
	CodeAgentNotLoggedIn     ErrorCode = "AGENT_NOT_LOGGED_IN"
	CodeAgentNotInWrapUp     ErrorCode = "AGENT_NOT_IN_WRAP_UP"
	CodeCallNotFound         ErrorCode = "CALL_NOT_FOUND"
	CodeNotCallParty         ErrorCode = "NOT_CALL_PARTY"
	// CodeOperationNotAllowedForCallType refuses a control this kind of
	// call does not offer, rather than a control this caller may not use.
	CodeOperationNotAllowedForCallType ErrorCode = "OPERATION_NOT_ALLOWED_FOR_CALL_TYPE"
	CodeUserSuspended                  ErrorCode = "USER_SUSPENDED"
	CodeSwitchDown                     ErrorCode = "SWITCH_DOWN"
	CodeStorageDown                    ErrorCode = "STORAGE_DOWN"
	CodeRateLimited                    ErrorCode = "RATE_LIMITED"
	CodeInternal                       ErrorCode = "INTERNAL"
)

// APIError is the single error envelope of the API.
type APIError struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Params  map[string]any `json:"params,omitempty"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "error", err)
	}
}

// writeError writes the standard error envelope.
func writeError(w http.ResponseWriter, status int, code ErrorCode, message string, params map[string]any) {
	writeJSON(w, status, errorEnvelope{Error: APIError{Code: code, Message: message, Params: params}})
}

// isUniqueViolation reports a PostgreSQL unique-constraint failure, which
// means the operator chose an identifier somebody already has.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// violatesConstraint reports a PostgreSQL referential failure raised by one
// named constraint. Named, because the answer differs per constraint: a
// referenced row that must not disappear is a conflict the operator can
// resolve, and saying which one lets the message say what to do about it.
//
// Two SQLSTATEs, because PostgreSQL uses different ones for the same idea:
// an explicit ON DELETE RESTRICT raises 23001 restrict_violation, while
// NO ACTION and the insert side raise 23503 foreign_key_violation. Matching
// only 23503 — which is the one everybody knows — let a live delete fall
// through to "storage down" while every test agreed it would not.
func violatesConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != name {
		return false
	}
	return pgErr.Code == "23001" || pgErr.Code == "23503"
}
