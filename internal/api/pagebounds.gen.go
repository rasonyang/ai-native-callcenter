// SPDX-License-Identifier: Apache-2.0

// Code generated from docs/openapi.json (bounded query parameters) by scripts/gen-pagebounds.mjs. DO NOT EDIT.

package api

// QueryBound is one query parameter's declared range: the contract's
// `minimum` and `maximum`. The Has* fields say which are declared —
// a bound of 0 is a real bound, not an absent one.
type QueryBound struct {
	Min, Max       int
	HasMin, HasMax bool
}

// QueryBoundsByRoute is every integer query parameter the contract bounds,
// keyed "METHOD /path" then parameter name — the same route key
// OperationSecurityByRoute uses. A route absent from it bounds nothing.
var QueryBoundsByRoute = map[string]map[string]QueryBound{
	"GET /audit-logs": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 0, HasMin: true, HasMax: false},
	},
	"GET /callbacks": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /calls/{callId}/transcript": {
		"limit":    {Min: 1, Max: 1000, HasMin: true, HasMax: true},
		"sinceSeq": {Min: 0, Max: 0, HasMin: true, HasMax: false},
	},
	"GET /campaigns": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /campaigns/{campaignId}/attempts": {
		"limit":  {Min: 0, Max: 1000, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /campaigns/{campaignId}/contacts": {
		"limit":  {Min: 0, Max: 1000, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /cdrs": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /cdrs/mine": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /contacts": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /dnc-numbers": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /test-numbers": {
		"limit":  {Min: 0, Max: 200, HasMin: true, HasMax: true},
		"offset": {Min: 0, Max: 1000000, HasMin: true, HasMax: true},
	},
	"GET /webhook-subscriptions/{subscriptionId}/deliveries": {
		"limit": {Min: 1, Max: 200, HasMin: true, HasMax: true},
	},
}
