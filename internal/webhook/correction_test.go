// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// drainUntil runs the worker's pass until done is closed, the way Run's ticker
// would. One pass is not always enough: under make test-stress a pass came back
// without claiming a delivery that was already committed (its row was left at
// attempt_count 0), and the test then waited forever for a request that was
// never sent. The worker's answer to an empty pass is the next tick, so the
// test's is the next pass.
func drainUntil(ctx context.Context, w *Worker, done <-chan struct{}) error {
	deadline := time.After(30 * time.Second)
	for {
		w.drain(ctx)
		select {
		case <-done:
			return nil
		default:
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("no request within 30s of passes")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// received is one request as the customer's endpoint saw it.
type received struct {
	webhookID string
	revision  int
	talkSec   int
}

// A fuller CDR written while the earlier delivery is on the wire must still
// reach the customer (#445). The receiver holds the first request open until
// the test has written the fuller CDR, then answers 204; the worker must not
// let that 2xx settle the correction.
func TestACorrectionWrittenWhileTheFirstDeliveryIsInFlightIsStillSent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "webhookfix"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	hooks := st.Webhooks()

	var (
		mu   sync.Mutex
		seen []received
	)
	firstArrived := make(chan struct{})
	secondArrived := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b struct {
			Revision int `json:"revision"`
			CDR      struct {
				TalkSec int `json:"talkSec"`
			} `json:"cdr"`
		}
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		seen = append(seen, received{r.Header.Get("X-AICC-Webhook-Id"), b.Revision, b.CDR.TalkSec})
		n := len(seen)
		mu.Unlock()
		switch n {
		case 1:
			close(firstArrived)
			select {
			case <-release:
			case <-r.Context().Done():
				// The test gave up; let the server close.
				return
			}
		case 2:
			close(secondArrived)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	sub, err := hooks.Create(ctx, store.WebhookSubscriptionWrite{Name: "crm", URL: srv.URL, IsEnabled: true})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	callID := uuid.Must(uuid.NewV7())
	ended := time.Now().UTC().Add(-4 * time.Minute)
	cdr := func(endedAt time.Time, talkSec int) store.CDR {
		return store.CDR{
			CallID: callID, StartedAt: endedAt.Add(-time.Duration(talkSec) * time.Second),
			EndedAt: endedAt, CallType: "OUTBOUND", Status: "ANSWERED", DID: "95001", TalkSec: talkSec,
		}
	}
	if err := st.Ledger().InsertCDR(ctx, cdr(ended, 30)); err != nil {
		t.Fatalf("short write: %v", err)
	}

	w := newWorkerLogging(t, hooks, nil, slog.New(slog.NewTextHandler(t.Output(), nil)))
	// The receiver holds the first request open for as long as the test takes
	// to write the fuller CDR, which under load can outlast RequestTimeout.
	w.client.Timeout = time.Minute
	// An attempt that fails before reaching the receiver is due again at once
	// rather than a minute later, so a transport hiccup costs a pass, not the
	// test.
	w.now = func() time.Time { return time.Now().UTC().Add(-time.Hour) }

	runCtx, stop := context.WithCancel(ctx)
	var passes sync.WaitGroup
	defer passes.Wait()
	defer stop()
	firstPass := make(chan error, 1)
	passes.Go(func() { firstPass <- drainUntil(runCtx, w, firstArrived) })

	select {
	case <-firstArrived:
	case err := <-firstPass:
		t.Fatalf("the first delivery never reached the receiver: %v", err)
	}
	if err := st.Ledger().InsertCDR(ctx, cdr(ended.Add(4*time.Minute), 270)); err != nil {
		t.Fatalf("fuller write: %v", err)
	}
	close(release)
	if err := <-firstPass; err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// The correction is a delivery of its own, due now.
	if err := drainUntil(runCtx, w, secondArrived); err != nil {
		t.Fatalf("the correction never reached the receiver: %v", err)
	}

	mu.Lock()
	got := append([]received(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("receiver saw %d requests, want the original and the correction: %+v", len(got), got)
	}
	if got[0].revision != 1 || got[0].talkSec != 30 {
		t.Errorf("first request = %+v, want revision 1 with the short story", got[0])
	}
	if got[1].revision != 2 || got[1].talkSec != 270 {
		t.Errorf("second request = %+v, want revision 2 with the fuller story", got[1])
	}
	if got[0].webhookID == got[1].webhookID {
		t.Errorf("both requests carry X-AICC-Webhook-Id %s; one deliveryId must carry one body", got[0].webhookID)
	}

	rows, err := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(rows))
	}
	newest, oldest := rows[0], rows[1]
	if newest.Revision != 2 || newest.Status != store.DeliveryDelivered {
		t.Errorf("newest = %+v, want revision 2 delivered", newest)
	}
	if oldest.Revision != 1 || oldest.Status != store.DeliveryDelivered {
		t.Errorf("oldest = %+v, want revision 1 delivered", oldest)
	}
	var np, op store.CDR
	_ = json.Unmarshal(newest.Payload, &np)
	_ = json.Unmarshal(oldest.Payload, &op)
	if np.TalkSec != 270 || op.TalkSec != 30 {
		t.Errorf("stored payloads talkSec = %d (rev 2), %d (rev 1), want 270 and 30", np.TalkSec, op.TalkSec)
	}
}
