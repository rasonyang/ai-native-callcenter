// SPDX-License-Identifier: Apache-2.0

// Package webhook delivers finished calls to a customer's own system.
//
// Everything slow lives here. The enqueue is part of the ledger write and
// makes no network call (design 09 §2); this package drains what that queued,
// which is where the timeouts, the retries and somebody else's outage are.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Backoff is the schedule a delivery is retried on, and its length is the
// attempt cap: six tries over roughly nine hours, then the delivery is failed
// (design 09 §6).
//
// A 2xx is success; everything else, timeouts included, is retried the same
// way. Telling "your payload is malformed" from "our gateway hiccuped" by
// status code is guesswork, and guessing wrong in the permanent direction
// silently drops a call the customer wanted.
var Backoff = []time.Duration{
	10 * time.Second,
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
}

// RequestTimeout bounds one attempt. Short, because a customer's endpoint that
// takes longer than this under load is better retried than waited on.
const RequestTimeout = 10 * time.Second

// PerSubscriptionConcurrency is how many deliveries this deployment will have
// in flight against one subscription at a time (design 09 §6).
//
// A backlog is ours, not the customer's: a queue that built up here must not
// arrive at one endpoint as a burst it has to survive. The cap is per
// subscription rather than global so a slow or wedged receiver holds up only
// its own deliveries — other subscriptions keep draining at full speed behind
// it.
const PerSubscriptionConcurrency = 4

// Store is the slice of the outbox this package uses.
type Store interface {
	ClaimDue(ctx context.Context, n int) ([]store.WebhookDelivery, error)
	Endpoint(ctx context.Context, subscriptionID uuid.UUID) (url, token string, err error)
	MarkDelivered(ctx context.Context, deliveryID uuid.UUID, statusCode int) error
	Reschedule(ctx context.Context, deliveryID uuid.UUID, at time.Time, statusCode int, reason string) error
	MarkFailed(ctx context.Context, deliveryID uuid.UUID, statusCode int, reason string) error
}

// Metrics counts what a dashboard watches. Nil disables counting; the log line
// is emitted either way, because a metric is what somebody notices in a week
// and a log line is what they grep at 2am with the customer on the phone.
type Metrics interface {
	DeliverySucceeded(subscriptionID uuid.UUID)
	DeliveryFailed(subscriptionID uuid.UUID)
}

// body is what a customer's endpoint receives.
//
// `cdr`, not `event`: the two are different things, and naming the field for
// what it carries leaves the name free if a generic event webhook is ever
// added. `revision` is in the body rather than only in a header so a payload
// logged on its own still says which version of the call it is.
type body struct {
	DeliveryID     uuid.UUID       `json:"deliveryId"`
	SubscriptionID uuid.UUID       `json:"subscriptionId"`
	Revision       int             `json:"revision"`
	Attempt        int             `json:"attempt"`
	CDR            json.RawMessage `json:"cdr"`
}

// Worker drains the outbox.
type Worker struct {
	store   Store
	client  *http.Client
	metrics Metrics
	log     *slog.Logger
	// batch bounds one pass so a backlog is worked steadily rather than in one
	// burst at a customer who has just come back up.
	batch int
	tick  time.Duration
	// now is injected so tests do not sleep out a backoff.
	now func() time.Time
}

// New builds the worker.
func New(st Store, metrics Metrics, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store:   st,
		client:  &http.Client{Timeout: RequestTimeout},
		metrics: metrics,
		log:     log,
		batch:   20,
		tick:    5 * time.Second,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Run drains the outbox until the context ends.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.drain(ctx)
		}
	}
}

// drain takes one batch and delivers it.
//
// Deliveries run concurrently and that is a dividend of the CDR-only scope: a
// CDR is a complete, independent record of a finished call, so one arriving
// before another rearranges nothing. The generic-event design this replaced
// needed strict per-subscription ordering, because PARTY_ESTABLISHED after
// PARTY_RELEASED cannot be read.
//
// Concurrency is global across the batch but capped per subscription: every
// goroutine takes a slot from its subscription's semaphore before it sends,
// so a batch that happens to be all one customer's still arrives
// PerSubscriptionConcurrency at a time while other subscriptions are not held
// up behind it. Waiting for a slot costs nothing against the lease — a full
// batch of 20 for one subscription, every attempt burning the whole
// RequestTimeout, drains in about 50s against an hour-long lease — and drain
// blocks the Run loop, so ticks never overlap and a second batch cannot be
// claimed while this one is still queued behind the cap.
func (w *Worker) drain(ctx context.Context) {
	due, err := w.store.ClaimDue(ctx, w.batch)
	if err != nil {
		w.log.ErrorContext(ctx, "cannot read the webhook outbox", "error", err)
		return
	}
	slots := make(map[uuid.UUID]chan struct{}, len(due))
	for _, d := range due {
		if _, ok := slots[d.SubscriptionID]; !ok {
			slots[d.SubscriptionID] = make(chan struct{}, PerSubscriptionConcurrency)
		}
	}
	done := make(chan struct{}, len(due))
	for _, d := range due {
		slot := slots[d.SubscriptionID]
		go func(d store.WebhookDelivery) {
			defer func() { done <- struct{}{} }()
			select {
			case slot <- struct{}{}:
			case <-ctx.Done():
				// Shutdown does not wait for a slot that may never come.
				return
			}
			defer func() { <-slot }()
			w.deliver(ctx, d)
		}(d)
	}
	for range due {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

// deliver makes one attempt and records what came back.
func (w *Worker) deliver(ctx context.Context, d store.WebhookDelivery) {
	url, token, err := w.store.Endpoint(ctx, d.SubscriptionID)
	if err != nil {
		w.settle(ctx, d, 0, fmt.Sprintf("cannot read the subscription: %v", err))
		return
	}

	payload, err := json.Marshal(body{
		DeliveryID: d.DeliveryID, SubscriptionID: d.SubscriptionID,
		Revision: d.Revision, Attempt: d.AttemptCount, CDR: d.Payload,
	})
	if err != nil {
		// Nothing a retry can mend, but the attempt cap ends it rather than a
		// special case here: one way out, not two.
		w.settle(ctx, d, 0, fmt.Sprintf("cannot encode the delivery: %v", err))
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		w.settle(ctx, d, 0, fmt.Sprintf("cannot build the request: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AICC-Webhook-Id", d.DeliveryID.String())
	if token != "" {
		// Their credential, not ours. This deployment's own API keys point
		// the other way and
		// must never travel outward (design 09 §8).
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.settle(ctx, d, 0, err.Error())
		return
	}
	// The body is read and discarded so the connection can be reused; a
	// customer's error text is not ours to store.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := w.store.MarkDelivered(ctx, d.DeliveryID, resp.StatusCode); err != nil {
			w.log.ErrorContext(ctx, "delivered but could not be marked", "error", err,
				"deliveryId", d.DeliveryID)
			return
		}
		if w.metrics != nil {
			w.metrics.DeliverySucceeded(d.SubscriptionID)
		}
		return
	}
	w.settle(ctx, d, resp.StatusCode, fmt.Sprintf("HTTP %d", resp.StatusCode))
}

// settle reschedules a failed attempt or gives up on it.
//
// AttemptCount is the number of attempts *including* this one, because the
// claim incremented it — so it indexes the wait before the next try, and
// running off the end of the schedule is the cap.
func (w *Worker) settle(ctx context.Context, d store.WebhookDelivery, statusCode int, reason string) {
	if d.AttemptCount >= len(Backoff) {
		if err := w.store.MarkFailed(ctx, d.DeliveryID, statusCode, reason); err != nil {
			w.log.ErrorContext(ctx, "failed delivery could not be marked", "error", err,
				"deliveryId", d.DeliveryID)
		}
		if w.metrics != nil {
			w.metrics.DeliveryFailed(d.SubscriptionID)
		}
		// Both a metric and a log line, by owner directive: the metric is what
		// a dashboard watches, and this is what an operator finds when a
		// customer asks why one call never arrived.
		w.log.WarnContext(ctx, "giving up on a webhook delivery",
			"deliveryId", d.DeliveryID, "subscriptionId", d.SubscriptionID,
			"callId", d.CallID, "revision", d.Revision,
			"attempts", d.AttemptCount, "lastStatus", statusCode, "reason", reason)
		return
	}
	next := w.now().Add(Backoff[d.AttemptCount])
	if err := w.store.Reschedule(ctx, d.DeliveryID, next, statusCode, reason); err != nil {
		w.log.ErrorContext(ctx, "could not reschedule a delivery", "error", err,
			"deliveryId", d.DeliveryID)
	}
}
