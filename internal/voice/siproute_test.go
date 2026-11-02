// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
)

func TestParseHop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		uri  string
		want sipHop
	}{
		{"sip:10.0.0.9;lr", sipHop{scheme: "sip", host: "10.0.0.9", port: 5060, isLooseRouter: true}},
		{"sip:1001@10.0.0.8:5062;transport=UDP", sipHop{scheme: "sip", host: "10.0.0.8", port: 5062, transport: "udp"}},
		{"SIPS:sbc.example.net", sipHop{scheme: "sips", host: "sbc.example.net", port: 5061}},
		{"sip:[2001:db8::7]:5080;lr", sipHop{scheme: "sip", host: "2001:db8::7", port: 5080, isLooseRouter: true}},
		{"sip:[2001:db8::7]", sipHop{scheme: "sip", host: "2001:db8::7", port: 5060}},
		{"sip:proxy.example.net;maddr=10.0.0.44;LR", sipHop{scheme: "sip", host: "10.0.0.44", port: 5060, isLooseRouter: true}},
		{"sip:alice;day=tue@10.0.0.3:5070?Subject=hi", sipHop{scheme: "sip", host: "10.0.0.3", port: 5070}},
		{"sip:10.0.0.6;transport=tcp;lr", sipHop{scheme: "sip", host: "10.0.0.6", port: 5060, transport: "tcp", isLooseRouter: true}},
	}
	for _, tt := range tests {
		got, err := parseHop(tt.uri)
		if err != nil {
			t.Errorf("parseHop(%q): %v", tt.uri, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseHop(%q) = %+v, want %+v", tt.uri, got, tt.want)
		}
	}
	for _, bad := range []string{
		"tel:+15551234", "sip:", "sip:;lr", "sip:[2001:db8::1", "sip:[2001:db8::1]5060",
		"sip:2001:db8::1", "sip:10.0.0.1:0", "sip:10.0.0.1:65536", "sip:10.0.0.1:port",
	} {
		if hop, err := parseHop(bad); err == nil {
			t.Errorf("parseHop(%q) = %+v, want an error", bad, hop)
		}
	}
}

// Every BYE follows its route set: the Route headers in the INVITE's order,
// the Request-URI as RFC 3261 §12.2.1.1 sets it for the first entry's kind,
// and the next hop at the first entry's address when it is an IP literal over
// UDP, or an error otherwise (the dialog then falls back to the INVITE's
// source, which TestByeNextHopFallsBackToTheInviteSource covers). A hostname
// is never resolved, so the hostname rows are refusals.
func TestByeFollowsItsRouteSet(t *testing.T) {
	t.Parallel()
	const (
		contact = `"Caller" <sip:1001@10.0.0.8:5062>`
		target  = "sip:1001@10.0.0.8:5062"

		loose4     = "<sip:10.0.0.9;lr>"
		loosePort  = "<sip:10.0.0.8:5070;lr>"
		loose6     = "<sip:[2001:db8::7]:5080;lr>"
		looseHost  = "<sip:proxy.example.net;lr>;x-param=1"
		looseTCP   = "<sip:10.0.0.6;transport=tcp;lr>"
		looseSIPS  = "<sips:sbc.example.net:5061;lr>"
		looseMaddr = "<sip:proxy.example.net;maddr=10.0.0.44;lr>"
		strict4    = "<sip:10.0.0.7>"
		strictHost = "<sip:strict.example.net:5090>"
		// A strict router's URI may carry what a Request-URI may not.
		strictDirty = "<sip:10.0.0.7:5099;method=INVITE;transport=udp?X-Foo=bar>"
	)
	tests := []struct {
		name       string
		set        []string
		wantURI    string
		wantRoutes []string
		// wantHop is the next hop on a dual-stack socket and wantHop4 on the
		// UAS's IPv4 one; empty means the hop is refused.
		wantHop, wantHop4 string
	}{
		{name: "no route set", wantURI: target},
		{name: "one loose IPv4, default port", set: []string{loose4},
			wantURI: target, wantRoutes: []string{loose4},
			wantHop: "10.0.0.9:5060", wantHop4: "10.0.0.9:5060"},
		{name: "one loose, explicit port", set: []string{loosePort},
			wantURI: target, wantRoutes: []string{loosePort},
			wantHop: "10.0.0.8:5070", wantHop4: "10.0.0.8:5070"},
		{name: "one strict", set: []string{strict4},
			wantURI: "sip:10.0.0.7", wantRoutes: []string{"<" + target + ">"},
			wantHop: "10.0.0.7:5060", wantHop4: "10.0.0.7:5060"},
		{name: "one loose over tcp", set: []string{looseTCP},
			wantURI: target, wantRoutes: []string{looseTCP}},
		{name: "one loose sips", set: []string{looseSIPS},
			wantURI: target, wantRoutes: []string{looseSIPS}},
		{name: "one loose hostname", set: []string{looseHost},
			wantURI: target, wantRoutes: []string{looseHost}},
		{name: "one loose hostname with an IP maddr", set: []string{looseMaddr},
			wantURI: target, wantRoutes: []string{looseMaddr},
			wantHop: "10.0.0.44:5060", wantHop4: "10.0.0.44:5060"},
		{name: "three, hostname first, strict later", set: []string{looseHost, strict4, loosePort},
			wantURI: target, wantRoutes: []string{looseHost, strict4, loosePort}},
		{name: "three, loose first, strict later", set: []string{loosePort, strict4, looseHost},
			wantURI: target, wantRoutes: []string{loosePort, strict4, looseHost},
			wantHop: "10.0.0.8:5070", wantHop4: "10.0.0.8:5070"},
		{name: "three, strict hostname first", set: []string{strictHost, loose4, loose6},
			wantURI: "sip:strict.example.net:5090", wantRoutes: []string{loose4, loose6, "<" + target + ">"}},
		{name: "three, IPv6 first", set: []string{loose6, loose4, loosePort},
			wantURI: target, wantRoutes: []string{loose6, loose4, loosePort},
			wantHop: "[2001:db8::7]:5080"},
		{name: "eight, loose first", set: []string{loose4, loosePort, loose6, looseHost, looseTCP, looseSIPS, strict4, strictHost},
			wantURI: target, wantRoutes: []string{loose4, loosePort, loose6, looseHost, looseTCP, looseSIPS, strict4, strictHost},
			wantHop: "10.0.0.9:5060", wantHop4: "10.0.0.9:5060"},
		{name: "eight, strict first", set: []string{strictDirty, loose4, loosePort, loose6, looseHost, looseTCP, looseSIPS, strictHost},
			wantURI:    "sip:10.0.0.7:5099;transport=udp",
			wantRoutes: []string{loose4, loosePort, loose6, looseHost, looseTCP, looseSIPS, strictHost, "<" + target + ">"},
			wantHop:    "10.0.0.7:5099", wantHop4: "10.0.0.7:5099"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := string(buildBye("call-1", contact, "<sip:aicc@10.0.0.5>", "aicc-1",
				"10.0.0.5", 6060, tt.set, contact, byeReason{}))
			bye, err := parseSIP([]byte(raw))
			if err != nil {
				t.Fatalf("parse: %v\n%s", err, raw)
			}
			if bye.method != methodBye || bye.requestURI != tt.wantURI {
				t.Errorf("request line is %s %s, want BYE %s", bye.method, bye.requestURI, tt.wantURI)
			}
			if got := bye.multi["route"]; !slices.Equal(got, tt.wantRoutes) {
				t.Errorf("Route headers = %q, want %q\n%s", got, tt.wantRoutes, raw)
			}
			if n := strings.Count(raw, "\r\nMax-Forwards: 70\r\n"); n != 1 || strings.Count(strings.ToLower(raw), "max-forwards") != 1 {
				t.Errorf("want exactly one Max-Forwards: 70\n%s", raw)
			}
			for network, want := range map[string]string{"udp": tt.wantHop, "udp4": tt.wantHop4} {
				hop, err := routeSetNextHop(tt.set, network)
				switch {
				case want == "" && err == nil:
					t.Errorf("%s next hop = %v, want it refused", network, hop)
				case want != "" && err != nil:
					t.Errorf("%s next hop refused (%v), want %s", network, err, want)
				case want != "" && hop.String() != want:
					t.Errorf("%s next hop = %v, want %s", network, hop, want)
				}
			}
		})
	}
}

// However the INVITE groups and folds its Record-Route values, the dialog has
// the same route set. A folded line used to be dropped, which lost the second
// proxy (issue #325).
func TestRecordRouteIsTheSameSetHoweverItIsWritten(t *testing.T) {
	t.Parallel()
	want := []string{"<sip:p3.example.net;lr>", "<sip:p2;lr>", "<sip:p1;lr>"}
	tests := map[string]string{
		"separate headers": "Record-Route: <sip:p3.example.net;lr>\r\n" +
			"Record-Route: <sip:p2;lr>\r\nRecord-Route: <sip:p1;lr>\r\n",
		"one comma-joined header": "Record-Route: <sip:p3.example.net;lr>, <sip:p2;lr>,<sip:p1;lr>\r\n",
		"folded over two lines": "Record-Route: <sip:p3.example.net;lr>, <sip:p2;lr>,\r\n" +
			" <sip:p1;lr>\r\n",
		"folded over three lines": "Record-Route: <sip:p3.example.net;lr>,\r\n" +
			"\t<sip:p2;lr>,\r\n" +
			"   <sip:p1;lr>\r\n",
		"folded before the first value": "Record-Route:\r\n <sip:p3.example.net;lr>\r\n" +
			"record-route: <sip:p2;lr>,\r\n\t <sip:p1;lr>\r\n",
	}
	for name, routing := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := strings.Replace(inviteFromSwitch,
				"Record-Route: <sip:10.0.0.9;lr>\r\nRecord-Route: <sip:10.0.0.8;lr>\r\n", routing, 1)
			msg, err := parseSIP([]byte(raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := msg.recordRoutes(); !slices.Equal(got, want) {
				t.Errorf("route set = %q, want %q", got, want)
			}
			// The header after the folded one is still read as itself.
			if got := msg.fromHeader(); got != `"Caller" <sip:1001@10.0.0.8>;tag=remote-9` {
				t.Errorf("From = %q after the folded header", got)
			}
		})
	}
}

// Each BYE is its own transaction. The branch used to be the Call-ID's first
// eight characters, so calls whose Call-IDs share them sent BYEs a
// transaction-stateful proxy could take for one retransmitted request.
func TestEveryByeHasItsOwnBranch(t *testing.T) {
	t.Parallel()
	const n = 10000
	seen := make(map[string]bool, n)
	for i := range n {
		raw := buildBye(fmt.Sprintf("abcdefgh-%d", i), "<sip:1001@10.0.0.8>;tag=r", "<sip:aicc@10.0.0.5>",
			"aicc-1", "10.0.0.5", 6060, nil, "", byeReason{})
		msg, err := parseSIP(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, branch, _ := strings.Cut(msg.get("Via"), ";branch=")
		if !strings.HasPrefix(branch, "z9hG4bK") || len(branch) <= len("z9hG4bK") {
			t.Fatalf("branch %q is not an RFC 3261 branch", branch)
		}
		if seen[branch] {
			t.Fatalf("BYE %d repeats branch %q", i, branch)
		}
		seen[branch] = true
	}
}

// The dialog sends the BYE to the top of its route set, and to the INVITE's
// source when there is no set or the top entry cannot be sent to: the BYE
// still goes out, it only skips that proxy.
func TestByeNextHopFallsBackToTheInviteSource(t *testing.T) {
	t.Parallel()
	source := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 8), Port: 5080}
	tests := []struct {
		name     string
		routeSet []string
		want     string
	}{
		{"no route set: the source, not the Contact", nil, "10.0.0.8:5080"},
		{"IPv4 literal", []string{"<sip:10.0.0.9:5070;lr>", "<sip:10.0.0.1;lr>"}, "10.0.0.9:5070"},
		{"IPv4 literal, default port", []string{"<sip:10.0.0.9;transport=UDP;lr>"}, "10.0.0.9:5060"},
		{"hostname: not resolved", []string{"<sip:proxy.example.net;lr>"}, "10.0.0.8:5080"},
		{"tcp", []string{"<sip:10.0.0.9;transport=tcp;lr>"}, "10.0.0.8:5080"},
		{"sips", []string{"<sips:10.0.0.9;lr>"}, "10.0.0.8:5080"},
		{"IPv6 on the IPv4 socket", []string{"<sip:[2001:db8::7];lr>"}, "10.0.0.8:5080"},
		{"no URI at all", []string{"garbage"}, "10.0.0.8:5080"},
		{"unparseable URI", []string{"<sip:10.0.0.9:99999;lr>"}, "10.0.0.8:5080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &Dialog{
				SIPCallID: "call-1", RemoteContact: "<sip:1001@10.0.0.200:5060>",
				RecordRoutes: tt.routeSet,
				log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			if got := d.byeNextHop(source).String(); got != tt.want {
				t.Errorf("next hop = %s, want %s", got, tt.want)
			}
		})
	}
}
