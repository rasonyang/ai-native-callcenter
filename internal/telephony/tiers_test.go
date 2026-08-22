// SPDX-License-Identifier: Apache-2.0

package telephony

import "testing"

// A queue is called what it is called. The domain suffix was ours — the switch
// stores and reports literally what it is told — and in a single-tenant product
// it said nothing except which address this host had at the time. That is the
// part that hurt: when the machine moved from one address to another, every
// tier written under the old name became something converge could neither match
// nor delete, and it outlived the queue it named (C1).
func TestAQueueNameCarriesNoDomain(t *testing.T) {
	a := NewAdapter(nil, "aicc.demo")
	if got := a.QueueName("support-en"); got != "support-en" {
		t.Fatalf("QueueName = %q, want the name itself", got)
	}
	if got := a.bareQueueName("support-en"); got != "support-en" {
		t.Errorf("bareQueueName(bare) = %q", got)
	}
	// Rows written before the suffix was dropped are still out there, under
	// whatever address the host had then. Reading them as the queue they always
	// were is what lets converge reconcile them away; leaving them qualified is
	// what let one survive a change of address in the first place.
	for _, legacy := range []string{"support-en@aicc.demo", "support-en@192.168.31.176"} {
		if got := a.bareQueueName(legacy); got != "support-en" {
			t.Errorf("bareQueueName(%q) = %q, want support-en — a tier converge cannot name is a tier it cannot remove", legacy, got)
		}
	}
}

// A tier left behind by an address this host no longer has still reads as the
// queue it names, so converge can see it and take it away. This is C1 turned
// from a delete that has to cope with stale names into names that do not go
// stale: the row below outlived the queue it named because nothing upstream
// could tell it was support-en at all.
func TestATierUnderAnOldAddressIsStillThatQueue(t *testing.T) {
	a := NewAdapter(nil, "192.168.31.55")
	out := "queue|agent|state|level|position\n" +
		"support-en@192.168.31.176|agent-wei|Ready|1|2\n" +
		"support-zh|agent-ben|Ready|1|1\n" +
		"+OK\n"
	byAgent := map[string][]string{}
	for _, tier := range parseTiers(out) {
		byAgent[tier.Agent] = append(byAgent[tier.Agent], a.bareQueueName(tier.Queue))
	}
	if got := byAgent["agent-wei"]; len(got) != 1 || got[0] != "support-en" {
		t.Errorf("the stale tier reads as %v, want [support-en] — converge cannot remove what it cannot name", got)
	}
	if got := byAgent["agent-ben"]; len(got) != 1 || got[0] != "support-zh" {
		t.Errorf("the current tier reads as %v, want [support-zh]", got)
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
