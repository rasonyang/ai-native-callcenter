// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// The row a session-limit call leaves in cdrs, written the way the running
// stack writes it: the bot's recorder and the human path's assembler, both
// into one real LedgerStore (#230). The unit tests read the bot's provisional
// row from a fake; on a transfer the real stack replaces that row.
//
// Set AICC_TEST_DATABASE_URL (see internal/testdb) and these run; without it
// they skip.

// signallingLedger is the real ledger, telling the test when the assembler's
// write (which runs on its own goroutine) has landed.
type signallingLedger struct {
	*store.LedgerStore
	inserted chan uuid.UUID
}

func (l *signallingLedger) InsertCDR(ctx context.Context, cdr store.CDR) error {
	err := l.LedgerStore.InsertCDR(ctx, cdr)
	l.inserted <- cdr.CallID
	return err
}

func sessionLimitLedger(t *testing.T) (*store.LedgerStore, *telephony.CDRAssembler, chan uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "sessionlimit"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ledger := st.Ledger()
	inserted := make(chan uuid.UUID, 16)
	queueID := uuid.Must(uuid.NewV7())
	asm := telephony.NewCDRAssembler(&signallingLedger{LedgerStore: ledger, inserted: inserted},
		queueNames{"support": queueID}, nil, discard())
	return ledger, asm, inserted, queueID
}

type queueNames map[string]uuid.UUID

func (q queueNames) QueueIDByName(_ context.Context, name string) (uuid.UUID, bool) {
	id, ok := q[name]
	return id, ok
}

func waitForHumanWrite(t *testing.T, inserted chan uuid.UUID, callID uuid.UUID) {
	t.Helper()
	select {
	case got := <-inserted:
		if got != callID {
			t.Fatalf("the human path wrote %s, want %s", got, callID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the human path never wrote the call")
	}
}

// sessionLimitSnapshot is the switch's view of a caller the bot ran for 65 s
// and handed to the support queue at its time limit. With an agent, they
// answer 15 s into the wait and talk until the end; without one, the caller
// waits until the end and leaves. stamped says aicc_bot_sec was written,
// which only the handover does.
func sessionLimitSnapshot(callID uuid.UUID, base, end time.Time, cause, summary string,
	hasAgent, stamped bool) telephony.Snapshot {

	at := func(sec int) *time.Time { v := base.Add(time.Duration(sec) * time.Second); return &v }
	flowID := uuid.Must(uuid.NewV7())
	snap := telephony.Snapshot{
		CallID:    callID,
		CallType:  events.CallTypeInbound,
		Language:  "zh",
		CreatedAt: base,
		EndedAt:   &end,
		UserData:  map[string]any{"crmTicket": "T-0042 / 工单"},
		Parties: []telephony.PartySnapshot{
			{
				PartyID: uuid.Must(uuid.NewV7()), ChannelID: "chan-caller", Role: telephony.RoleOriginator,
				State: telephony.PartyReleased, Number: "13800138000", OtherNumber: "95012",
				CreatedAt: base, AnsweredAt: at(0), ReleasedAt: &end, ReleaseCause: cause,
			},
			{
				PartyID: uuid.Must(uuid.NewV7()), ChannelID: "chan-bot", Role: telephony.RoleTarget,
				State: telephony.PartyReleased, Number: "95012", IsBotLeg: true,
				CreatedAt: base, AnsweredAt: at(0), ReleasedAt: at(65), ReleaseCause: "NORMAL_CLEARING",
				Bridges: []telephony.BridgeSpan{{OtherChannelID: "chan-caller", StartedAt: base, EndedAt: *at(65)}},
			},
		},
		Bot: telephony.BotShare{
			FlowID: &flowID, FlowSlug: "novanet_support", DID: "95012",
			Summary: summary, Reason: "SESSION_LIMIT", IsStamped: stamped,
		},
	}
	if !stamped {
		return snap
	}
	snap.Bot.Sec = 65
	snap.Bot.Queue = "support"
	snap.Queue = telephony.QueueFacts{Name: "support", JoinedAt: *at(66)}
	if !hasAgent {
		snap.Queue.LeftAt = end
		snap.Queue.Cause = "Cancel"
		return snap
	}
	agentID := uuid.Must(uuid.NewV7())
	snap.Queue.BridgedAt = *at(81)
	snap.Queue.AgentID = &agentID
	snap.Parties = append(snap.Parties, telephony.PartySnapshot{
		PartyID: uuid.Must(uuid.NewV7()), ChannelID: "chan-agent", Role: telephony.RoleTarget,
		State: telephony.PartyReleased, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008",
		CreatedAt: *at(70), AnsweredAt: at(81), ReleasedAt: &end, ReleaseCause: "NORMAL_CLEARING",
		Bridges: []telephony.BridgeSpan{{OtherChannelID: "chan-caller", StartedAt: *at(81), EndedAt: end}},
	})
	return snap
}

// A call the time limit handed to a person is the human path's row, whichever
// write lands first: its hangupCause is how the whole call ended (the caller
// leg's release cause), never SESSION_LIMIT, and the reason is
// userData.botReason beside the summary. Option B of #230: nothing about how
// the row is written changes; this pins what it is.
func TestASessionLimitTransferKeepsTheFinalLegsCause(t *testing.T) {
	ledger, asm, inserted, queueID := sessionLimitLedger(t)
	ctx := context.Background()

	summaries := []struct{ name, text string }{
		{"en", "The call reached its time limit; the platform transferred it."},
		{"zh", "通话已达到时长上限，由系统转接人工。"},
		{"none", ""},
	}
	for _, cause := range []string{"NORMAL_CLEARING", "ORIGINATOR_CANCEL", "USER_BUSY", ""} {
		for _, summary := range summaries {
			for _, isHumanFirst := range []bool{false, true} {
				order := "bot first"
				if isHumanFirst {
					order = "human first"
				}
				name := fmt.Sprintf("cause %q/summary %s/%s", cause, summary.name, order)
				t.Run(name, func(t *testing.T) {
					callID := uuid.Must(uuid.NewV7())
					base := time.Now().Add(-2 * time.Minute).Truncate(time.Millisecond)
					// The caller's leg outlives the bot's session. The bot's row
					// ends when it finishes (now), so the human path's ending
					// lies ahead of it, which a late bot write must not undo.
					end := time.Now().Add(90 * time.Second).Truncate(time.Millisecond)
					hasAgent := cause == "NORMAL_CLEARING"
					snap := sessionLimitSnapshot(callID, base, end, cause, summary.text, hasAgent, true)

					recorder := newCallRecorder(callID, base, nil)
					recorder.markSessionLimit()
					recorder.markTransferred(summary.text, hangupCauseSessionLimit)
					recorder.markHandedOver(queueID)
					facts := testFacts()
					facts.userData = map[string]any{"crmTicket": "T-0042 / 工单"}

					writeBot := func() { recorder.finish(ledger, facts, discard()) }
					writeHuman := func() {
						asm.CallFinished(snap)
						waitForHumanWrite(t, inserted, callID)
					}
					if isHumanFirst {
						writeHuman()
						writeBot()
					} else {
						writeBot()
						if got, err := ledger.GetCDR(ctx, callID); err != nil || got.HangupCause != "SESSION_LIMIT" {
							t.Fatalf("bot's provisional row: cause=%q err=%v, want SESSION_LIMIT", got.HangupCause, err)
						}
						writeHuman()
					}

					got, err := ledger.GetCDR(ctx, callID)
					if err != nil {
						t.Fatalf("GetCDR() error = %v", err)
					}
					if !got.EndedAt.Equal(end) {
						t.Errorf("endedAt = %v, want the human path's %v: the earlier ending won", got.EndedAt, end)
					}
					if got.HangupCause != cause || got.HangupCause == "SESSION_LIMIT" {
						t.Errorf("hangupCause = %q, want the caller leg's %q", got.HangupCause, cause)
					}
					if got.IsContained {
						t.Error("a session-limit transfer was marked contained")
					}
					if got.QueueID == nil || *got.QueueID != queueID {
						t.Errorf("queueId = %v, want %v", got.QueueID, queueID)
					}
					if got.UserData["crmTicket"] != "T-0042 / 工单" {
						t.Errorf("userData lost the call's own data: %v", got.UserData)
					}
					// The reason rides only beside a summary, which is why the
					// limit's own summary is never empty (sessionLimitSummary).
					wantReason, wantSummary := any(nil), any(nil)
					if summary.text != "" {
						wantReason, wantSummary = "SESSION_LIMIT", summary.text
					}
					if got.UserData["botReason"] != wantReason || got.UserData["botSummary"] != wantSummary {
						t.Errorf("botReason=%v botSummary=%v, want %v and %v",
							got.UserData["botReason"], got.UserData["botSummary"], wantReason, wantSummary)
					}
					if isAnswered := got.Status == store.CDRStatusAnswered; isAnswered != hasAgent {
						t.Errorf("status = %s with an agent answering = %v", got.Status, hasAgent)
					}
				})
			}
		}
	}
}

// A call the bot itself ended at the limit is the bot's row and says
// SESSION_LIMIT: through the closing phase, and when the caller hangs up
// during the handover line, before the handover stamped aicc_bot_sec. The
// human path sees the same call end and declines to write it.
func TestASessionLimitTheBotEndedKeepsSessionLimit(t *testing.T) {
	ledger, asm, inserted, _ := sessionLimitLedger(t)
	ctx := context.Background()
	summary := "通话已达到时长上限，由系统转接人工。"

	for _, tc := range []struct {
		name string
		// isTransferDecided: the limit chose the queue and the caller hung up
		// before the handover line ended.
		isTransferDecided bool
	}{
		{"closing phase", false},
		{"caller gone before the handover", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callID := uuid.Must(uuid.NewV7())
			base := time.Now().Add(-70 * time.Second).Truncate(time.Millisecond)
			recorder := newCallRecorder(callID, base, nil)
			recorder.markSessionLimit()
			snapSummary := ""
			if tc.isTransferDecided {
				recorder.markTransferred(summary, hangupCauseSessionLimit)
				snapSummary = summary
			} else {
				recorder.markHangup()
			}
			recorder.finish(ledger, testFacts(), discard())

			// The switch's view of the same call, ending a moment later: the
			// summary and reason were stamped at the decision, aicc_bot_sec
			// never was.
			end := time.Now().Add(time.Second).Truncate(time.Millisecond)
			snap := sessionLimitSnapshot(callID, base, end, "NORMAL_CLEARING", snapSummary, false, false)
			if !tc.isTransferDecided {
				snap.Bot.Reason = ""
			}
			asm.CallFinished(snap)
			select {
			case got := <-inserted:
				t.Fatalf("the human path wrote %s; a call the bot ended is the bot's row", got)
			case <-time.After(300 * time.Millisecond):
			}

			got, err := ledger.GetCDR(ctx, callID)
			if err != nil {
				t.Fatalf("GetCDR() error = %v", err)
			}
			if got.HangupCause != "SESSION_LIMIT" || got.IsContained || got.QueueID != nil {
				t.Errorf("cause=%q contained=%v queue=%v, want SESSION_LIMIT, not contained, no queue",
					got.HangupCause, got.IsContained, got.QueueID)
			}
			wantReason := any(nil)
			if tc.isTransferDecided {
				wantReason = "SESSION_LIMIT"
			}
			if got.UserData["botReason"] != wantReason {
				t.Errorf("botReason = %v, want %v", got.UserData["botReason"], wantReason)
			}
		})
	}
}
