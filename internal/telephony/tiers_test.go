// SPDX-License-Identifier: Apache-2.0

package telephony

import "testing"

// The domain suffix is applied by this application, not by mod_callcenter:
// aicc_xml.lua names every queue "<name>@<domain>" when it renders the
// configuration and QueueName does the same for tier commands. The switch
// stores and reports literally what it was told. So the adapter owns both
// directions, and a round trip has to land back where it started or a
// reconcile would remove and re-add every tier for ever.
func TestQueueNameRoundTripsThroughTheDomain(t *testing.T) {
	a := NewAdapter(nil, "aicc.demo")
	if got := a.QueueName("support-en"); got != "support-en@aicc.demo" {
		t.Fatalf("QueueName = %q", got)
	}
	if got := a.bareQueueName("support-en@aicc.demo"); got != "support-en" {
		t.Errorf("bareQueueName = %q, want support-en", got)
	}
	// Already bare is left alone.
	if got := a.bareQueueName("support-en"); got != "support-en" {
		t.Errorf("bareQueueName(bare) = %q", got)
	}
	// Another domain's queue is not ours, and must not be collapsed into
	// looking like one of ours.
	if got := a.bareQueueName("support-en@elsewhere.test"); got != "support-en@elsewhere.test" {
		t.Errorf("bareQueueName(foreign) = %q, want it left qualified", got)
	}
}

func TestParseTiersSkipsTheHeaderAndTheStatusLine(t *testing.T) {
	out := "queue|agent|state|level|position\n" +
		"support-en@aicc.demo|agent-wei|Ready|1|2\n" +
		"malformed|row\n" +
		"+OK\n"
	tiers := parseTiers(out)
	if len(tiers) != 1 {
		t.Fatalf("parsed %d tiers, want 1: %+v", len(tiers), tiers)
	}
	got := tiers[0]
	if got.Queue != "support-en@aicc.demo" || got.Agent != "agent-wei" ||
		got.Level != 1 || got.Position != 2 {
		t.Errorf("tier = %+v", got)
	}
}
