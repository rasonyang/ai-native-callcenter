// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"
	"time"
)

// Verbatim from the switch during the VC-S12-01 rerun on 2026-08-23, header
// row and +OK trailer included. Typed from memory it would agree with whatever
// this parser got wrong; copied from the machine it does not.
const liveMemberListing = `queue|instance_id|uuid|session_uuid|cid_number|cid_name|system_epoch|joined_epoch|rejoined_epoch|bridge_epoch|abandoned_epoch|base_score|skill_score|serving_agent|serving_system|state|score
support-en|single_box|01a02c57-ead9-76ba-9579-d0d63dcfda1f|01a02c57-a227-710d-834d-ee6a713f5a16|18688886669|18688886669|1787450335|1787450354|0|0|0|0|0|agent-wei|single_box|Trying|50
+OK
`

func TestTheSwitchsOwnMemberListingIsRead(t *testing.T) {
	members := parseQueueMembers(liveMemberListing)
	if len(members) != 1 {
		t.Fatalf("read %d members, want 1: %+v", len(members), members)
	}
	m := members[0]

	if m.Queue != "support-en" {
		t.Errorf("queue = %q", m.Queue)
	}
	// session_uuid, not the member's own uuid: the caller's channel is what
	// every leg raised for them points back to.
	if m.ChannelID != "01a02c57-a227-710d-834d-ee6a713f5a16" {
		t.Errorf("channelId = %q, want the caller's session uuid", m.ChannelID)
	}
	if m.Number != "18688886669" {
		t.Errorf("number = %q", m.Number)
	}
	if want := time.Unix(1787450354, 0).UTC(); !m.JoinedAt.Equal(want) {
		t.Errorf("joinedAt = %s, want the switch's joined_epoch %s", m.JoinedAt, want)
	}
	if !m.IsWaiting() {
		t.Errorf("state %q did not count as waiting; a caller an agent is being "+
			"rung for has still reached nobody", m.State)
	}
}

func TestOnlyCallersWhoHaveReachedNobodyCountAsWaiting(t *testing.T) {
	for state, want := range map[string]bool{
		"Waiting": true, "Trying": true, "waiting": true,
		"Answered": false, "Abandoned": false, "": false,
	} {
		if got := (QueueMember{State: state}).IsWaiting(); got != want {
			t.Errorf("state %q counted as waiting = %v, want %v", state, got, want)
		}
	}
}

// A row this parser cannot read honestly is dropped. The alternative is a
// caller whose wait began now, which reads as a fresh call and quietly
// improves the queue's service level for having lost track of them.
func TestARowWithNoUsableJoinTimeIsDroppedRatherThanInvented(t *testing.T) {
	cases := map[string]string{
		"no join time":     `support-en|single_box|u|chan|18600000000|n|1787450335|0|0|0|0|0|0|a|single_box|Waiting|50`,
		"unparseable":      `support-en|single_box|u|chan|18600000000|n|1787450335|x|0|0|0|0|0|a|single_box|Waiting|50`,
		"short row":        `support-en|single_box|u|chan|18600000000`,
		"no session id":    `support-en|single_box|u||18600000000|n|1787450335|1787450354|0|0|0|0|0|a|single_box|Waiting|50`,
		"header echoed":    `queue|instance_id|uuid|session_uuid|cid_number|cid_name|system_epoch|joined_epoch|rejoined_epoch|bridge_epoch|abandoned_epoch|base_score|skill_score|serving_agent|serving_system|state|score`,
		"just the trailer": `+OK`,
	}
	for name, line := range cases {
		if got := parseQueueMembers(line); len(got) != 0 {
			t.Errorf("%s: read %+v, want nothing", name, got)
		}
	}
}

// uuid_getvar's three answers, captured from the switch on 2026-08-23 against
// a live caller's channel mid-call. Written from the documentation this parser
// would have agreed with whatever it got wrong; these are what the switch
// actually said.
func TestWhatTheSwitchAnswersForAChannelVariable(t *testing.T) {
	for reply, want := range map[string]string{
		"01a02c97-ae6b-773a-a2ea-a5359f0f313a": "01a02c97-ae6b-773a-a2ea-a5359f0f313a", // set
		"en":                                   "en",
		"95001":                                "95001",
		"_undef_":                              "", // set on no call: the switch's word for nothing
		"-ERR No such channel!":                "", // the channel is gone
		"":                                     "",
		"  en  ":                               "en",
	} {
		if got := parseChannelVariable(reply); got != want {
			t.Errorf("parseChannelVariable(%q) = %q, want %q", reply, got, want)
		}
	}
}
