// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"strings"
	"testing"
)

const inviteFromSwitch = "INVITE sip:aicc@10.0.0.5:6060 SIP/2.0\r\n" +
	"Via: SIP/2.0/UDP 10.0.0.9:5060;branch=z9hG4bKouter\r\n" +
	"via: SIP/2.0/UDP 10.0.0.8:5060;branch=z9hG4bKinner\r\n" +
	"Record-Route: <sip:10.0.0.9;lr>\r\n" +
	"Record-Route: <sip:10.0.0.8;lr>\r\n" +
	"f: \"Caller\" <sip:1001@10.0.0.8>;tag=remote-9\r\n" +
	"t: <sip:aicc@10.0.0.5>\r\n" +
	"i: call-abc-123\r\n" +
	"CSeq: 42 INVITE\r\n" +
	"m: <sip:10.0.0.8:5060>\r\n" +
	"X-Aicc-Call-Id: 0198f000-dead-beef\r\n" +
	"x-aicc-dnis: 8000\r\n" +
	"Content-Type: application/sdp\r\n" +
	"Content-Length: 0\r\n\r\n" +
	"v=0\r\n"

func TestParseHandlesRealWorldHeaderForms(t *testing.T) {
	msg, err := parseSIP([]byte(inviteFromSwitch))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if msg.method != methodInvite {
		t.Errorf("method = %q, want INVITE", msg.method)
	}
	// Compact forms are what a proxy under load actually sends.
	if got := msg.callID(); got != "call-abc-123" {
		t.Errorf("compact Call-ID parsed as %q", got)
	}
	if got := msg.contact(); got != "<sip:10.0.0.8:5060>" {
		t.Errorf("compact Contact parsed as %q", got)
	}
	// Header names are case insensitive, so the lowercase Via must still count.
	if vias := msg.vias(); len(vias) != 2 || !strings.Contains(vias[0], "outer") {
		t.Errorf("vias = %v, want both in order with outer first", vias)
	}
	if routes := msg.recordRoutes(); len(routes) != 2 {
		t.Errorf("recordRoutes = %v, want two", routes)
	}
	if got := msg.body; got != "v=0" {
		t.Errorf("body = %q", got)
	}
}

func TestCustomHeadersAreNormalisedButValuesArePreserved(t *testing.T) {
	msg, _ := parseSIP([]byte(inviteFromSwitch))
	headers := msg.customHeaders()

	// The value is a correlation key; changing its case would break the join.
	if got := headers["X-Aicc-Call-Id"]; got != "0198f000-dead-beef" {
		t.Errorf("X-Aicc-Call-Id = %q", got)
	}
	if got := headers["X-Aicc-Dnis"]; got != "8000" {
		t.Errorf("lowercase custom header came through as %q, want normalised key", got)
	}
	if _, present := headers["Cseq"]; present {
		t.Error("a non X- header leaked into the custom set")
	}
}

func TestMultipleViasOnOneLineAreSplit(t *testing.T) {
	raw := "OPTIONS sip:aicc@10.0.0.5 SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP a.example;branch=z9hG4bK1, SIP/2.0/UDP b.example;branch=z9hG4bK2\r\n" +
		"From: <sip:x@a.example>;tag=1\r\nTo: <sip:aicc@10.0.0.5>\r\n" +
		"Call-ID: c\r\nCSeq: 1 OPTIONS\r\n\r\n"

	msg, err := parseSIP([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if vias := msg.vias(); len(vias) != 2 {
		t.Fatalf("vias = %v, want two split on the comma", vias)
	}
}

func TestCommasInsideURIsAndQuotesDoNotSplit(t *testing.T) {
	got := splitHeaderValues(`"Doe, John" <sip:a@b;p=1,2>, <sip:c@d>`)
	if len(got) != 2 {
		t.Fatalf("split into %d parts (%v), want 2", len(got), got)
	}
	if !strings.Contains(got[0], "Doe, John") {
		t.Errorf("a comma inside a quoted display name split the value: %q", got[0])
	}
}

func TestResponsesEchoTheFullRoutingSet(t *testing.T) {
	msg, _ := parseSIP([]byte(inviteFromSwitch))
	response := string(build200OKInvite(msg, "v=0\r\n", "10.0.0.5", 6060, "aicc-1"))

	// Dropping or reordering these makes the response unroutable back through
	// the proxies that added them.
	outer := strings.Index(response, "z9hG4bKouter")
	inner := strings.Index(response, "z9hG4bKinner")
	if outer < 0 || inner < 0 {
		t.Fatalf("a Via was dropped from the response:\n%s", response)
	}
	if outer > inner {
		t.Error("Via order was reversed in the response")
	}
	if strings.Count(response, "Record-Route:") != 2 {
		t.Errorf("Record-Route set not echoed:\n%s", response)
	}
	if !strings.Contains(response, "To: <sip:aicc@10.0.0.5>;tag=aicc-1") {
		t.Error("the local tag was not added to To")
	}
	if !strings.Contains(response, "Content-Length: 5") { // len("v=0\r\n")
		t.Errorf("Content-Length does not match the body:\n%s", response)
	}
}

func TestExistingToTagIsNotDuplicated(t *testing.T) {
	raw := strings.Replace(inviteFromSwitch, "t: <sip:aicc@10.0.0.5>",
		"t: <sip:aicc@10.0.0.5>;tag=already", 1)
	msg, _ := parseSIP([]byte(raw))

	response := string(buildReject(msg, "486 Busy Here", "aicc-2"))
	if strings.Count(response, "tag=") != 2 { // one in From, one in To
		t.Errorf("a second To tag was appended:\n%s", response)
	}
}

// A 487 belongs to the INVITE transaction. Answering with the CANCEL's own
// CSeq leaves the caller waiting for a final response that never comes.
func Test487CarriesTheInviteCSeq(t *testing.T) {
	invite, _ := parseSIP([]byte(inviteFromSwitch))

	response := string(build487(invite, "aicc-3"))
	if !strings.Contains(response, "CSeq: 42 INVITE") {
		t.Errorf("487 does not carry the INVITE's CSeq:\n%s", response)
	}
	if strings.Contains(response, "CANCEL") {
		t.Errorf("487 refers to the CANCEL transaction:\n%s", response)
	}
	if !strings.Contains(response, "487 Request Terminated") {
		t.Errorf("wrong status line:\n%s", response)
	}
}

func TestByeReversesTheRouteSetAndTargetsTheContact(t *testing.T) {
	msg, _ := parseSIP([]byte(inviteFromSwitch))
	bye := string(buildBye(msg.callID(), msg.fromHeader(), msg.toHeader(), "aicc-4",
		"10.0.0.5", 6060, msg.recordRoutes(), msg.contact(), byeReason{}))

	if !strings.HasPrefix(bye, "BYE sip:10.0.0.8:5060 SIP/2.0") {
		t.Errorf("BYE is not addressed to the peer's Contact:\n%s", bye)
	}
	// The route set is applied in reverse for a request in the other direction.
	first := strings.Index(bye, "Route: <sip:10.0.0.8;lr>")
	second := strings.Index(bye, "Route: <sip:10.0.0.9;lr>")
	if first < 0 || second < 0 {
		t.Fatalf("Route headers missing:\n%s", bye)
	}
	if first > second {
		t.Error("the route set was not reversed")
	}
	// From and To swap relative to the INVITE, and our tag goes on the new From.
	if !strings.Contains(bye, "From: <sip:aicc@10.0.0.5>;tag=aicc-4") {
		t.Errorf("From/To were not swapped or the local tag is missing:\n%s", bye)
	}
	if !strings.Contains(bye, `To: "Caller" <sip:1001@10.0.0.8>;tag=remote-9`) {
		t.Errorf("To does not carry the remote party:\n%s", bye)
	}
	// An ordinary goodbye explains nothing, because there is nothing to
	// explain: the conversation ended the way conversations end.
	if strings.Contains(bye, "Reason:") {
		t.Errorf("a plain BYE carries a Reason it has no reason for:\n%s", bye)
	}
}

// A restart is not the caller hanging up and not the bot finishing, and the
// switch has no way to tell those apart unless it is told.
//
// aicc_inbound.lua keeps a caller alive when the bot leg vanishes and hands
// them to a person, and logs which vanishing it was from what the switch
// recorded — "unknown" being what it gets when the BYE says nothing.
// FreeSWITCH maps the Q.850 reason onto the hangup cause, so this is also what
// makes the ledger able to tell a restart from a goodbye.
func TestAByeSentBecauseWeAreRestartingSaysSo(t *testing.T) {
	msg, _ := parseSIP([]byte(inviteFromSwitch))
	bye := string(buildBye(msg.callID(), msg.fromHeader(), msg.toHeader(), "aicc-4",
		"10.0.0.5", 6060, msg.recordRoutes(), msg.contact(), byeReasonRestart))

	if !strings.Contains(bye, `Reason: SIP;cause=503;text="Service Restart"`) {
		t.Errorf("the SIP reason is missing or malformed:\n%s", bye)
	}
	if !strings.Contains(bye, `Reason: Q.850;cause=41;text="Temporary failure"`) {
		t.Errorf("the Q.850 reason is missing or malformed:\n%s", bye)
	}
	// Headers belong above the body, and a BYE has none — Content-Length is
	// the last line, so anything after it is outside the message.
	if strings.Index(bye, "Reason:") > strings.Index(bye, "Content-Length:") {
		t.Errorf("the Reason headers are below Content-Length:\n%s", bye)
	}
}

func TestParseInfoDTMF(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{"relay form", "application/dtmf-relay", "Signal=5\r\nDuration=250", "5"},
		{"relay with spaces", "application/dtmf-relay", "Signal = #", "#"},
		{"lowercase letter digit", "application/dtmf-relay", "signal=a", "A"},
		{"bare form", "application/dtmf", "7", "7"},
		{"unrelated body", "application/sdp", "v=0", ""},
		{"relay without a signal", "application/dtmf-relay", "Duration=100", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &sipMessage{
				headers: map[string]string{"content-type": tt.contentType},
				body:    tt.body,
			}
			if got := parseInfoDTMF(msg); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "SIP/2.0\r\n\r\n", "SIP/2.0 notanumber OK\r\n\r\n"} {
		if _, err := parseSIP([]byte(raw)); err == nil {
			t.Errorf("parsing %q returned no error", raw)
		}
	}
}
