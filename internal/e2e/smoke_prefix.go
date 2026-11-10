// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The called-number prefix (issue #531). The e2e gateway dials every call with
// a destination-prefix; the prefix must reach the SIP wire and nothing above
// it. The functions below judge what the smoke observed and take plain values,
// so a leaking input can be fed to them without a network.

// PrefixFacts is what the smoke saw about the prefix, written to smoke.json.
type PrefixFacts struct {
	Prefix string `json:"prefix"`
	// RequestURIUser and ToUser are the simulator's record of the INVITE.
	RequestURIUser string `json:"requestUriUser"`
	ToUser         string `json:"toUser"`
	// WebhookDeliveries counts the deliveries received for the call and
	// WebhookToNumber is the toNumber they carried (the first one's).
	WebhookDeliveries int    `json:"webhookDeliveries"`
	WebhookToNumber   string `json:"webhookToNumber"`
	// Events counts every event the administrator's stream carried while the
	// call ran; the next two count those of the call.
	Events      int `json:"events"`
	CallEvents  int `json:"callEvents"`
	PartyEvents int `json:"partyEvents"`
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// judgeWire checks that the INVITE carried the prefix in front of the dialled
// number, in the Request-URI and in the To header.
func judgeWire(d calleesim.DialogRecord, prefix, to string, f *PrefixFacts, col *collector) {
	f.RequestURIUser = sipUser(d.RequestURI)
	f.ToUser = sipUser(d.InviteHeaders["to"])
	col.that(f.RequestURIUser == prefix+to, "the Request-URI user on the wire is the prefix plus the dialled number (%q, want %q)", f.RequestURIUser, prefix+to)
	col.that(f.ToUser == prefix+to, "the To user on the wire is the prefix plus the dialled number (%q, want %q)", f.ToUser, prefix+to)
	col.that(d.Number == to, "the simulator keyed the dialog on the dialled number, prefix removed (%q)", d.Number)
}

// judgeWebhooks checks the bodies received for one call: at least one, each
// naming the call, each with the dialled number as toNumber, none containing
// the prefix anywhere.
func judgeWebhooks(bodies [][]byte, callID uuid.UUID, prefix, to string, f *PrefixFacts, col *collector) {
	n := 0
	leaked, wrongTo := 0, 0
	for _, body := range bodies {
		var d struct {
			CDR struct {
				CallID   uuid.UUID `json:"callId"`
				ToNumber string    `json:"toNumber"`
			} `json:"cdr"`
		}
		if json.Unmarshal(body, &d) != nil || d.CDR.CallID != callID {
			continue
		}
		n++
		if n == 1 {
			f.WebhookToNumber = d.CDR.ToNumber
		}
		if d.CDR.ToNumber != to {
			wrongTo++
		}
		if containsFold(string(body), prefix) {
			leaked++
		}
	}
	f.WebhookDeliveries = n
	col.that(n > 0, "the webhook delivered the call (%d deliveries)", n)
	col.that(n > 0 && wrongTo == 0, "every webhook delivery's toNumber is the dialled number (%d of %d differ, first %q)", wrongTo, n, f.WebhookToNumber)
	col.that(leaked == 0, "no webhook delivery contains the prefix (%d of %d do)", leaked, n)
}

// judgeEvents checks the raw data of every event the administrator's stream
// carried: none contains the prefix, and the stream was not empty of the
// call's own events, so the check is not vacuous.
func judgeEvents(raw [][]byte, callID uuid.UUID, prefix string, f *PrefixFacts, col *collector) {
	leaked := 0
	var firstLeak string
	for _, data := range raw {
		var ev struct {
			Type   string    `json:"type"`
			CallID uuid.UUID `json:"callId"`
		}
		if containsFold(string(data), prefix) {
			leaked++
			if firstLeak == "" {
				firstLeak = string(data)
			}
		}
		if json.Unmarshal(data, &ev) != nil || ev.CallID != callID {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Type, "PARTY_"):
			f.PartyEvents++
		case strings.HasPrefix(ev.Type, "CALL_"):
			f.CallEvents++
		}
	}
	f.Events = len(raw)
	col.that(f.PartyEvents > 0 && f.CallEvents > 0, "the stream carried the call's own events (%d PARTY_*, %d CALL_*), so the prefix check is not vacuous", f.PartyEvents, f.CallEvents)
	col.that(leaked == 0, "no event contains the prefix (%d of %d do%s)", leaked, len(raw), firstSuffix(firstLeak))
}

func firstSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// webhookReceiver is an HTTP endpoint inside the network that keeps every body
// POSTed to it.
type webhookReceiver struct {
	srv *http.Server
	url string

	mu     sync.Mutex
	bodies [][]byte
}

// newWebhookReceiver listens on every address and names itself by the local
// address the route to apiBase uses, which is the one the application reaches.
func newWebhookReceiver(apiBase string) (*webhookReceiver, error) {
	u, err := url.Parse(apiBase)
	if err != nil {
		return nil, err
	}
	probe, err := net.Dial("udp", net.JoinHostPort(u.Hostname(), "9"))
	if err != nil {
		return nil, fmt.Errorf("find the driver's own address: %w", err)
	}
	host := probe.LocalAddr().(*net.UDPAddr).IP.String()
	probe.Close()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	r := &webhookReceiver{url: "http://" + net.JoinHostPort(host, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)) + "/"}
	r.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

func (r *webhookReceiver) received() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...)
}

func (r *webhookReceiver) close() { _ = r.srv.Close() }

// deliveriesFor counts the received bodies naming the call.
func deliveriesFor(bodies [][]byte, callID uuid.UUID) int {
	n := 0
	for _, b := range bodies {
		var d struct {
			CDR struct {
				CallID uuid.UUID `json:"callId"`
			} `json:"cdr"`
		}
		if json.Unmarshal(b, &d) == nil && d.CDR.CallID == callID {
			n++
		}
	}
	return n
}

// waitDeliveries waits for the first delivery of the call, then a short grace
// for a second one a wrongly written revision would send.
func (r *webhookReceiver) waitDeliveries(ctx context.Context, callID uuid.UUID, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for deliveriesFor(r.received(), callID) == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("no webhook delivery for the call within %s", wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return sleep(ctx, 6*time.Second) // one tick of the delivery worker and a little more
}

// createSubscription points a subscription for the DID's calls at the
// receiver, through the public API.
func (c *httpc) createSubscription(ctx context.Context, name, receiverURL, did string) (uuid.UUID, error) {
	enabled := true
	var sub api.WebhookSubscription
	err := c.do(ctx, http.MethodPost, "/webhook-subscriptions", api.WebhookSubscriptionWrite{
		Name: name, URL: receiverURL, IsEnabled: &enabled, Filter: &api.WebhookFilter{DID: &[]string{did}},
	}, &sub, http.StatusCreated)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create webhook subscription: %w", err)
	}
	return sub.SubscriptionID, nil
}

// eventTap holds the administrator's event stream open and keeps the data of
// every event.
type eventTap struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	data [][]byte
	err  error
}

// openEventTap signs in as the administrator with a session of its own (the
// stream a supervisor sees carries everything) and returns once the stream is
// open, so an event published after it returns is not missed.
func openEventTap(ctx context.Context, base, username, password string) (*eventTap, error) {
	login := &httpc{base: base, client: newClient()}
	var user api.CurrentUser
	if err := login.do(ctx, http.MethodPost, "/auth/login", api.LoginRequest{Username: username, Password: password}, &user, http.StatusOK); err != nil {
		return nil, fmt.Errorf("login for the event stream: %w", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, base+"/api/v1/events", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	stream := &http.Client{Jar: login.client.Jar} // no timeout: the body is read for the whole call
	resp, err := stream.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open the event stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("open the event stream: %s", resp.Status)
	}
	t := &eventTap{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:")); ok {
				t.mu.Lock()
				t.data = append(t.data, bytes.TrimSpace(append([]byte(nil), data...)))
				t.mu.Unlock()
			}
		}
		// Only stop ends the stream. The server closing it first (a slow
		// consumer cut off, a restart) means later events were not seen.
		if sctx.Err() == nil {
			err := sc.Err()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			t.mu.Lock()
			t.err = err
			t.mu.Unlock()
		}
	}()
	return t, nil
}

// stop closes the stream and returns what it carried.
func (t *eventTap) stop() ([][]byte, error) {
	t.cancel()
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.data, t.err
}
