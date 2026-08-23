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
	CodeValidationFailed   ErrorCode = "VALIDATION_FAILED"
	CodeNotFound           ErrorCode = "NOT_FOUND"
	CodeConflict           ErrorCode = "CONFLICT"
	CodeExtensionInUse     ErrorCode = "EXTENSION_IN_USE"
	// CodeExtensionAssignedToAgent refuses to delete an extension somebody
	// works at. Distinct from CodeExtensionInUse, which is a sign-in
	// collision: this one is about the binding, not the session.
	CodeExtensionAssignedToAgent ErrorCode = "EXTENSION_ASSIGNED_TO_AGENT"
	CodeAgentAlreadyLoggedIn     ErrorCode = "AGENT_ALREADY_LOGGED_IN"
	CodeAgentNotLoggedIn         ErrorCode = "AGENT_NOT_LOGGED_IN"
	CodeAgentNotInWrapUp         ErrorCode = "AGENT_NOT_IN_WRAP_UP"
	CodeCallNotFound             ErrorCode = "CALL_NOT_FOUND"
	CodeNotCallParty             ErrorCode = "NOT_CALL_PARTY"
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

// violatesConstraint reports a PostgreSQL foreign-key failure raised by one
// named constraint. Named, because the answer differs per constraint: a
// referenced row that must not disappear is a conflict the operator can
// resolve, and saying which one lets the message say what to do about it.
func violatesConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == name
}
