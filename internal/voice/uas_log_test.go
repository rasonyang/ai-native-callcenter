// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pion/rtcp"
)

// logRecord is one captured log line: the raw text, so a key written twice is
// visible (a decoded map would hide it), and the decoded attributes.
type logRecord struct {
	raw   string
	attrs map[string]any
}

func (r logRecord) msg() string { s, _ := r.attrs["msg"].(string); return s }

// logCapture is a JSON logger over a locked buffer, parsed per line.
type logCapture struct {
	buf    lockedBuffer
	logger *slog.Logger
}

func newLogCapture() *logCapture {
	c := &logCapture{}
	c.logger = slog.New(slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return c
}

// configure points a test UAS at the capture.
func (c *logCapture) configure(cfg *Config) {
	cfg.Logger = c.logger
}

func (c *logCapture) records(t *testing.T) []logRecord {
	t.Helper()
	var out []logRecord
	for line := range strings.SplitSeq(c.buf.String(), "\n") {
		if line == "" {
			continue
		}
		rec := logRecord{raw: line, attrs: map[string]any{}}
		if err := json.Unmarshal([]byte(line), &rec.attrs); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// awaitRecord waits for a record with the message, since the UAS logs from its
// own goroutines, after the peer has already seen the packet.
func (c *logCapture) awaitRecord(t *testing.T, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range c.records(t) {
			if r.msg() == msg {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q record arrived; log:\n%s", msg, c.buf.String())
}

// checkDialogRecords asserts the logging contract for every record about the
// one dialog a test drives: sipCallId is the Call-ID, callId is the header's
// id or absent, never the Call-ID, and neither key is written twice.
func checkDialogRecords(t *testing.T, c *logCapture, sipCallID, wantCallID string) {
	t.Helper()
	seen := 0
	for _, r := range c.records(t) {
		if m := r.msg(); m == "sip uas started" || m == "sip uas stopped" {
			continue
		}
		got, _ := r.attrs["sipCallId"].(string)
		if got == "" && isDispatchLine(r.msg()) {
			// A datagram with no Call-ID at all (another test's RTP landing on
			// a reused port, say): it names no dialog, so it is not this one.
			// One that has a Call-ID always names it.
			t.Logf("skipping a line about a datagram with no Call-ID: %s", r.raw)
			continue
		}
		if got != "" && got != sipCallID {
			// Another dialog's message: under load a socket's port is reused
			// while an earlier test's peer is still answering it. It names its
			// own Call-ID, which is the contract; it is just not this dialog.
			t.Logf("skipping another dialog's line: %s", r.raw)
			continue
		}
		seen++
		if got != sipCallID {
			t.Errorf("%q: sipCallId = %q, want %q\n%s", r.msg(), got, sipCallID, r.raw)
		}
		got, has := r.attrs["callId"].(string)
		switch {
		case wantCallID == "" && has:
			t.Errorf("%q carries callId %q although the header held no usable id\n%s", r.msg(), got, r.raw)
		case wantCallID != "" && got != wantCallID:
			t.Errorf("%q: callId = %q, want %q\n%s", r.msg(), got, wantCallID, r.raw)
		}
		if has && got == sipCallID {
			t.Errorf("%q: callId equals the SIP Call-ID\n%s", r.msg(), r.raw)
		}
		for _, key := range []string{`"callId":`, `"sipCallId":`} {
			if n := strings.Count(r.raw, key); n > 1 {
				t.Errorf("%q writes %s %d times\n%s", r.msg(), key, n, r.raw)
			}
		}
	}
	if seen < 2 {
		t.Errorf("only %d records about the dialog; log:\n%s", seen, c.buf.String())
	}
}

// isDispatchLine is a line the UAS logs about a datagram before it belongs to
// any dialog.
func isDispatchLine(msg string) bool {
	return msg == "sip method not handled" || msg == "sip response ignored" || msg == "sip parse failed"
}

// Call-IDs as UACs really send them. A `;` or `=` is not a legal word
// character in a Call-ID (RFC 3261 §25.1), but the parser takes a header value
// whole, so the UAS must still log it verbatim; that is what the case checks.
var messySIPCallIDs = []struct{ name, id string }{
	{"sofia non-RFC variant", "58e41e83-5042-1240-2298-610aa82ba9f6"},
	{"local at host", "a84b4c76e66710@10.130.0.5"},
	{"plus slash", "ab+cd/ef_gh@host.example"},
	{"semicolon and equals (not legal on the wire, tolerated)", "x;y=z@host"},
	{"over 200 chars", strings.Repeat("0123456789abcdef", 14) + "@long.example"},
}

const (
	v7ID = "01a19507-5177-7dc9-bef3-91e32915b768"
	v4ID = "9b2f0c1e-6d3a-4e8b-8f21-5c7a1d0e4b36"
)

var messyAICCHeaders = []struct {
	name  string
	lines []string // nil: none; the peer sends exactly these
	want  string   // canonical callId, "" for absent
}{
	{"v7", []string{"X-AICC-Call-ID: " + v7ID}, v7ID},
	{"v4 legacy", []string{"X-AICC-Call-ID: " + v4ID}, v4ID},
	{"upper-case value is canonicalised", []string{"X-AICC-Call-ID: " + strings.ToUpper(v7ID)}, v7ID},
	{"missing", []string{}, ""},
	{"empty", []string{"X-AICC-Call-ID: "}, ""},
	{"garbage", []string{"X-AICC-Call-ID: not-a-uuid-at-all"}, ""},
	{"nil uuid", []string{"X-AICC-Call-ID: " + uuid.Nil.String()}, ""},
	{"twice, first wins", []string{"X-AICC-Call-ID: " + v7ID, "X-AICC-Call-ID: " + v4ID}, v7ID},
	{"lowercase name", []string{"x-aicc-call-id: " + v7ID}, v7ID},
}

func TestEveryDialogLineCarriesTheSIPCallIDAndOnlyARealCallID(t *testing.T) {
	// Every header shape on the first Call-ID, and every other Call-ID shape on
	// the first header: the two vary independently, so the full product adds
	// calls (and RTP binds) without adding a case.
	type pair struct{ id, hdr int }
	var pairs []pair
	for h := range messyAICCHeaders {
		pairs = append(pairs, pair{0, h})
	}
	for i := 1; i < len(messySIPCallIDs); i++ {
		pairs = append(pairs, pair{i, 0})
	}
	for _, pr := range pairs {
		id, hdr := messySIPCallIDs[pr.id], messyAICCHeaders[pr.hdr]
		for _, end := range []string{"bot hangs up", "peer hangs up"} {
			t.Run(id.name+"/"+hdr.name+"/"+end, func(t *testing.T) {
				capture := newLogCapture()
				_, hooks, p := startUAS(t, capture.configure)
				p.callID = id.id
				p.aiccLines = hdr.lines
				_, dialog := p.connect(hooks)

				if dialog.SIPCallID != id.id {
					t.Fatalf("dialog.SIPCallID = %q, want %q", dialog.SIPCallID, id.id)
				}
				wantID := uuid.Nil
				if hdr.want != "" {
					wantID = uuid.MustParse(hdr.want)
				}
				if got := dialog.CallID(); got != wantID {
					t.Errorf("dialog.CallID() = %s, want %s", got, wantID)
				}

				if end == "bot hangs up" {
					dialog.Stop()
					capture.awaitRecord(t, "sip bye sent")
				} else {
					p.cseq++
					p.request("BYE", "")
					p.awaitStatus(200, 2*time.Second)
					select {
					case <-hooks.ended:
					case <-time.After(2 * time.Second):
						t.Fatal("the call was never reported as ended")
					}
				}
				checkDialogRecords(t, capture, id.id, hdr.want)
			})
		}
	}
}

// Two dialogs whose Call-IDs differ only in case are two dialogs, and each
// one's lines carry its own Call-ID and its own call id.
func TestCallIDsThatDifferOnlyInCaseKeepTheirOwnLines(t *testing.T) {
	capture := newLogCapture()
	uas, hooks, a := startUAS(t, capture.configure)
	b := newPeer(t, uas.LocalPort())
	a.callID, b.callID = "AbC-1234@Host", "abc-1234@host"
	otherID := "01a19507-5177-7dc9-bef3-91e32915b769"
	a.aiccLines = []string{"X-AICC-Call-ID: " + v7ID}
	b.aiccLines = []string{"X-AICC-Call-ID: " + otherID}

	_, da := a.connect(hooks)
	_, db := b.connect(hooks)
	if da.SIPCallID != a.callID || db.SIPCallID != b.callID {
		t.Fatalf("dialogs mixed up: %q %q", da.SIPCallID, db.SIPCallID)
	}
	if got := uas.ActiveCalls(); got != 2 {
		t.Fatalf("active calls = %d, want 2 (Call-IDs are case sensitive)", got)
	}
	da.Stop()
	db.Stop()
	capture.awaitRecord(t, "sip bye sent")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && countRecords(t, capture, "sip bye sent") < 2 {
		time.Sleep(5 * time.Millisecond)
	}

	want := map[string]string{a.callID: v7ID, b.callID: otherID}
	byeLines := 0
	for _, r := range capture.records(t) {
		sip, _ := r.attrs["sipCallId"].(string)
		if sip == "" {
			continue
		}
		wantCall, ok := want[sip]
		if !ok {
			t.Errorf("%q carries unknown sipCallId %q", r.msg(), sip)
			continue
		}
		if got, _ := r.attrs["callId"].(string); got != wantCall {
			t.Errorf("%q: sipCallId %q has callId %q, want %q", r.msg(), sip, got, wantCall)
		}
		if r.msg() == "sip bye sent" {
			byeLines++
		}
	}
	if byeLines != 2 {
		t.Errorf("%d bye lines, want 2", byeLines)
	}
}

func countRecords(t *testing.T, c *logCapture, msg string) int {
	n := 0
	for _, r := range c.records(t) {
		if r.msg() == msg {
			n++
		}
	}
	return n
}

// The pre-dialog refusals log through the same logger and keep the raw slot
// key under "reservation".
func TestARefusedInviteLogsItsSIPCallIDAndTheReservation(t *testing.T) {
	capture := newLogCapture()
	uas, hooks, first := startUAS(t, func(c *Config) {
		capture.configure(c)
		c.MaxCalls = 1
	})
	first.connect(hooks) // takes the only slot
	p := newPeer(t, uas.LocalPort())
	p.callID = "refused@host"
	p.aiccLines = []string{"X-AICC-Call-ID: " + v7ID}
	p.request("INVITE", p.offer())
	p.awaitStatus(486, 2*time.Second)
	capture.awaitRecord(t, "invite rejected at capacity")

	for _, r := range capture.records(t) {
		if r.msg() != "invite rejected at capacity" {
			continue
		}
		if r.attrs["sipCallId"] != "refused@host" || r.attrs["callId"] != v7ID || r.attrs["reservation"] != v7ID {
			t.Errorf("refusal line has the wrong ids: %s", r.raw)
		}
	}
}

// An id learned after media started (the orchestrator's fallback mint) reaches
// the dialog's later lines and those of its RTP and RTCP sessions; a second,
// different id changes nothing, and uuid.Nil is not an id.
func TestABoundCallIDReachesTheMediaSessionsLaterLines(t *testing.T) {
	capture := newLogCapture()
	_, hooks, p := startUAS(t, capture.configure)
	p.callID = "late-bind@host"
	p.aiccLines = []string{"X-AICC-Call-ID: garbage"}
	_, dialog := p.connect(hooks) // media (RTP, RTCP) has started

	if dialog.CallID() != uuid.Nil {
		t.Fatalf("CallID() = %s before any bind", dialog.CallID())
	}
	dialog.BindCallID(uuid.Nil)
	if dialog.CallID() != uuid.Nil {
		t.Fatal("binding uuid.Nil set an id")
	}
	minted := uuid.MustParse(v7ID)
	dialog.BindCallID(minted)
	dialog.BindCallID(uuid.MustParse(v4ID))
	if got := dialog.CallID(); got != minted {
		t.Fatalf("CallID() = %s after a second bind, want the first, %s", got, minted)
	}

	dialog.RTP.log.Info("probe rtp")
	dialog.mu.Lock()
	session := dialog.rtcp
	dialog.mu.Unlock()
	if session == nil {
		t.Fatal("the dialog has no rtcp session")
	}
	session.handlePacket(&rtcp.Goodbye{Sources: []uint32{1}})
	dialog.Stop()
	capture.awaitRecord(t, "sip bye sent")

	for _, r := range capture.records(t) {
		if r.msg() == "sip uas started" {
			continue
		}
		if r.attrs["sipCallId"] != "late-bind@host" {
			t.Errorf("%q: wrong sipCallId: %s", r.msg(), r.raw)
		}
	}
	for _, msg := range []string{"probe rtp", "rtcp goodbye from remote", "sip bye sent"} {
		found := false
		for _, r := range capture.records(t) {
			if r.msg() != msg {
				continue
			}
			found = true
			if r.attrs["callId"] != v7ID {
				t.Errorf("%q after the bind lacks the bound callId: %s", msg, r.raw)
			}
			if strings.Count(r.raw, `"callId":`) != 1 {
				t.Errorf("%q writes callId more than once: %s", msg, r.raw)
			}
		}
		if !found {
			t.Errorf("no %q record", msg)
		}
	}
	// Lines logged before the bind stay as they were: no guess is back-filled.
	for _, r := range capture.records(t) {
		if r.msg() == "invite accepted" || r.msg() == "rtp started" {
			if _, has := r.attrs["callId"]; has {
				t.Errorf("%q, logged before the bind, carries callId: %s", r.msg(), r.raw)
			}
		}
	}
}

// The watchdog's lines are dialog lines too.
func TestWatchdogLinesCarryTheDialogIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		cfg  func(*Config)
		feed bool
	}{
		{"media never started", "media never started", func(c *Config) {
			c.RTPDeadTimeout = 100 * time.Millisecond
			c.FirstMediaTimeout = 300 * time.Millisecond
		}, false},
		{"media went dead", "media went dead", func(c *Config) {
			c.RTPDeadTimeout = 300 * time.Millisecond
			c.FirstMediaTimeout = 0
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newLogCapture()
			_, hooks, p := startUAS(t, func(c *Config) {
				capture.configure(c)
				tc.cfg(c)
			})
			p.callID = "watchdog@host"
			p.aiccLines = []string{"X-AICC-Call-ID: " + v7ID}
			answer, _ := p.connect(hooks)
			if tc.feed {
				sendCallerFrame(t, p, answer)
			}
			select {
			case <-hooks.ended:
			case <-time.After(5 * time.Second):
				t.Fatal("the watchdog never ended the call")
			}
			capture.awaitRecord(t, tc.msg)
			capture.awaitRecord(t, "sip bye sent")
			checkDialogRecords(t, capture, "watchdog@host", v7ID)
		})
	}
}

// A SIP response reaching the UAS (the 200 OK to a BYE it sent, or a stray one
// for a dialog it never had) is logged with the Call-ID it names: through the
// dialog's logger while the dialog is known, so it carries callId too.
func TestAnIgnoredResponseNamesItsDialog(t *testing.T) {
	capture := newLogCapture()
	_, hooks, p := startUAS(t, capture.configure)
	p.callID = "resp-1@host"
	p.aiccLines = []string{"X-AICC-Call-ID: " + v7ID}
	_, _ = p.connect(hooks)

	respond := func(callID string) {
		t.Helper()
		msg := "SIP/2.0 200 OK\r\n" +
			"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK-resp\r\n" +
			"From: <sip:aicc@127.0.0.1>;tag=aicc-1\r\n" +
			"To: <sip:1001@127.0.0.1>;tag=peer-tag\r\n" +
			"Call-ID: " + callID + "\r\n" +
			"CSeq: 1 BYE\r\n" +
			"Content-Length: 0\r\n\r\n"
		if _, err := p.sip.WriteToUDP([]byte(msg), p.uasAddr); err != nil {
			t.Fatalf("send response: %v", err)
		}
	}
	respond(p.callID)
	respond("stray-2@elsewhere")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && countRecords(t, capture, "sip response ignored") < 2 {
		time.Sleep(5 * time.Millisecond)
	}

	want := map[string]string{p.callID: v7ID, "stray-2@elsewhere": ""}
	seen := 0
	for _, r := range capture.records(t) {
		if r.msg() != "sip response ignored" {
			continue
		}
		seen++
		sip, _ := r.attrs["sipCallId"].(string)
		wantID, ok := want[sip]
		if !ok {
			t.Errorf("response line names Call-ID %q: %s", sip, r.raw)
			continue
		}
		got, has := r.attrs["callId"].(string)
		if wantID == "" && has || wantID != "" && got != wantID {
			t.Errorf("response for %q: callId = %q, want %q: %s", sip, got, wantID, r.raw)
		}
		if status, _ := r.attrs["status"].(float64); status != 200 {
			t.Errorf("response line status = %v, want 200: %s", r.attrs["status"], r.raw)
		}
	}
	if seen != 2 {
		t.Errorf("%d response lines, want 2; log:\n%s", seen, capture.buf.String())
	}
}
