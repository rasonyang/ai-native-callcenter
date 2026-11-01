// SPDX-License-Identifier: Apache-2.0

// Package uuidgate is a CI gate: every id the server mints is a UUIDv7 (#400).
// Non-test code may not call the github.com/google/uuid constructors that
// produce a v4 (New, NewString, NewRandom, NewRandomFromReader). An id uses
// uuid.NewV7; a non-id random token (SIP Call-ID, tag, branch, nonce) uses
// crypto/rand, because a v7's leading hex is its millisecond timestamp.
package uuidgate
