// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func atMs(ms int) time.Time     { return base.Add(time.Duration(ms) * time.Millisecond) }
func atMsPtr(ms int) *time.Time { t := atMs(ms); return &t }

// placingPhone is the originator of a call an agent's phone placed. The real
// switch auto-answers it (sip_auto_answer) between 0.3 and 1.2 s after the
// click, never at the instant the call was created, so a test cannot pass by
// accident on equal timestamps.
func placingPhone(agent *uuid.UUID, number string, autoAnswerMs int, released *time.Time, bridges ...BridgeSpan) PartySnapshot {
	return PartySnapshot{
		Role: RoleOriginator, Number: number, ExtensionNumber: number, AgentID: agent,
		ChannelID: "phone-" + number, CreatedAt: at(0), AnsweredAt: atMsPtr(autoAnswerMs),
		ReleasedAt: released, Bridges: bridges,
	}
}

// answeredAtCase is one shape of a call and what its row must say about when
// somebody on the far side answered.
type answeredAtCase struct {
	name       string
	snap       Snapshot
	wantAnswer *time.Time // nil: answeredAt must be absent
	wantStatus string
	wantBill   int
}

// checkAnsweredAt assembles the case and holds its row to the matrix, then to
// the invariant that makes the field mean one thing: on an agent-placed call,
// answeredAt is set exactly when a leg other than the placing phone answered.
func checkAnsweredAt(t *testing.T, tc answeredAtCase) {
	t.Helper()
	queues := staticQueues{"support": uuid.New(), "internal_queue": uuid.New()}
	cdr := newAssembler(&memoryLedger{}, queues).assemble(t.Context(), tc.snap)

	switch {
	case tc.wantAnswer == nil && !cdr.AnsweredAt.IsZero():
		t.Errorf("answeredAt = %v, want absent", cdr.AnsweredAt)
	case tc.wantAnswer != nil && !cdr.AnsweredAt.Equal(*tc.wantAnswer):
		t.Errorf("answeredAt = %v, want %v", cdr.AnsweredAt, *tc.wantAnswer)
	}
	if cdr.Status != tc.wantStatus {
		t.Errorf("status = %s, want %s", cdr.Status, tc.wantStatus)
	}
	if cdr.BillSec != tc.wantBill {
		t.Errorf("billSec = %d, want %d", cdr.BillSec, tc.wantBill)
	}

	originator, _ := split(tc.snap)
	if !isAgentPlaced(originator) {
		return
	}
	farSideAnswered := false
	for _, p := range tc.snap.Parties {
		if p.Role != RoleOriginator && p.AnsweredAt != nil {
			farSideAnswered = true
		}
	}
	if isSet := !cdr.AnsweredAt.IsZero(); isSet != farSideAnswered {
		t.Errorf("answeredAt set = %v but a non-originator leg answered = %v: on an "+
			"agent-placed call the two must agree", isSet, farSideAnswered)
	}
}

// An agent's phone auto-answers every call it places, so the originator's
// answer says nothing about the far end. answeredAt used to fall back to it
// whenever no leg was billed: a click-to-dial that never reached anybody was
// stored NO_ANSWER with billSec 0 and an answeredAt, and one that did reach an
// extension or a bot recorded the phone's own pick-up instead of theirs. The
// row contradicted itself (#330). These are the shapes the issue lists, and
// the control rows that must keep answeredAt exactly as it was.
func TestAnAgentsCallRecordsTheFarSidesAnswerNotTheirOwnPhones(t *testing.T) {
	t.Parallel()
	agent1 := uuid.MustParse("00000000-0000-0000-0000-000000001008")
	agent2 := uuid.MustParse("00000000-0000-0000-0000-000000001002")
	flowID := uuid.MustParse("00000000-0000-0000-0000-000000009501")
	bot := BotShare{DID: "95001", FlowID: &flowID, FlowSlug: "novanet_support"}
	trunk := func(createdMs int, answered, released *time.Time, cause string) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
			CreatedAt: atMs(createdMs), AnsweredAt: answered, ReleasedAt: released, ReleaseCause: cause,
		}
	}
	extension := func(agent *uuid.UUID, createdMs int, answered *time.Time, released *time.Time, bridges ...BridgeSpan) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: agent,
			ChannelID: "phone-1002", CreatedAt: atMs(createdMs), AnsweredAt: answered,
			ReleasedAt: released, Bridges: bridges,
		}
	}
	talk := func(from, to int) BridgeSpan { return BridgeSpan{StartedAt: at(from), EndedAt: at(to)} }

	for _, tc := range []answeredAtCase{
		{
			name: "1 click-to-dial, gateway down: no trunk leg, the agent hangs up at 0.8 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atMsPtr(800),
				Parties: []PartySnapshot{placingPhone(&agent1, "1008", 500, atMsPtr(800))},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "2 click-to-dial, trunk 480 CALL_REJECTED at 4 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(4),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(4)),
					trunk(600, nil, atPtr(4), "CALL_REJECTED"),
				},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "3 click-to-dial, trunk USER_BUSY at 2 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(2),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(2)),
					trunk(600, nil, atPtr(2), causeUserBusy),
				},
			},
			wantStatus: store.CDRStatusBusy,
		},
		{
			name: "4a click-to-dial, trunk rings out NO_ANSWER at 30 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(30)),
					trunk(600, nil, atPtr(30), "NO_ANSWER"),
				},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// Early media is not an answer: the trunk leg never got one.
			name: "4b click-to-dial, early media (183) then UNALLOCATED_NUMBER",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(3),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 700, atPtr(3)),
					trunk(800, nil, atPtr(3), "UNALLOCATED_NUMBER"),
				},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "5 click-to-dial to a customer, answered at 8 s, talked 22 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(30), talk(8, 30)),
					func() PartySnapshot {
						p := trunk(600, atPtr(8), atPtr(30), "")
						p.Bridges = []BridgeSpan{talk(8, 30)}
						return p
					}(),
				},
			},
			wantAnswer: atPtr(8), wantStatus: store.CDRStatusAnswered, wantBill: 22,
		},
		{
			name: "6 click-to-dial, trunk answered but codec mismatch, no bridge",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(10),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(10)),
					trunk(600, atPtr(2), atPtr(10), ""),
				},
			},
			wantAnswer: atPtr(2), wantStatus: store.CDRStatusNoAnswer, wantBill: 8,
		},
		{
			name: "7 click-to-dial 1008 to 1002, which never rings",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(5),
				Parties: []PartySnapshot{placingPhone(&agent1, "1008", 500, atPtr(5))},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "8 click-to-dial 1008 to 1002, rings 6 s, answered, 40 s talk: 1002's answer, not 1008's",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(46),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(46), talk(6, 46)),
					extension(&agent2, 600, atPtr(6), atPtr(46), talk(6, 46)),
				},
			},
			wantAnswer: atPtr(6), wantStatus: store.CDRStatusAnswered,
		},
		{
			// The originator's answer equals the bridge here, so this row
			// cannot tell the two apart; it pins that a keypad dial is not
			// disturbed by the rule.
			name: "9 keypad dial 1008 to 1002, answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(20),
				Parties: []PartySnapshot{
					func() PartySnapshot {
						p := placingPhone(&agent1, "1008", 0, atPtr(20), talk(6, 20))
						p.AnsweredAt = atPtr(6)
						return p
					}(),
					extension(&agent2, 200, atPtr(6), atPtr(20), talk(6, 20)),
				},
			},
			wantAnswer: atPtr(6), wantStatus: store.CDRStatusAnswered,
		},
		{
			name: "10 1008 to 1002 with nobody signed in at 1002 (extension only), answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(20),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(20), talk(5, 20)),
					extension(nil, 600, atPtr(5), atPtr(20), talk(5, 20)),
				},
			},
			wantAnswer: atPtr(5), wantStatus: store.CDRStatusAnswered,
		},
		{
			name: "11 click-to-dial 1008 to 95001, the bot answers at 1.5 s and hangs up at 40 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(40), Bot: bot,
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(40)),
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: atMs(600), AnsweredAt: atMsPtr(1500), ReleasedAt: atPtr(40),
					},
				},
			},
			wantAnswer: atMsPtr(1500), wantStatus: store.CDRStatusAnswered,
		},
		{
			name: "12 click-to-dial 1008 to 95001, no bot slot: the bot leg never answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(5), Bot: bot,
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(5)),
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: atMs(600), ReleasedAt: atPtr(5), ReleaseCause: "ORIGINATOR_CANCEL",
					},
				},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "13 signed-out 1008 (extension only) click-to-dial, trunk 480",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(4),
				Parties: []PartySnapshot{
					placingPhone(nil, "1008", 500, atPtr(4)),
					trunk(600, nil, atPtr(4), "CALL_REJECTED"),
				},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "14 control: INBOUND, bot answers, abandoned in the queue after 25 s: the caller's answer",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInbound, CreatedAt: at(0), EndedAt: atPtr(26),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "13800138000", ChannelID: "caller",
						CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(26),
					},
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: at(1), AnsweredAt: atMsPtr(1500), ReleasedAt: atPtr(10),
					},
				},
				QueueStays: []QueueFacts{{Name: "support", JoinedAt: at(10), LeftAt: at(26), Cause: "ABANDONED"}},
			},
			wantAnswer: atPtr(1), wantStatus: store.CDRStatusNoAnswer, wantBill: 25,
		},
		{
			name: "15 control: AI outbound, customer answers, BOT_BRIDGE_UNCONFIRMED after 30 s (#313)",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(30),
				Bot: BotShare{DID: "95099", FlowID: &flowID, FlowSlug: "lead_gen"},
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
					ReleaseCause: "BOT_BRIDGE_UNCONFIRMED",
				}},
			},
			wantAnswer: atPtr(0), wantStatus: store.CDRStatusAnswered, wantBill: 30,
		},
		{
			name: "16 control: INTERNAL on a dialplan-spawned leg keeps the originator's answer",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(10),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ChannelID: "spawned",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(10),
				}},
			},
			wantAnswer: atPtr(0), wantStatus: store.CDRStatusAnswered,
		},
		{
			// Owner-confirmed rule 3: the queue extension's own answer is the
			// switch's, not a person's, and nobody bills an internal call.
			name: "17 handset dial into a queue extension the queue answered and nobody took",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(20),
				Parties:    []PartySnapshot{placingPhone(&agent1, "1008", 500, atPtr(20))},
				QueueStays: []QueueFacts{{Name: "internal_queue", JoinedAt: atMs(500), LeftAt: at(20), Cause: "ABANDONED"}},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkAnsweredAt(t, tc)
		})
	}
}

// The shapes a real switch produces that the tidy matrix does not: legs that
// arrive in an unlucky order, a leg that never reports its end, and a hang-up
// right after the answer. answeredAt has to come out of each one the same way
// (#330).
func TestAnAgentsCallAnswerSurvivesLegsThatArriveOutOfOrder(t *testing.T) {
	t.Parallel()
	agent1 := uuid.MustParse("00000000-0000-0000-0000-000000001008")
	agent2 := uuid.MustParse("00000000-0000-0000-0000-000000001002")
	callee := func(createdMs int, answered, released *time.Time) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: &agent2,
			ChannelID: "phone-1002", CreatedAt: atMs(createdMs), AnsweredAt: answered, ReleasedAt: released,
		}
	}
	dialledTrunk := func(createdMs int, answered, released *time.Time, cause string) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: "18688886660", ChannelID: "trunk-" + cause,
			CreatedAt: atMs(createdMs), AnsweredAt: answered, ReleasedAt: released, ReleaseCause: cause,
		}
	}

	for _, tc := range []answeredAtCase{
		{
			// The callee's leg is created before the placing phone's own
			// auto-answer: the second leg outran its caller.
			name: "a callee leg created before the originator's auto-answer",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 1200, atPtr(30)),
					callee(300, atPtr(6), atPtr(30)),
				},
			},
			wantAnswer: atPtr(6), wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// A retry: the first number rang out, the second answered. The
			// answer is the second leg's, and it is billed from there.
			name: "two dialled legs where only the second answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(30)),
					dialledTrunk(600, nil, atPtr(5), "NO_ANSWER"),
					dialledTrunk(5500, atPtr(9), atPtr(30), "NORMAL_CLEARING"),
				},
			},
			wantAnswer: atPtr(9), wantStatus: store.CDRStatusNoAnswer, wantBill: 21,
		},
		{
			// Between extensions the earliest far-side answer wins even
			// when the later leg is listed first.
			name: "two far-side legs answered, the later one listed first",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(30)),
					callee(900, atPtr(9), atPtr(30)),
					callee(600, atPtr(6), atPtr(30)),
				},
			},
			wantAnswer: atPtr(6), wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// The call ended by the switch: the answered leg never reported
			// its release.
			name: "a far-side leg with an answer and no release",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, nil),
					callee(600, atPtr(6), nil),
				},
			},
			wantAnswer: atPtr(6), wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// The far end answered and the agent hung up 0.2 s later: it was
			// answered, billed from that second, for less than a second.
			name: "the agent hangs up 0.2 s after the far end answers",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atMsPtr(8200),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atMsPtr(8200)),
					dialledTrunk(600, atPtr(8), atMsPtr(8200), "ORIGINATOR_CANCEL"),
				},
			},
			wantAnswer: atPtr(8), wantStatus: store.CDRStatusNoAnswer, wantBill: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkAnsweredAt(t, tc)
		})
	}
}

// The stored row, not only the assembled one: CallFinished is the path that
// writes it, on its own goroutine, and a ledger turns a zero answeredAt into
// NULL. Row 1 must reach the ledger without an answer, rows 8 and 11 with the
// far side's, not the phone's (#330).
func TestTheStoredRowOfAnAgentsCallCarriesTheFarSidesAnswer(t *testing.T) {
	t.Parallel()
	agent1 := uuid.MustParse("00000000-0000-0000-0000-000000001008")
	agent2 := uuid.MustParse("00000000-0000-0000-0000-000000001002")
	flowID := uuid.MustParse("00000000-0000-0000-0000-000000009501")

	for _, tc := range []struct {
		name       string
		snap       Snapshot
		wantAnswer *time.Time
		wantStatus string
	}{
		{
			name: "gateway down: no answer is stored",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atMsPtr(800),
				Parties: []PartySnapshot{placingPhone(&agent1, "1008", 500, atMsPtr(800))},
			},
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			name: "1002 answers at 6 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal, CreatedAt: at(0), EndedAt: atPtr(46),
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(46), BridgeSpan{StartedAt: at(6), EndedAt: at(46)}),
					{
						Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: &agent2, ChannelID: "phone-1002",
						CreatedAt: atMs(600), AnsweredAt: atPtr(6), ReleasedAt: atPtr(46),
						Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(46)}},
					},
				},
			},
			wantAnswer: atPtr(6), wantStatus: store.CDRStatusAnswered,
		},
		{
			name: "the bot answers an agent's call to 95001 at 1.5 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(40),
				Bot: BotShare{DID: "95001", FlowID: &flowID, FlowSlug: "novanet_support"},
				Parties: []PartySnapshot{
					placingPhone(&agent1, "1008", 500, atPtr(40)),
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: atMs(600), AnsweredAt: atMsPtr(1500), ReleasedAt: atPtr(40),
					},
				},
			},
			wantAnswer: atMsPtr(1500), wantStatus: store.CDRStatusAnswered,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := finishCall(t, tc.snap)
			if len(rows) != 1 {
				t.Fatalf("wrote %d rows, want 1", len(rows))
			}
			got := rows[0]
			switch {
			case tc.wantAnswer == nil && !got.AnsweredAt.IsZero():
				t.Errorf("stored answeredAt = %v, want zero (NULL)", got.AnsweredAt)
			case tc.wantAnswer != nil && !got.AnsweredAt.Equal(*tc.wantAnswer):
				t.Errorf("stored answeredAt = %v, want %v", got.AnsweredAt, *tc.wantAnswer)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("stored status = %s, want %s", got.Status, tc.wantStatus)
			}
		})
	}
}
