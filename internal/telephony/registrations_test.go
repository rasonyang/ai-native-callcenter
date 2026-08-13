// SPDX-License-Identifier: Apache-2.0

package telephony

import "testing"

// Captured from the live switch, including the WebSocket contact form the
// agent softphone registers with.
const liveRegistrations = `
Registrations:
=================================================================================================
Call-ID:    	r0mdqg7o7hlq0vsbkei1
User:       	1008@192.168.31.248
Contact:    	"" <sip:rqaegtos@s9ndvllpovdr.invalid;transport=ws;fs_nat=yes>
Agent:      	SIP.js/0.21.2
Status:     	Registered(WSS-NAT)(unknown) EXP(2026-08-13 12:51:41) EXPSECS(156)
Ping-Status:	Reachable
Ping-Time:	4.78
Host:       	RasonYangs-MacBook-Pro.local
IP:         	192.168.31.248
Port:       	57567
Auth-User:  	1008
Auth-Realm: 	ws.aicc.test

Call-ID:    	ukzjREwCPvgy02I5ky4DKifrPktSOcwy
User:       	1007@192.168.31.248
Contact:    	"1007" <sip:1007@192.168.31.248:64459;ob>
Agent:      	Telephone 1.6
Status:     	Registered(UDP)(unknown) EXP(2026-08-13 12:50:54) EXPSECS(109)
Ping-Status:	Unreachable
Ping-Time:	0.00
Host:       	RasonYangs-MacBook-Pro.local

Total items returned: 2
`

func TestParseRegistrations(t *testing.T) {
	got := parseRegistrations(liveRegistrations)
	if len(got) != 2 {
		t.Fatalf("parsed %d registrations, want 2: %+v", len(got), got)
	}

	if got[0].Extension != "1008" {
		t.Errorf("first extension = %q, want 1008 without the domain", got[0].Extension)
	}
	if !got[0].IsReachable {
		t.Error("a reachable endpoint was reported as unreachable")
	}

	if got[1].Extension != "1007" {
		t.Errorf("second extension = %q", got[1].Extension)
	}
	if got[1].IsReachable {
		t.Error("an endpoint failing its keepalive was reported as reachable: this is exactly the crashed-tab case")
	}
}

func TestParseRegistrationsHandlesEmptyListing(t *testing.T) {
	if got := parseRegistrations("Total items returned: 0\n"); len(got) != 0 {
		t.Errorf("parsed %+v from an empty listing", got)
	}
	if got := parseRegistrations(""); len(got) != 0 {
		t.Errorf("parsed %+v from empty output", got)
	}
}

func TestRegistrationsDefaultsToTheInternalProfile(t *testing.T) {
	a, c := newTestAdapter()
	c.reply = liveRegistrations

	if _, err := a.Registrations(""); err != nil {
		t.Fatalf("Registrations() error = %v", err)
	}
	if c.last() != "sofia status profile internal reg" {
		t.Errorf("command = %q", c.last())
	}
}
