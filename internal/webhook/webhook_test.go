// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newWorker is New with a connection pool of its own. The worker's client
// otherwise uses http.DefaultTransport, which every parallel test in the
// package shares, and httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections: one test closing its server
// breaks a connection another test is about to post on ("transport
// connection broken: http: CloseIdleConnections called"), and a POST on a
// reused connection is not retried. A pool of its own keeps a test's outcome
// a function of its own server only.
func newWorker(t *testing.T, st Store, m Metrics) *Worker {
	t.Helper()
	return newWorkerLogging(t, st, m, discard())
}

// newWorkerLogging is newWorker with a logger, for the tests that assert what
// an operator is told.
func newWorkerLogging(t *testing.T, st Store, m Metrics, log *slog.Logger) *Worker {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	w := New(st, m, log)
	w.client.Transport = tr
	return w
}

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
	// reasons keeps why each delivery was rescheduled or failed, so an
	// assertion that a delivery was not marked delivered can say why.
	reasons map[uuid.UUID]string
	// codes keeps the status code each rescheduled or failed delivery was
	// settled with (last_status_code).
	codes map[uuid.UUID]int
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
		reasons:     map[uuid.UUID]string{},
		codes:       map[uuid.UUID]int{},
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

// whyNot is the recorded reason a delivery was rescheduled or failed instead
// of delivered, for a failure message.
func (f *fakeStore) whyNot(id uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.reasons[id]; ok {
		return r
	}
	return "no settlement was recorded at all"
}

// deliveredCount is how many deliveries have been marked so far.
func (f *fakeStore) deliveredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.delivered)
}

func (f *fakeStore) Reschedule(_ context.Context, id uuid.UUID, at time.Time, code int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled[id] = at
	f.codes[id] = code
	f.reasons[id] = reason
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id uuid.UUID, code int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[id] = code
	f.failed[id] = reason
	f.reasons[id] = reason
	return nil
}

func pending(attempt int) store.WebhookDelivery {
	return pendingFor(uuid.Must(uuid.NewV7()), attempt)
}

// pendingFor is pending for a named subscription, for the tests about what one
// subscription is subjected to.
func pendingFor(subscriptionID uuid.UUID, attempt int) store.WebhookDelivery {
	return store.WebhookDelivery{
		DeliveryID:     uuid.Must(uuid.NewV7()),
		SubscriptionID: subscriptionID,
		CallID:         uuid.Must(uuid.NewV7()),
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
	t.Parallel()
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
	newWorker(t, st, nil).drain(t.Context())

	if code, ok := st.delivered[d.DeliveryID]; !ok || code != http.StatusOK {
		t.Fatalf("delivered = %d, ok = %v, want 200; settled instead as: %s",
			code, ok, st.whyNot(d.DeliveryID))
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
	t.Parallel()
	var had bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, had = r.Header["Authorization"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := pending(1)
	newWorker(t, newFakeStore(srv.URL, "", d), nil).drain(t.Context())
	if had {
		t.Error("an Authorization header was sent for a subscription with no token")
	}
}

// Anything but 2xx is retried on the schedule, and the wait is indexed by the
// attempt already made — so a first failure waits the first interval, not the
// second.
func TestAFailedAttemptIsRescheduledOnTheSchedule(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := pending(1)
	st := newFakeStore(srv.URL, "t", d)
	w := newWorker(t, st, nil)
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
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	d := pending(len(Backoff)) // the last attempt the schedule allows
	st := newFakeStore(srv.URL, "t", d)
	counted := &countingMetrics{}
	newWorker(t, st, counted).drain(t.Context())

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
	t.Parallel()
	d := pending(1)
	// A port nothing is listening on.
	st := newFakeStore("http://127.0.0.1:1/hook", "t", d)
	w := newWorker(t, st, nil)
	w.drain(t.Context())

	if _, ok := st.rescheduled[d.DeliveryID]; !ok {
		t.Error("a connection refused left the delivery neither retried nor failed")
	}
}

// A 2xx that is not 200 is still success. A receiver answering 204 has taken
// the delivery, and retrying it would post them a duplicate.
func TestEveryTwoHundredIsSuccess(t *testing.T) {
	t.Parallel()
	// No socket at all: the worker's client gets a transport that answers
	// with the status under test. Classifying a status is not about
	// networking, and a real server per code on the shared default transport
	// failed in CI when a parallel test's server Close broke the connection
	// mid-request (see newWorker), which read as "HTTP 201 was treated as a
	// failure". The wire path is covered by the tests above.
	for _, code := range []int{200, 201, 202, 204} {
		d := pending(1)
		st := newFakeStore("http://receiver.invalid/hook", "t", d)
		w := New(st, nil, discard())
		w.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: code,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})
		w.drain(t.Context())

		st.mu.Lock()
		got, ok := st.delivered[d.DeliveryID]
		st.mu.Unlock()
		if !ok {
			t.Errorf("HTTP %d (attempt %d) was treated as a failure, want delivered; settled as: %s",
				code, d.AttemptCount, st.whyNot(d.DeliveryID))
		} else if got != code {
			t.Errorf("HTTP %d was recorded as %d", code, got)
		}
	}
}

// roundTripFunc is an http.RoundTripper that never touches the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A backlog here must not arrive at one customer as a burst. Ten deliveries
// for one subscription are worked concurrently — the point of the fan-out —
// but never more than PerSubscriptionConcurrency of them are in flight at the
// endpoint at once.
func TestOneSubscriptionIsNeverHitByMoreThanTheCap(t *testing.T) {
	t.Parallel()
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

	sub := uuid.Must(uuid.NewV7())
	const count = 10
	due := make([]store.WebhookDelivery, 0, count)
	for range count {
		due = append(due, pendingFor(sub, 1))
	}
	st := newFakeStore(srv.URL, "t", due...)
	newWorker(t, st, nil).drain(t.Context())

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
	t.Parallel()
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
	subA, subB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
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
		newWorker(t, st, nil).drain(t.Context())
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

// lockedBuffer is a log sink the worker's goroutines and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// redirectingServer answers /old with the given redirect to /new, and /new
// with 200, counting how often /new was reached.
func redirectingServer(t *testing.T, code int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", code)
		case "/new":
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// A redirect is a failed attempt, not a delivery. Go's client would turn the
// redirected POST into a body-less GET, and the page it lands on answering
// 2xx would mark a delivery the subscriber never received as DELIVERED.
func TestARedirectIsAFailedAttemptAndIsNotFollowed(t *testing.T) {
	t.Parallel()
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv, hits := redirectingServer(t, code)

			// The control: the target accepts a delivery posted straight to it.
			ctl := pending(1)
			ctlStore := newFakeStore(srv.URL+"/new", "t", ctl)
			newWorker(t, ctlStore, nil).drain(t.Context())
			if _, ok := ctlStore.delivered[ctl.DeliveryID]; !ok {
				t.Fatalf("the control delivery to /new was not delivered: %s", ctlStore.whyNot(ctl.DeliveryID))
			}
			hits.Store(0)

			d := pending(1)
			st := newFakeStore(srv.URL+"/old", "t", d)
			w := newWorker(t, st, nil)
			base := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
			w.now = func() time.Time { return base }
			w.drain(t.Context())

			if _, ok := st.delivered[d.DeliveryID]; ok {
				t.Fatalf("a %d was recorded as delivered", code)
			}
			at, ok := st.rescheduled[d.DeliveryID]
			if !ok {
				t.Fatalf("a %d was not rescheduled: %s", code, st.whyNot(d.DeliveryID))
			}
			if want := base.Add(Backoff[1]); !at.Equal(want) {
				t.Errorf("next attempt at %v, want %v", at, want)
			}
			if got := st.codes[d.DeliveryID]; got != code {
				t.Errorf("recorded status code = %d, want %d", got, code)
			}
			if r := st.reasons[d.DeliveryID]; !strings.Contains(r, "redirect") {
				t.Errorf("reason %q does not say it was a redirect", r)
			}
			if _, failed := st.failed[d.DeliveryID]; failed {
				t.Error("one redirect gave up on the delivery")
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("the redirect target was reached %d times, want 0", n)
			}
		})
	}
}

// A redirect on the last attempt ends the delivery like any other failure:
// FAILED with the 3xx, counted, and announced to the operator.
func TestARedirectOnTheLastAttemptFailsTheDeliveryAndSaysSo(t *testing.T) {
	t.Parallel()
	srv, _ := redirectingServer(t, http.StatusMovedPermanently)

	d := pending(len(Backoff))
	st := newFakeStore(srv.URL+"/old", "t", d)
	counted := &countingMetrics{}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	newWorkerLogging(t, st, counted, log).drain(t.Context())

	if _, ok := st.failed[d.DeliveryID]; !ok {
		t.Fatalf("the delivery was not failed: %s", st.whyNot(d.DeliveryID))
	}
	if _, ok := st.rescheduled[d.DeliveryID]; ok {
		t.Error("it was rescheduled past the end of the schedule")
	}
	if got := st.codes[d.DeliveryID]; got != http.StatusMovedPermanently {
		t.Errorf("recorded status code = %d, want 301", got)
	}
	counted.mu.Lock()
	failed := counted.failed
	counted.mu.Unlock()
	if failed != 1 {
		t.Errorf("failures counted = %d, want 1", failed)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "giving up on a webhook delivery") ||
		!strings.Contains(out, "lastStatus=301") {
		t.Errorf("no WARN with lastStatus=301 was logged:\n%s", out)
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
