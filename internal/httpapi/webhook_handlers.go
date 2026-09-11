// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// WebhookService is where finished calls are told to somebody else.
type WebhookService interface {
	List(ctx context.Context) ([]store.WebhookSubscription, error)
	Get(ctx context.Context, id uuid.UUID) (store.WebhookSubscription, error)
	Create(ctx context.Context, in store.WebhookSubscriptionWrite) (store.WebhookSubscription, error)
	Update(ctx context.Context, id uuid.UUID, in store.WebhookSubscriptionWrite) (store.WebhookSubscription, error)
	Delete(ctx context.Context, id uuid.UUID) (bool, error)
	Deliveries(ctx context.Context, id uuid.UUID, limit int) ([]store.WebhookDelivery, error)
}

func (s *Server) ListWebhookSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.webhooks.List(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read subscriptions", nil)
		return
	}
	items := make([]api.WebhookSubscription, 0, len(subs))
	for _, sub := range subs {
		items = append(items, apiWebhookSubscription(sub))
	}
	writeJSON(w, http.StatusOK, api.WebhookSubscriptionList{Items: items})
}

func (s *Server) GetWebhookSubscription(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	sub, err := s.webhooks.Get(r.Context(), id)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiWebhookSubscription(sub))
}

func (s *Server) CreateWebhookSubscription(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeSubscription(w, r)
	if !ok {
		return
	}
	sub, err := s.webhooks.Create(r.Context(), in)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, apiWebhookSubscription(sub))
}

func (s *Server) UpdateWebhookSubscription(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	in, ok := decodeSubscription(w, r)
	if !ok {
		return
	}
	sub, err := s.webhooks.Update(r.Context(), id, in)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiWebhookSubscription(sub))
}

func (s *Server) DeleteWebhookSubscription(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	deleted, err := s.webhooks.Delete(r.Context(), id)
	switch {
	case err != nil:
		writeSubscriptionError(w, err)
	case !deleted:
		writeError(w, http.StatusNotFound, CodeNotFound, "no such subscription", nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// deliveriesLimitDefault matches the contract's default; a client that says
// nothing gets a page rather than the whole history.
const deliveriesLimitDefault = 50

func (s *Server) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request,
	id uuid.UUID, params api.ListWebhookDeliveriesParams) {

	limit := deliveriesLimitDefault
	if params.Limit != nil {
		limit = *params.Limit
	}
	// The subscription is read first so a wrong id is 404 rather than an empty
	// list: "this subscriber has no deliveries" and "there is no such
	// subscriber" are different answers to the same screen.
	if _, err := s.webhooks.Get(r.Context(), id); err != nil {
		writeSubscriptionError(w, err)
		return
	}
	rows, err := s.webhooks.Deliveries(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read deliveries", nil)
		return
	}
	items := make([]api.WebhookDelivery, 0, len(rows))
	for _, row := range rows {
		items = append(items, apiWebhookDelivery(row))
	}
	writeJSON(w, http.StatusOK, api.WebhookDeliveryList{Items: items})
}

// decodeSubscription reads and validates a write body.
//
// The URL is checked here rather than at delivery time, which is the same rule
// the filter follows: a subscription that can never reach anywhere is refused
// when somebody writes it, not discovered as a customer receiving silence.
//
// The filter's keys are read from the raw body rather than from the decoded
// type, because a key the type has no field for decodes to nothing at all: a
// filter naming `talkSec` would be stored as "every call" and the subscriber
// would be told 201. The body is unmarshalled twice over the same bytes — once
// into the contract type, once into the keys — rather than with
// DisallowUnknownFields, which would also refuse unknown top-level fields and
// could not say which level it refused.
func decodeSubscription(w http.ResponseWriter, r *http.Request) (store.WebhookSubscriptionWrite, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return store.WebhookSubscriptionWrite{}, false
	}
	var body api.WebhookSubscriptionWrite
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return store.WebhookSubscriptionWrite{}, false
	}
	var keyed struct {
		Filter map[string]json.RawMessage `json:"filter"`
	}
	if err := json.Unmarshal(raw, &keyed); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return store.WebhookSubscriptionWrite{}, false
	}
	if unknown := store.UnknownWebhookFilterKeys(slices.Sorted(maps.Keys(keyed.Filter))); len(unknown) > 0 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			fmt.Sprintf("filter has no key %q", unknown[0]),
			map[string]any{"field": "filter", "key": unknown[0]})
		return store.WebhookSubscriptionWrite{}, false
	}
	if body.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a name is required", map[string]any{"field": "name"})
		return store.WebhookSubscriptionWrite{}, false
	}
	parsed, err := url.Parse(body.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"url must be an absolute http or https address",
			map[string]any{"field": "url"})
		return store.WebhookSubscriptionWrite{}, false
	}

	out := store.WebhookSubscriptionWrite{
		Name:      body.Name,
		URL:       body.URL,
		AuthToken: body.AuthToken,
		// Absent means enabled: somebody who has just written down where their
		// calls should go meant for them to go there.
		IsEnabled: body.IsEnabled == nil || *body.IsEnabled,
	}
	if body.Filter != nil {
		out.Filter = storeFilter(*body.Filter)
	}
	return out, true
}

func storeFilter(in api.WebhookFilter) store.WebhookFilter {
	var out store.WebhookFilter
	if in.CallType != nil {
		for _, t := range *in.CallType {
			out.CallType = append(out.CallType, string(t))
		}
	}
	if in.DID != nil {
		out.DID = *in.DID
	}
	if in.QueueID != nil {
		out.QueueID = *in.QueueID
	}
	if in.Status != nil {
		out.Status = *in.Status
	}
	if in.IsContained != nil {
		out.IsContained = *in.IsContained
	}
	return out
}

func apiFilter(in store.WebhookFilter) api.WebhookFilter {
	var out api.WebhookFilter
	if len(in.CallType) > 0 {
		types := make([]api.CallType, 0, len(in.CallType))
		for _, t := range in.CallType {
			types = append(types, api.CallType(t))
		}
		out.CallType = &types
	}
	if len(in.DID) > 0 {
		did := in.DID
		out.DID = &did
	}
	if len(in.QueueID) > 0 {
		queues := in.QueueID
		out.QueueID = &queues
	}
	if len(in.Status) > 0 {
		status := in.Status
		out.Status = &status
	}
	if len(in.IsContained) > 0 {
		contained := in.IsContained
		out.IsContained = &contained
	}
	return out
}

// apiWebhookSubscription maps a stored subscription onto the wire type.
//
// The token is not among the fields, and cannot be: the API type has no place
// for it. hasAuthToken is what a screen needs — whether this subscription would
// reach its endpoint with any credential at all — and is all it gets.
func apiWebhookSubscription(sub store.WebhookSubscription) api.WebhookSubscription {
	return api.WebhookSubscription{
		SubscriptionID: sub.SubscriptionID,
		Name:           sub.Name,
		URL:            sub.URL,
		Filter:         apiFilter(sub.Filter),
		IsEnabled:      sub.IsEnabled,
		HasAuthToken:   sub.AuthToken != "",
		CreatedAt:      sub.CreatedAt,
		UpdatedAt:      sub.UpdatedAt,
	}
}

func apiWebhookDelivery(d store.WebhookDelivery) api.WebhookDelivery {
	out := api.WebhookDelivery{
		DeliveryID:    d.DeliveryID,
		CallID:        d.CallID,
		Revision:      d.Revision,
		Status:        api.WebhookDeliveryStatus(d.Status),
		AttemptCount:  d.AttemptCount,
		NextAttemptAt: d.NextAttemptAt,
		CreatedAt:     d.CreatedAt,
	}
	if d.LastStatusCode != nil {
		out.LastStatusCode = d.LastStatusCode
	}
	if d.LastError != "" {
		reason := d.LastError
		out.LastError = &reason
	}
	if !d.DeliveredAt.IsZero() {
		at := d.DeliveredAt
		out.DeliveredAt = &at
	}
	return out
}

func writeSubscriptionError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such subscription", nil)
		return
	}
	writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot reach storage", nil)
}
