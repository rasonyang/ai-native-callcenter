// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// Delivery statuses. SCREAMING_SNAKE and byte-identical to the column's CHECK,
// to the contract's enum and to what the screen renders.
const (
	DeliveryPending   = "PENDING"
	DeliveryDelivered = "DELIVERED"
	DeliveryFailed    = "FAILED"
)

// WebhookFilter is which finished calls a subscription wants.
//
// A field left empty does not constrain; a field with values requires the
// CDR's own to be one of them; the fields are ANDed. The zero value therefore
// means "every call", which is what an operator who names no filter means.
//
// Deliberately a fixed set of fields rather than an expression language. The
// keys are checked when a subscription is written, so a filter that can never
// match is refused at creation instead of being discovered months later as a
// customer receiving silence (design 09 §5).
type WebhookFilter struct {
	CallType    []string    `json:"callType,omitempty"`
	DID         []string    `json:"did,omitempty"`
	QueueID     []uuid.UUID `json:"queueId,omitempty"`
	Status      []string    `json:"status,omitempty"`
	IsContained []bool      `json:"isContained,omitempty"`
}

// Matches reports whether this call is one the subscription asked for.
func (f WebhookFilter) Matches(cdr CDR) bool {
	if len(f.CallType) > 0 && !slices.Contains(f.CallType, cdr.CallType) {
		return false
	}
	if len(f.DID) > 0 && !slices.Contains(f.DID, cdr.DID) {
		return false
	}
	if len(f.Status) > 0 && !slices.Contains(f.Status, cdr.Status) {
		return false
	}
	if len(f.IsContained) > 0 && !slices.Contains(f.IsContained, cdr.IsContained) {
		return false
	}
	if len(f.QueueID) > 0 {
		// A call that never reached a queue matches no queue filter. Said
		// explicitly rather than left to a nil dereference.
		if cdr.QueueID == nil || !slices.Contains(f.QueueID, *cdr.QueueID) {
			return false
		}
	}
	return true
}

// WebhookSubscription is a place finished calls are delivered to.
//
// AuthToken is the customer's own credential and travels outward; it is never
// served to a client, which is why the API type carries HasAuthToken instead.
type WebhookSubscription struct {
	SubscriptionID uuid.UUID     `json:"subscriptionId"`
	Name           string        `json:"name"`
	URL            string        `json:"url"`
	Filter         WebhookFilter `json:"filter"`
	AuthToken      string        `json:"-"`
	IsEnabled      bool          `json:"isEnabled"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
}

// WebhookDelivery is one attempt-set at telling one subscription about one
// revision of one call.
type WebhookDelivery struct {
	DeliveryID     uuid.UUID `json:"deliveryId"`
	SubscriptionID uuid.UUID `json:"subscriptionId"`
	CallID         uuid.UUID `json:"callId"`
	Revision       int       `json:"revision"`
	Payload        []byte    `json:"-"`
	Status         string    `json:"status"`
	AttemptCount   int       `json:"attemptCount"`
	NextAttemptAt  time.Time `json:"nextAttemptAt"`
	LastStatusCode *int      `json:"lastStatusCode,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	DeliveredAt    time.Time `json:"deliveredAt,omitzero"`
}

// WebhookStore holds subscriptions and the outbox that serves them.
type WebhookStore struct {
	q    *queries.Queries
	pool *pgxpool.Pool
}

// Webhooks returns the subscription store.
func (s *Store) Webhooks() *WebhookStore {
	return &WebhookStore{q: s.Queries, pool: s.Pool}
}

// WebhookSubscriptionWrite is what a create or update supplies.
//
// AuthToken is a pointer so that omitted and empty are different requests:
// nil leaves the stored token alone, a pointer to "" clears it. A plain string
// could not tell them apart, and a PUT that forgot the field would silently
// disarm the subscription's credential.
type WebhookSubscriptionWrite struct {
	Name      string
	URL       string
	Filter    WebhookFilter
	AuthToken *string
	IsEnabled bool
}

func (w *WebhookStore) Create(ctx context.Context, in WebhookSubscriptionWrite) (WebhookSubscription, error) {
	filter, err := json.Marshal(in.Filter)
	if err != nil {
		return WebhookSubscription{}, fmt.Errorf("encode filter: %w", err)
	}
	var token string
	if in.AuthToken != nil {
		token = *in.AuthToken
	}
	row, err := w.q.InsertWebhookSubscription(ctx, queries.InsertWebhookSubscriptionParams{
		SubscriptionID: uuid.Must(uuid.NewV7()),
		Name:           in.Name,
		URL:            in.URL,
		Filter:         filter,
		AuthToken:      token,
		IsEnabled:      in.IsEnabled,
	})
	if err != nil {
		return WebhookSubscription{}, err
	}
	return webhookSubscriptionFrom(row)
}

func (w *WebhookStore) Update(ctx context.Context, id uuid.UUID, in WebhookSubscriptionWrite) (WebhookSubscription, error) {
	filter, err := json.Marshal(in.Filter)
	if err != nil {
		return WebhookSubscription{}, fmt.Errorf("encode filter: %w", err)
	}
	row, err := w.q.UpdateWebhookSubscription(ctx, queries.UpdateWebhookSubscriptionParams{
		SubscriptionID: id,
		Name:           in.Name,
		URL:            in.URL,
		Filter:         filter,
		IsEnabled:      in.IsEnabled,
		AuthToken:      in.AuthToken,
	})
	if err != nil {
		return WebhookSubscription{}, err
	}
	return webhookSubscriptionFrom(row)
}

func (w *WebhookStore) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := w.q.DeleteWebhookSubscription(ctx, id)
	return n > 0, err
}

func (w *WebhookStore) Get(ctx context.Context, id uuid.UUID) (WebhookSubscription, error) {
	row, err := w.q.GetWebhookSubscription(ctx, id)
	if err != nil {
		return WebhookSubscription{}, err
	}
	return webhookSubscriptionFrom(row)
}

func (w *WebhookStore) List(ctx context.Context) ([]WebhookSubscription, error) {
	rows, err := w.q.ListWebhookSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WebhookSubscription, 0, len(rows))
	for _, row := range rows {
		sub, err := webhookSubscriptionFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

// Deliveries lists a subscription's recent attempts, newest first.
func (w *WebhookStore) Deliveries(ctx context.Context, id uuid.UUID, limit int) ([]WebhookDelivery, error) {
	rows, err := w.q.ListWebhookDeliveries(ctx, queries.ListWebhookDeliveriesParams{
		SubscriptionID: id,
		Limit:          int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]WebhookDelivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookDeliveryFrom(row))
	}
	return out, nil
}

// Endpoint answers where a delivery goes and what credential it carries. Read
// at delivery time rather than frozen with the payload, because an operator
// correcting a mistyped URL must not have to wait out the retries of every
// delivery already queued against the old one.
func (w *WebhookStore) Endpoint(ctx context.Context, id uuid.UUID) (url, token string, err error) {
	row, err := w.q.WebhookSubscriptionURLAndToken(ctx, id)
	if err != nil {
		return "", "", err
	}
	return row.URL, row.AuthToken, nil
}

// ClaimDue takes up to n deliveries that are due, marking them attempted so no
// other claim can take them.
func (w *WebhookStore) ClaimDue(ctx context.Context, n int) ([]WebhookDelivery, error) {
	rows, err := w.q.ClaimDueWebhookDeliveries(ctx, int32(n))
	if err != nil {
		return nil, err
	}
	out := make([]WebhookDelivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookDeliveryFrom(row))
	}
	return out, nil
}

func (w *WebhookStore) MarkDelivered(ctx context.Context, id uuid.UUID, statusCode int) error {
	code := int32(statusCode)
	return w.q.MarkWebhookDelivered(ctx, queries.MarkWebhookDeliveredParams{
		DeliveryID: id, LastStatusCode: &code,
	})
}

func (w *WebhookStore) Reschedule(ctx context.Context, id uuid.UUID, at time.Time, statusCode int, reason string) error {
	var code *int32
	if statusCode > 0 {
		c := int32(statusCode)
		code = &c
	}
	return w.q.RescheduleWebhookDelivery(ctx, queries.RescheduleWebhookDeliveryParams{
		DeliveryID: id, NextAttemptAt: stamp(at), LastStatusCode: code, LastError: reason,
	})
}

func (w *WebhookStore) MarkFailed(ctx context.Context, id uuid.UUID, statusCode int, reason string) error {
	var code *int32
	if statusCode > 0 {
		c := int32(statusCode)
		code = &c
	}
	return w.q.MarkWebhookFailed(ctx, queries.MarkWebhookFailedParams{
		DeliveryID: id, LastStatusCode: code, LastError: reason,
	})
}

// Sweep removes settled deliveries past their own window. PENDING is never
// swept: a delivery still owed is work, not history.
func (w *WebhookStore) Sweep(ctx context.Context, deliveredBefore, failedBefore time.Time) (int64, error) {
	return w.q.SweepWebhookDeliveries(ctx, queries.SweepWebhookDeliveriesParams{
		CreatedAt:   stamp(deliveredBefore),
		CreatedAt_2: stamp(failedBefore),
	})
}

// enqueueFor queues this call for every enabled subscription that wants it,
// inside the caller's transaction.
//
// Three outcomes per subscription, and only the third is a second delivery to
// the customer (design 09 §2.1): a delivery still PENDING has its payload
// replaced, so they only ever hear the corrected story; anything else — no row
// yet, or one already DELIVERED or FAILED — enqueues the next revision.
func enqueueFor(ctx context.Context, q *queries.Queries, cdr CDR) error {
	subs, err := q.EnabledWebhookSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("read subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	payload, err := json.Marshal(cdr)
	if err != nil {
		return fmt.Errorf("encode cdr: %w", err)
	}

	for _, sub := range subs {
		var filter WebhookFilter
		if len(sub.Filter) > 0 {
			if err := json.Unmarshal(sub.Filter, &filter); err != nil {
				// A filter that cannot be read is not a reason to lose the
				// call: it is refused at write time, so this can only be a
				// row edited outside the application.
				return fmt.Errorf("decode filter of %s: %w", sub.SubscriptionID, err)
			}
		}
		if !filter.Matches(cdr) {
			continue
		}
		replaced, err := q.ReplacePendingWebhookDelivery(ctx, queries.ReplacePendingWebhookDeliveryParams{
			SubscriptionID: sub.SubscriptionID,
			CallID:         cdr.CallID,
			Payload:        payload,
		})
		if err == nil {
			_ = replaced
			continue
		}
		if err != pgx.ErrNoRows {
			return fmt.Errorf("replace pending delivery: %w", err)
		}
		if _, err := q.EnqueueWebhookDelivery(ctx, queries.EnqueueWebhookDeliveryParams{
			DeliveryID:     uuid.Must(uuid.NewV7()),
			SubscriptionID: sub.SubscriptionID,
			CallID:         cdr.CallID,
			Payload:        payload,
		}); err != nil && err != pgx.ErrNoRows {
			// ErrNoRows is ON CONFLICT DO NOTHING: another writer got there
			// first with this revision, which is the race the constraint
			// exists to settle. Nothing is lost and nothing is duplicated.
			return fmt.Errorf("enqueue delivery: %w", err)
		}
	}
	return nil
}

func webhookSubscriptionFrom(row queries.WebhookSubscription) (WebhookSubscription, error) {
	var filter WebhookFilter
	if len(row.Filter) > 0 {
		if err := json.Unmarshal(row.Filter, &filter); err != nil {
			return WebhookSubscription{}, fmt.Errorf("decode filter: %w", err)
		}
	}
	return WebhookSubscription{
		SubscriptionID: row.SubscriptionID,
		Name:           row.Name,
		URL:            row.URL,
		Filter:         filter,
		AuthToken:      row.AuthToken,
		IsEnabled:      row.IsEnabled,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}, nil
}

func webhookDeliveryFrom(row queries.WebhookDelivery) WebhookDelivery {
	d := WebhookDelivery{
		DeliveryID:     row.DeliveryID,
		SubscriptionID: row.SubscriptionID,
		CallID:         row.CallID,
		Revision:       int(row.Revision),
		Payload:        row.Payload,
		Status:         row.Status,
		AttemptCount:   int(row.AttemptCount),
		NextAttemptAt:  row.NextAttemptAt.Time,
		LastError:      row.LastError,
		CreatedAt:      row.CreatedAt.Time,
		DeliveredAt:    row.DeliveredAt.Time,
	}
	if row.LastStatusCode != nil {
		code := int(*row.LastStatusCode)
		d.LastStatusCode = &code
	}
	return d
}

// txFor runs fn inside one transaction, rolling back on any error.
func txFor(ctx context.Context, pool *pgxpool.Pool, fn func(*queries.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(queries.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
