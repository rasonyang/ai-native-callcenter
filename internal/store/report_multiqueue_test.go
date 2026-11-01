// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// The queue reports on calls that passed through two queues (#283). The row
// carries one queue and the reports group by it, so the owner's decision
// fixes what a two-queue call counts as: the first queue the caller joined,
// with that stay's wait. A call answered in sales and transferred to support
// is a sales call that waited sales' time; support's report does not see it,
// and its wait in support is in the journey, not in any average.
//
// One consequence worth stating: a caller sales timed out on who then gave up
// in support is abandoned (the missed reason names how the call ended), and
// is counted as an abandoned sales call.
//
// The rows go through the CDR assembler, as the switch's calls do, so this is
// the assembler's reading of the stays and the reports' reading of the rows
// together. Needs AICC_TEST_DATABASE_URL; skips without it.

type signallingLedger struct {
	*store.LedgerStore
	inserted chan uuid.UUID
}

func (l *signallingLedger) InsertCDR(ctx context.Context, cdr store.CDR) error {
	err := l.LedgerStore.InsertCDR(ctx, cdr)
	l.inserted <- cdr.CallID
	return err
}

type queueNames map[string]uuid.UUID

func (q queueNames) QueueIDByName(_ context.Context, name string) (uuid.UUID, bool) {
	id, ok := q[name]
	return id, ok
}

// shape is one way a call goes through the queues; wantQueue/wantWait is the
// row decision 1 gives it.
type shape struct {
	snap      telephony.Snapshot
	wantQueue string
	wantWait  int
	answered  bool
	abandoned bool
	twoQueues bool
}

func tp(t time.Time) *time.Time { return &t }

func caller(start, end time.Time) telephony.PartySnapshot {
	return telephony.PartySnapshot{PartyID: uuid.Must(uuid.NewV7()), ChannelID: "caller-" + uuid.NewString(),
		Role: telephony.RoleOriginator, Number: "+8613900001111",
		CreatedAt: start, AnsweredAt: tp(start), ReleasedAt: tp(end), ReleaseCause: "NORMAL_CLEARING"}
}

// agent is a delivery to an agent: answered and bridged to the caller from
// answered to released, or rung and never picked up when answered is zero.
func agent(callerChan, number string, id uuid.UUID, created, answered, released time.Time) telephony.PartySnapshot {
	p := telephony.PartySnapshot{PartyID: uuid.Must(uuid.NewV7()), ChannelID: number + "-" + uuid.NewString(),
		Role: telephony.RoleTarget, Number: number, AgentID: &id,
		CreatedAt: created, ReleasedAt: tp(released), ReleaseCause: "NO_ANSWER"}
	if !answered.IsZero() {
		p.AnsweredAt = tp(answered)
		p.ReleaseCause = "NORMAL_CLEARING"
		p.Bridges = []telephony.BridgeSpan{{OtherChannelID: callerChan, StartedAt: answered, EndedAt: released}}
	}
	return p
}

func TestQueueReportsCountATwoQueueCallUnderItsFirstQueue(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "multiqueue"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ledger := st.Ledger()
	sales, support := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	inserted := make(chan uuid.UUID, 1)
	asm := telephony.NewCDRAssembler(&signallingLedger{LedgerStore: ledger, inserted: inserted},
		queueNames{"sales": sales, "support": support}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	wei, ben, carol := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	origin := time.Date(2026, time.October, 20, 9, 0, 0, 0, time.UTC)
	sec := func(t time.Time, s int) time.Time { return t.Add(time.Duration(s) * time.Second) }

	// Sixty calls, five shapes, waits that vary call to call so an average
	// taken over the wrong stay cannot land on the right number by chance.
	var calls []shape
	for i := range 60 {
		s := origin.Add(time.Duration(i) * 7 * time.Minute)
		w1, w2 := 3+i%17, 20+i%23 // sales' wait, support's wait
		var sh shape
		switch i % 5 {
		case 0: // answered in sales, transferred, answered in support
			c := caller(s, sec(s, 2+w1+60+1+w2+90))
			bridged := sec(s, 2+w1)
			back := sec(bridged, 60)
			sh = shape{wantQueue: "sales", wantWait: w1, answered: true, twoQueues: true}
			sh.snap = telephony.Snapshot{
				Parties: []telephony.PartySnapshot{c,
					agent(c.ChannelID, "1008", wei, sec(bridged, -2), bridged, back),
					agent(c.ChannelID, "1007", ben, sec(back, 1+w2-3), sec(back, 1+w2), *c.ReleasedAt)},
				QueueStays: []telephony.QueueFacts{
					{Name: "sales", JoinedAt: sec(s, 2), BridgedAt: bridged, LeftAt: back, Cause: "Terminated"},
					{Name: "support", JoinedAt: sec(back, 1), BridgedAt: sec(back, 1+w2), LeftAt: *c.ReleasedAt, Cause: "Terminated"},
				},
			}
		case 1: // answered in sales, transferred, gave up in support
			c := caller(s, sec(s, 2+w1+60+1+w2))
			bridged := sec(s, 2+w1)
			back := sec(bridged, 60)
			sh = shape{wantQueue: "sales", wantWait: w1, answered: true, twoQueues: true}
			sh.snap = telephony.Snapshot{
				Parties: []telephony.PartySnapshot{c,
					agent(c.ChannelID, "1008", wei, sec(bridged, -2), bridged, back),
					agent(c.ChannelID, "1007", ben, sec(back, 5), time.Time{}, *c.ReleasedAt)},
				QueueStays: []telephony.QueueFacts{
					{Name: "sales", JoinedAt: sec(s, 2), BridgedAt: bridged, LeftAt: back, Cause: "Terminated"},
					{Name: "support", JoinedAt: sec(back, 1), LeftAt: *c.ReleasedAt, Cause: "Cancel", CancelReason: "BREAK_OUT"},
				},
			}
		case 2: // sales timed out on them, they gave up in support
			timeout := 30 + w1
			c := caller(s, sec(s, 2+timeout+1+w2))
			sh = shape{wantQueue: "sales", wantWait: timeout, abandoned: true, twoQueues: true}
			sh.snap = telephony.Snapshot{
				Parties: []telephony.PartySnapshot{c,
					agent(c.ChannelID, "1009", carol, sec(s, 4), time.Time{}, sec(s, 19))},
				QueueStays: []telephony.QueueFacts{
					{Name: "sales", JoinedAt: sec(s, 2), LeftAt: sec(s, 2+timeout), Cause: "Timeout", CancelReason: "TIMEOUT"},
					{Name: "support", JoinedAt: sec(s, 3+timeout), LeftAt: *c.ReleasedAt, Cause: "Cancel"},
				},
			}
		case 3: // support only, answered
			c := caller(s, sec(s, 2+w2+120))
			sh = shape{wantQueue: "support", wantWait: w2, answered: true}
			sh.snap = telephony.Snapshot{
				Parties: []telephony.PartySnapshot{c,
					agent(c.ChannelID, "1007", ben, sec(s, w2), sec(s, 2+w2), *c.ReleasedAt)},
				QueueStays: []telephony.QueueFacts{
					{Name: "support", JoinedAt: sec(s, 2), BridgedAt: sec(s, 2+w2), LeftAt: *c.ReleasedAt, Cause: "Terminated"},
				},
			}
		default: // sales only, hung up almost at once
			c := caller(s, sec(s, 5))
			sh = shape{wantQueue: "sales", wantWait: 3, abandoned: true}
			sh.snap = telephony.Snapshot{
				Parties: []telephony.PartySnapshot{c},
				QueueStays: []telephony.QueueFacts{
					{Name: "sales", JoinedAt: sec(s, 2), LeftAt: sec(s, 5), Cause: "Cancel"},
				},
			}
		}
		sh.snap.CallID = uuid.Must(uuid.NewV7())
		sh.snap.CallType = events.CallTypeInbound
		sh.snap.State = telephony.CallEnded
		sh.snap.CreatedAt = s
		sh.snap.EndedAt = sh.snap.Parties[0].ReleasedAt
		sh.snap.Queue = sh.snap.QueueStays[len(sh.snap.QueueStays)-1]

		asm.CallFinished(sh.snap)
		select {
		case got := <-inserted:
			if got != sh.snap.CallID {
				t.Fatalf("call %d: wrote %s, want %s", i, got, sh.snap.CallID)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("call %d was never written", i)
		}
		calls = append(calls, sh)
	}

	// Each row is what decision 1 says, and the journey keeps both queues.
	ids := map[string]uuid.UUID{"sales": sales, "support": support}
	for i, sh := range calls {
		row, err := ledger.GetCDR(ctx, sh.snap.CallID)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if row.QueueID == nil || *row.QueueID != ids[sh.wantQueue] || row.QueueWaitSec != sh.wantWait {
			t.Errorf("call %d: queue %v wait %d; want %s and %d s", i, row.QueueID, row.QueueWaitSec, sh.wantQueue, sh.wantWait)
		}
		queueLegs := 0
		for _, leg := range row.Legs {
			if leg.Kind == "QUEUE" {
				queueLegs++
			}
			if leg.DurationSec < 0 {
				t.Errorf("call %d: leg %s %s lasted %d s", i, leg.Kind, leg.Label, leg.DurationSec)
			}
		}
		if want := len(sh.snap.QueueStays); queueLegs != want {
			t.Errorf("call %d: %d QUEUE legs in the journey, want %d", i, queueLegs, want)
		}
		if got := row.Status == store.CDRStatusAnswered; got != sh.answered {
			t.Errorf("call %d: status %s, answered %v wanted", i, row.Status, sh.answered)
		}
	}

	type totals struct {
		calls, answered, abandoned, withinSLA, maxWait, waitSum int
	}
	want := map[uuid.UUID]*totals{sales: {}, support: {}}
	all := totals{}
	for _, sh := range calls {
		for _, agg := range []*totals{want[ids[sh.wantQueue]], &all} {
			agg.calls++
			agg.waitSum += sh.wantWait
			agg.maxWait = max(agg.maxWait, sh.wantWait)
			if sh.answered {
				agg.answered++
				if sh.wantWait <= 20 {
					agg.withinSLA++
				}
			}
			if sh.abandoned {
				agg.abandoned++
			}
		}
	}
	if want[support].calls != 12 {
		t.Fatalf("fixture: support should be first for 12 calls, is for %d", want[support].calls)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

	from, to := origin.Add(-time.Hour), origin.Add(24*time.Hour)
	byQueue, err := ledger.ReportByQueue(ctx, from, to)
	if err != nil {
		t.Fatalf("ReportByQueue: %v", err)
	}
	if len(byQueue) != 2 {
		t.Fatalf("ReportByQueue returned %d queues, want 2: %+v", len(byQueue), byQueue)
	}
	for _, got := range byQueue {
		w := want[*got.QueueID]
		if w == nil {
			t.Fatalf("a report row for queue %v, which no call was first in", got.QueueID)
		}
		if got.TotalCalls != w.calls || got.AnsweredCalls != w.answered || got.AbandonedCalls != w.abandoned ||
			got.AnsweredWithinSLA != w.withinSLA || got.MaxWaitSec != w.maxWait ||
			!near(got.AvgWaitSec, float64(w.waitSum)/float64(w.calls)) {
			t.Errorf("queue %v: got %+v, want %+v (avg %.3f)", got.QueueID, got, *w, float64(w.waitSum)/float64(w.calls))
		}
	}

	overview, err := ledger.ReportOverview(ctx, from, to, nil)
	if err != nil {
		t.Fatalf("ReportOverview: %v", err)
	}
	if overview.TotalCalls != all.calls || overview.QueueCalls != all.calls ||
		overview.AnsweredCalls != all.answered || overview.AbandonedCalls != all.abandoned ||
		overview.AnsweredWithinSLA != all.withinSLA || !near(overview.AvgWaitSec, float64(all.waitSum)/float64(all.calls)) {
		t.Errorf("overview = %+v, want %+v", overview, all)
	}
	// Filtered to support, the overview is support's own callers: none of the
	// thirty-six that reached support second.
	onlySupport, err := ledger.ReportOverview(ctx, from, to, &support)
	if err != nil {
		t.Fatalf("ReportOverview(support): %v", err)
	}
	ws := want[support]
	if onlySupport.TotalCalls != ws.calls || onlySupport.AbandonedCalls != 0 ||
		!near(onlySupport.AvgWaitSec, float64(ws.waitSum)/float64(ws.calls)) {
		t.Errorf("support overview = %+v, want %d calls, none abandoned, avg wait %.3f",
			onlySupport, ws.calls, float64(ws.waitSum)/float64(ws.calls))
	}
}
