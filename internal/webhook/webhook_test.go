// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeStore is the outbox, in memory, recording how each delivery settled.
type fakeStore struct {
	mu    sync.Mutex
	due   []store.WebhookDelivery
	url   string
	token string
	// urls overrides url per subscription, for the tests that need two
	// customers with two endpoints.
	urls        map[uuid.UUID]string
	delivered   map[uuid.UUID]int
	failed      map[uuid.UUID]string
	rescheduled map[uuid.UUID]time.Time
	// deliveredCh announces each settled delivery, so a test can wait on
	// progress instead of sleeping and hoping.
	deliveredCh chan uuid.UUID
}

func newFakeStore(url, token string, due ...store.WebhookDelivery) *fakeStore {
	return &fakeStore{
		due: due, url: url, token: token,
		urls:        map[uuid.UUID]string{},
		delivered:   map[uuid.UUID]int{},
		failed:      map[uuid.UUID]string{},
		rescheduled: map[uuid.UUID]time.Time{},
		deliveredCh: make(chan uuid.UUID, 256),
	}
}

func (f *fakeStore) ClaimDue(context.Context, int) ([]store.WebhookDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.due
	f.due = nil
	return out, nil
}

func (f *fakeStore) Endpoint(_ context.Context, subscriptionID uuid.UUID) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if url, ok := f.urls[subscriptionID]; ok {
		return url, f.token, nil
	}
	return f.url, f.token, nil
}

func (f *fakeStore) MarkDelivered(_ context.Context, id uuid.UUID, code int) error {
	f.mu.Lock()
	f.delivered[id] = code
	f.mu.Unlock()
	select {
	case f.deliveredCh <- id:
	default:
	}
	return nil
}

// deliveredCount is how many deliveries have been marked so far.
func (f *fakeStore) deliveredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.delivered)
}

func (f *fakeStore) Reschedule(_ context.Context, id uuid.UUID, at time.Time, _ int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled[id] = at
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id uuid.UUID, _ int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = reason
	return nil
}

func pending(attempt int) store.WebhookDelivery {
	return pendingFor(uuid.New(), attempt)
}

// pendingFor is pending for a named subscription, for the tests about what one
// subscription is subjected to.
func pendingFor(subscriptionID uuid.UUID, attempt int) store.WebhookDelivery {
	return store.WebhookDelivery{
		DeliveryID:     uuid.New(),
		SubscriptionID: subscriptionID,
		CallID:         uuid.New(),
		Revision:       1,
		Payload:        json.RawMessage(`{"callId":"c-1","talkSec":40}`),
		Status:         store.DeliveryPending,
		AttemptCount:   attempt,
	}
}

// What the customer's endpoint actually receives. The wrapper and the headers
// are the contract with somebody else's code, so they are asserted rather than
// assumed.
func TestWhatTheCustomersEndpointReceives(t *testing.T) {
	var (
		gotAuth string
		gotID   string
		gotType string
		gotBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotID = r.Header.Get("X-AICC-Webhook-Id")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := pending(1)
	st := newFakeStore(srv.URL, "their-token", d)
	New(st, nil, discard()).drain(t.Context())

	if code, ok := st.delivered[d.DeliveryID]; !ok || code != http.StatusOK {
		t.Fatalf("delivered = %d, ok = %v, want 200", code, ok)
	}
	// Their credential, not ours. This deployment's own API keys point the
	// other way and must
	// never travel outward.
	if gotAuth != "Bearer their-token" {
		t.Errorf("Authorization = %q, want the subscription's own token", gotAuth)
	}
	if gotID != d.DeliveryID.String() {
		t.Errorf("X-AICC-Webhook-Id = %q, want the delivery id — it is the receiver's "+
			"deduplication key", gotID)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q", gotType)
	}

	var body struct {
		DeliveryID uuid.UUID       `json:"deliveryId"`
		Revision   int             `json:"revision"`
		Attempt    int             `json:"attempt"`
		CDR        json.RawMessage `json:"cdr"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body: %v (%s)", err, gotBody)
	}
	if body.DeliveryID != d.DeliveryID || body.Revision != 1 {
		t.Errorf("body = %+v, want the delivery's own id and revision", body)
	}
	// The CDR is passed through as stored rather than re-encoded, so what the
	// customer receives is what the ledger holds.
	if string(body.CDR) != `{"callId":"c-1","talkSec":40}` {
		t.Errorf("cdr = %s, want the stored row verbatim", body.CDR)
	}
}

// A subscription with no token presents none, rather than sending an empty
// bearer that a receiver would have to special-case.
func TestNoTokenMeansNoAuthorizationHeader(t *testing.T) {
	var had bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, had = r.Header["Authorization"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := pending(1)
	New(newFakeStore(srv.URL, "", d), nil, discard()).drain(t.Context())
	if had {
		t.Error("an Authorization header was sent for a subscription with no token")
	}
}

// Anything but 2xx is retried on the schedule, and the wait is indexed by the
// attempt already made — so a first failure waits the first interval, not the
// second.
func TestAFailedAttemptIsRescheduledOnTheSchedule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := pending(1)
	st := newFakeStore(srv.URL, "t", d)
	w := New(st, nil, discard())
	base := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return base }
	w.drain(t.Context())

	at, ok := st.rescheduled[d.DeliveryID]
	if !ok {
		t.Fatal("a 500 was not retried at all")
	}
	if want := base.Add(Backoff[1]); !at.Equal(want) {
		t.Errorf("next attempt at %v, want %v — attempt %d indexes the wait before the next try",
			at, want, d.AttemptCount)
	}
	if _, failed := st.failed[d.DeliveryID]; failed {
		t.Error("one failure gave up on the delivery")
	}
}

// The schedule's length is the cap. Running off the end is what ends a
// delivery, and it is announced: a metric for the dashboard, a WARN for the
// operator with the customer on the phone.
func TestRunningOutOfAttemptsFailsTheDeliveryAndSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	d := pending(len(Backoff)) // the last attempt the schedule allows
	st := newFakeStore(srv.URL, "t", d)
	counted := &countingMetrics{}
	New(st, counted, discard()).drain(t.Context())

	if _, ok := st.failed[d.DeliveryID]; !ok {
		t.Fatalf("the delivery was not failed after %d attempts", d.AttemptCount)
	}
	if _, ok := st.rescheduled[d.DeliveryID]; ok {
		t.Error("it was rescheduled past the end of the schedule")
	}
	if counted.failed != 1 {
		t.Errorf("failures counted = %d, want 1 — a dashboard is how anybody notices",
			counted.failed)
	}
}

// An endpoint that cannot be reached at all is a failure like any other. A
// transport error carries no status code, and the delivery must still settle
// rather than being claimed and forgotten.
func TestAnUnreachableEndpointIsRetriedLikeAnyOtherFailure(t *testing.T) {
	d := pending(1)
	// A port nothing is listening on.
	st := newFakeStore("http://127.0.0.1:1/hook", "t", d)
	w := New(st, nil, discard())
	w.drain(t.Context())

	if _, ok := st.rescheduled[d.DeliveryID]; !ok {
		t.Error("a connection refused left the delivery neither retried nor failed")
	}
}

// A 2xx that is not 200 is still success. A receiver answering 204 has taken
// the delivery, and retrying it would post them a duplicate.
func TestEveryTwoHundredIsSuccess(t *testing.T) {
	for _, code := range []int{200, 201, 202, 204} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		d := pending(1)
		st := newFakeStore(srv.URL, "t", d)
		New(st, nil, discard()).drain(t.Context())
		srv.Close()

		if _, ok := st.delivered[d.DeliveryID]; !ok {
			t.Errorf("HTTP %d was treated as a failure", code)
		}
	}
}

// A backlog here must not arrive at one customer as a burst. Ten deliveries
// for one subscription are worked concurrently — the point of the fan-out —
// but never more than PerSubscriptionConcurrency of them are in flight at the
// endpoint at once.
func TestOneSubscriptionIsNeverHitByMoreThanTheCap(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		// Wide enough that overlapping requests really do overlap; without it
		// a fast handler could serialise by luck and prove nothing.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub := uuid.New()
	const count = 10
	due := make([]store.WebhookDelivery, 0, count)
	for range count {
		due = append(due, pendingFor(sub, 1))
	}
	st := newFakeStore(srv.URL, "t", due...)
	New(st, nil, discard()).drain(t.Context())

	mu.Lock()
	got := peak
	mu.Unlock()
	if got > PerSubscriptionConcurrency {
		t.Errorf("peak in flight = %d, want at most %d — a backlog here is not the "+
			"customer's problem to absorb", got, PerSubscriptionConcurrency)
	}
	if got < 2 {
		t.Errorf("peak in flight = %d, want more than one — the cap bounds the fan-out, "+
			"it does not serialise it", got)
	}
	if n := st.deliveredCount(); n != count {
		t.Errorf("delivered = %d, want %d", n, count)
	}
}

// The cap is per subscription, so one wedged receiver holds up only its own
// deliveries. B's endpoint is answered in full while A's is still hanging.
func TestASlowSubscriberDoesNotHoldUpAnotherOne(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()
	quick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer quick.Close()

	// One more than the cap each, so both subscriptions have a delivery that
	// has to wait for a slot.
	const each = PerSubscriptionConcurrency + 1
	subA, subB := uuid.New(), uuid.New()
	var due []store.WebhookDelivery
	quickIDs := map[uuid.UUID]bool{}
	for range each {
		due = append(due, pendingFor(subA, 1))
		d := pendingFor(subB, 1)
		quickIDs[d.DeliveryID] = true
		due = append(due, d)
	}
	st := newFakeStore(slow.URL, "t", due...)
	st.urls[subB] = quick.URL

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		New(st, nil, discard()).drain(t.Context())
	}()

	// Every one of B's deliveries settles while A is still hanging.
	for i := range each {
		select {
		case id := <-st.deliveredCh:
			if !quickIDs[id] {
				t.Errorf("delivery %v settled, but the blocked subscription's deliveries "+
					"cannot have", id)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d deliveries to the responsive subscription got through "+
				"while the other one was blocked", i, each)
		}
	}

	close(release)
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("drain did not finish after the slow subscriber answered")
	}
	if n := st.deliveredCount(); n != 2*each {
		t.Errorf("delivered = %d, want %d", n, 2*each)
	}
}

type countingMetrics struct {
	mu        sync.Mutex
	succeeded int
	failed    int
}

func (c *countingMetrics) DeliverySucceeded(uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.succeeded++
}

func (c *countingMetrics) DeliveryFailed(uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed++
}
