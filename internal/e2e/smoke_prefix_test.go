// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

const (
	testPrefix = "a1b2cc"
	testTo     = "13800000001"
)

func failures(c collector) string { return strings.Join(c.failures, " | ") }

func wireDialog(requestUserURI, toUser, number string) calleesim.DialogRecord {
	return calleesim.DialogRecord{
		Number:     number,
		RequestURI: "sip:" + requestUserURI + "@10.1.2.11:5080",
		InviteHeaders: map[string]string{
			"to": "<sip:" + toUser + "@10.1.2.13:5080>",
		},
	}
}

func TestTheWireCheckPassesWhenBothUsersCarryThePrefix(t *testing.T) {
	var col collector
	judgeWire(wireDialog(testPrefix+testTo, testPrefix+testTo, testTo), testPrefix, testTo, &PrefixFacts{}, &col)
	if len(col.failures) != 0 {
		t.Fatal(failures(col))
	}
}

func TestTheWireCheckFailsWhenAUserLostThePrefix(t *testing.T) {
	cases := map[string]calleesim.DialogRecord{
		"request uri": wireDialog(testTo, testPrefix+testTo, testTo),
		"to":          wireDialog(testPrefix+testTo, testTo, testTo),
		"upper case":  wireDialog(strings.ToUpper(testPrefix)+testTo, testPrefix+testTo, testTo),
	}
	for name, d := range cases {
		var col collector
		judgeWire(d, testPrefix, testTo, &PrefixFacts{}, &col)
		if len(col.failures) == 0 {
			t.Errorf("%s: a dialog without the prefix passed", name)
		}
	}
}

func delivery(callID uuid.UUID, toNumber string, extra string) []byte {
	return []byte(fmt.Sprintf(`{"deliveryId":"%s","revision":1,"attempt":1,"cdr":{"callId":"%s","toNumber":"%s"%s}}`,
		uuid.New(), callID, toNumber, extra))
}

func TestTheWebhookCheckPassesOnACleanDelivery(t *testing.T) {
	id := uuid.New()
	var col collector
	f := &PrefixFacts{}
	judgeWebhooks([][]byte{delivery(uuid.New(), "999", ""), delivery(id, testTo, `,"did":"95000001"`)}, id, testPrefix, testTo, f, &col)
	if len(col.failures) != 0 {
		t.Fatal(failures(col))
	}
	if f.WebhookDeliveries != 1 || f.WebhookToNumber != testTo {
		t.Errorf("facts = %+v", f)
	}
}

func TestTheWebhookCheckFailsOnALeakedPrefix(t *testing.T) {
	id := uuid.New()
	cases := map[string][][]byte{
		"toNumber carries it":        {delivery(id, testPrefix+testTo, "")},
		"another field carries it":   {delivery(id, testTo, `,"note":"to A1B2CC13800000001"`)},
		"only the second revision":   {delivery(id, testTo, ""), delivery(id, testPrefix+testTo, "")},
		"no delivery for the call":   {delivery(uuid.New(), testTo, "")},
		"a delivery that is no JSON": {[]byte("a1b2cc")},
	}
	for name, bodies := range cases {
		var col collector
		judgeWebhooks(bodies, id, testPrefix, testTo, &PrefixFacts{}, &col)
		if len(col.failures) == 0 {
			t.Errorf("%s: passed", name)
		}
	}
}

func event(typ string, callID uuid.UUID, extra string) []byte {
	return []byte(fmt.Sprintf(`{"version":1,"seq":1,"type":"%s","callId":"%s","payload":{"to":"%s"%s}}`, typ, callID, testTo, extra))
}

func TestTheEventCheckPassesOnACleanStreamThatSawTheCall(t *testing.T) {
	id := uuid.New()
	var col collector
	f := &PrefixFacts{}
	judgeEvents([][]byte{event("CALL_CREATED", id, ""), event("PARTY_ESTABLISHED", id, ""), event("PARTY_RELEASED", uuid.New(), "")}, id, testPrefix, f, &col)
	if len(col.failures) != 0 {
		t.Fatal(failures(col))
	}
	if f.CallEvents != 1 || f.PartyEvents != 1 || f.Events != 3 {
		t.Errorf("facts = %+v", f)
	}
}

func TestTheEventCheckFailsOnALeakAnywhereInAnyEvent(t *testing.T) {
	id := uuid.New()
	base := func() [][]byte { return [][]byte{event("CALL_CREATED", id, ""), event("PARTY_ESTABLISHED", id, "")} }
	leaks := map[string][][]byte{
		"party event":         append(base(), event("PARTY_RELEASED", id, `,"number":"`+testPrefix+testTo+`"`)),
		"another call's":      append(base(), event("CALL_ENDED", uuid.New(), `,"x":"A1B2CC"`)),
		"an event of no call": append(base(), []byte(`{"type":"SYSTEM_RESET","payload":{"n":"a1b2cc"}}`)),
	}
	for name, raw := range leaks {
		var col collector
		judgeEvents(raw, id, testPrefix, &PrefixFacts{}, &col)
		if len(col.failures) == 0 {
			t.Errorf("%s: a leaking stream passed", name)
		}
	}
}

func TestTheEventCheckIsNotVacuous(t *testing.T) {
	id := uuid.New()
	for name, raw := range map[string][][]byte{
		"no events at all":      nil,
		"only CALL_*":           {event("CALL_CREATED", id, "")},
		"only PARTY_*":          {event("PARTY_ESTABLISHED", id, "")},
		"events of other calls": {event("CALL_CREATED", uuid.New(), ""), event("PARTY_ESTABLISHED", uuid.New(), "")},
	} {
		var col collector
		judgeEvents(raw, id, testPrefix, &PrefixFacts{}, &col)
		if len(col.failures) == 0 {
			t.Errorf("%s: an empty observation passed", name)
		}
	}
}

// The receiver keeps what is POSTed to it and names itself by the address the
// route to the API uses.
func TestTheWebhookReceiverKeepsEveryBody(t *testing.T) {
	r, err := newWebhookReceiver("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if !strings.HasPrefix(r.url, "http://127.0.0.1:") {
		t.Fatalf("url = %s", r.url)
	}
	id := uuid.New()
	for range 3 {
		resp, err := http.Post(r.url, "application/json", strings.NewReader(string(delivery(id, testTo, ""))))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	if n := deliveriesFor(r.received(), id); n != 3 {
		t.Fatalf("deliveries = %d, want 3", n)
	}
}

// The tap is open when openEventTap returns and hands back every data line.
func TestTheEventTapCollectsWhatTheStreamCarries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "s", Value: "1", Path: "/"})
			fmt.Fprint(w, `{}`)
		case "/api/v1/events":
			if c, err := r.Cookie("s"); err != nil || c.Value != "1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "id: 1\nevent: CALL_CREATED\ndata: {\"type\":\"CALL_CREATED\"}\n\n: keepalive\n\nid: 2\ndata: {\"type\":\"PARTY_RELEASED\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	tap, err := openEventTap(t.Context(), srv.URL, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		tap.mu.Lock()
		n := len(tap.data)
		tap.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collected %d data lines, want 2", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := tap.stop()
	if err != nil || len(data) != 2 || string(data[1]) != `{"type":"PARTY_RELEASED"}` {
		t.Fatalf("data = %q, err = %v", data, err)
	}
}
