// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// Issue #515: each check that read a duration or an order from two wall-clock
// stamps reads a monotonic clock (or, for C10's listing, what the driver had
// read) instead. Each test below gives a record whose wall-clock stamps show a
// step and whose monotonic values do not, and the reverse: the verdict follows
// the monotonic values. Where it fits, the reading is #506's failing ring:
// 2814 ms on the wall clock, 4864 ms on the monotonic clock, a net step of
// -2050 ms.

// step506 is #506's net step of the wall clock inside one interval.
const step506 = -2050.0

// testSimStart is when the hand-built runs' simulator started, the zero of
// their monotonic timeline; testRunStart is the same for the driver.
var (
	testSimStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	testRunStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

// onBothClocks fills a hand-built dialog's monotonic values from its wall-clock
// stamps, as a simulator whose wall clock did not step records them.
func onBothClocks(d *calleesim.DialogRecord) {
	ms := func(t time.Time) float64 { return durMs(t.Sub(testSimStart)) }
	d.InviteAfterStartMs = ms(d.InviteAt)
	if d.Final != nil {
		d.Final.AfterStartMs = ms(d.Final.SentAt)
		d.Final.AfterInviteMs = d.Final.AfterStartMs - d.InviteAfterStartMs
	}
	if d.End != nil {
		d.End.AfterStartMs = ms(d.End.At)
		d.End.AfterInviteMs = d.End.AfterStartMs - d.InviteAfterStartMs
	}
}

// onBothClocksAll does that for every dialog of a hand-built run, and fills
// the driver's monotonic values of its direct calls and facts the same way.
func onBothClocksAll(v *RunView, f *RefusalFacts) {
	for i := range v.Dialogs {
		onBothClocks(&v.Dialogs[i])
	}
	for i := range v.Direct {
		if v.Direct[i].Dialog != nil {
			onBothClocks(v.Direct[i].Dialog)
		}
		v.Direct[i].PlacedAfterRunStartMs = durMs(v.Direct[i].Placed.Sub(testRunStart))
	}
	if f != nil {
		f.HolderOnBotAfterRunStartMs = nil
		if f.HolderOnBotAt != nil {
			f.HolderOnBotAfterRunStartMs = ptr(durMs(f.HolderOnBotAt.Sub(testRunStart)))
		}
	}
}

// moveWall moves a wall-clock stamp by ms and leaves the monotonic values: a
// step of the wall clock between this event and the ones it is compared with.
func moveWall(t *time.Time, ms float64) {
	*t = t.Add(time.Duration(ms * float64(time.Millisecond)))
}

// verdictOf returns the failures of the one check whose text holds key, and
// fails the test when no such check ran.
func verdictOf(t *testing.T, checks, failures []string, key string) []string {
	t.Helper()
	ran := false
	for _, c := range checks {
		ran = ran || strings.Contains(c, key)
	}
	if !ran {
		t.Fatalf("no check holds %q: %q", key, checks)
	}
	var out []string
	for _, f := range failures {
		if strings.Contains(f, key) {
			out = append(out, f)
		}
	}
	return out
}

// clockCase is one row of a test below: the interval or order as the wall
// clock reads it and as the monotonic clock does, and the verdict wanted.
type clockCase struct {
	name       string
	wall, mono float64
	pass       bool
}

func runClockCases(t *testing.T, cases []clockCase, judge func(t *testing.T, tc clockCase) (failures []string)) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failures := judge(t, tc)
			if got := len(failures) == 0; got != tc.pass {
				t.Fatalf("passed = %v, want %v: %v", got, tc.pass, failures)
			}
			if !tc.pass && !strings.Contains(failures[0], "wall clock") {
				t.Errorf("the failure does not give the wall clock's reading: %s", failures[0])
			}
		})
	}
}

// C04: the two INVITEs at least the retry interval apart, less the INVITE
// slack (59 s), read on the simulator's monotonic timeline.
func TestC04GapReadsTheMonotonicTimeline(t *testing.T) {
	const key = "the two INVITEs are at least the 60 s retry interval apart"
	runClockCases(t, []clockCase{
		{"below the bound", 60000, 58999, false},
		{"the bound", 60000, 59000, true},
		{"the gap measured on 2026-11-09", 61168, 61168, true},
		{"wall 57950 ms, monotonic 60000 ms (a -2050 ms step)", 60000 + step506, 60000, true},
		{"wall 60000 ms, monotonic 57950 ms", 60000, 60000 + step506, false},
	}, func(t *testing.T, tc clockCase) []string {
		f := c04Fixture(0)
		d1, d2 := &f.dialogs[0], &f.dialogs[1]
		d2.InviteAt = d1.InviteAt.Add(time.Duration(tc.wall * float64(time.Millisecond)))
		d2.InviteAfterStartMs = d1.InviteAfterStartMs + tc.mono
		res := f.assess()
		a := res.Attempts[1]
		if a.InviteGapMs == nil || *a.InviteGapMs != tc.mono || a.InviteGapWallMs == nil || *a.InviteGapWallMs != tc.wall {
			t.Errorf("inviteGapMs %v, inviteGapWallMs %v; want %v and %v", a.InviteGapMs, a.InviteGapWallMs, tc.mono, tc.wall)
		}
		return verdictOf(t, res.Checks, res.Failures, key)
	})
}

// C10's control: its second INVITE at or after the moment C10's retry was due
// (the callee's 486 plus 60 s), less the INVITE slack, read on the simulator's
// monotonic timeline. wall and mono are the INVITE's lateness.
func TestC10ControlReadsTheMonotonicTimeline(t *testing.T) {
	const key = "the control's second INVITE"
	runClockCases(t, []clockCase{
		{"a second before the retry was due", 500, -1000, true},
		{"just over a second before", 500, -1001, false},
		{"as measured, +70 ms", 70, 70, true},
		{"wall -1550 ms, monotonic +500 ms (a -2050 ms step)", 500 + step506, 500, true},
		{"wall +500 ms, monotonic -1550 ms", 500, 500 + step506, false},
	}, func(t *testing.T, tc clockCase) []string {
		f := c10Run2Fixture(0, false)
		fin := f.dialogs[0].Final
		ctl := &f.control[1]
		ctl.InviteAt = fin.SentAt.Add(RetryIntervalSec*time.Second + time.Duration(tc.wall*float64(time.Millisecond)))
		ctl.InviteAfterStartMs = fin.AfterStartMs + durMs(RetryIntervalSec*time.Second) + tc.mono
		res := f.assess()
		return verdictOf(t, res.Checks, res.Failures, key)
	})
}

// C10: the simulator's record was taken after C10's retry came due, on the
// simulator's monotonic timeline. wall and mono are how long after.
func TestC10RecordReadAfterTheRetryReadsTheMonotonicTimeline(t *testing.T) {
	const key = "the simulator's record was read"
	runClockCases(t, []clockCase{
		{"as measured, 13.5 s after", 13500, 13500, true},
		{"at the due moment", 13500, 0, false},
		{"wall -500 ms, monotonic +1550 ms (a -2050 ms step)", 1550 + step506, 1550, true},
		{"wall +1550 ms, monotonic -500 ms", 1550, 1550 + step506, false},
	}, func(t *testing.T, tc clockCase) []string {
		f := c10Run2Fixture(0, false)
		fin := f.dialogs[0].Final
		f.readAt = fin.SentAt.Add(RetryIntervalSec*time.Second + time.Duration(tc.wall*float64(time.Millisecond)))
		f.takenMs = fin.AfterStartMs + durMs(RetryIntervalSec*time.Second) + tc.mono
		res := f.assess()
		return verdictOf(t, res.Checks, res.Failures, key)
	})
}

// C10's listing: at or after the callee's final response, and after the only
// INVITE. Driver and simulator are two processes, so neither is read on a
// clock: the driver posts the listing only after it has read attempt 1
// FINISHED, and the listing keeps what it read. A wall clock that puts the
// listing before the 486 (run 2's 268 ms, less a 2050 ms step) does not fail
// it; a listing whose record shows the attempt unfinished does, whatever the
// wall clock says.
func TestC10ListingIsHeldToWhatTheDriverRead(t *testing.T) {
	keys := []string{"listed at or after the callee's final response", "the only INVITE"}
	for _, tc := range []struct {
		name    string
		wallMs  float64 // the listing's PostedAt after the 486, on the wall clock
		spoil   func(f *c10Fixture)
		pass    bool
		wantMsg string
	}{
		{"wall: posted 268 ms after the 486, read FINISHED BUSY/USER_BUSY", 268, nil, true, ""},
		{"wall: posted 1782 ms before the 486 (a -2050 ms step), read FINISHED BUSY/USER_BUSY", 268 + step506, nil, true, ""},
		{"wall: posted 268 ms after, but the attempt read was still DIALING", 268, func(f *c10Fixture) {
			a := *f.cv.Listing.FirstAttempt
			a.State, a.Outcome, a.HangupCause, a.FinishedAt = "DIALING", nil, nil, nil
			f.cv.Listing.FirstAttempt = &a
		}, false, "read DIALING"},
		{"the attempt read finished with another cause", 268, func(f *c10Fixture) {
			a := *f.cv.Listing.FirstAttempt
			cause := "NORMAL_CLEARING"
			a.HangupCause = &cause
			f.cv.Listing.FirstAttempt = &a
		}, false, "BUSY/NORMAL_CLEARING"},
		{"the attempt read is not attempt 1", 268, func(f *c10Fixture) {
			a := *f.cv.Listing.FirstAttempt
			a.CallID = uuid.Must(uuid.NewV7())
			f.cv.Listing.FirstAttempt = &a
		}, false, "not attempt 1's"},
		{"the listing records no attempt", 268, func(f *c10Fixture) { f.cv.Listing.FirstAttempt = nil }, false, "records no attempt"},
		{"the simulator answered something else", 268, func(f *c10Fixture) { f.dialogs[0].Final.Status = 487 }, false, "sent 487"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := c10Run2Fixture(0, false)
			f.cv.Listing.PostedAt = f.dialogs[0].Final.SentAt.Add(time.Duration(tc.wallMs * float64(time.Millisecond)))
			if tc.spoil != nil {
				tc.spoil(f)
			}
			res := f.assess()
			for _, key := range keys {
				failures := verdictOf(t, res.Checks, res.Failures, key)
				if got := len(failures) == 0; got != tc.pass {
					t.Fatalf("%s: passed = %v, want %v: %v", key, got, tc.pass, failures)
				}
			}
			if !tc.pass {
				failures := verdictOf(t, res.Checks, res.Failures, keys[0])
				if !strings.Contains(failures[0], tc.wantMsg) || !strings.Contains(failures[0], "wall clock, not compared") {
					t.Errorf("the failure does not say what was read (%q) and the wall clock's reading: %s", tc.wantMsg, failures[0])
				}
			}
		})
	}
}

// The driver posts a listing only after it has read the contact's first
// attempt FINISHED, and keeps that attempt in the listing: the evidence
// TestC10ListingIsHeldToWhatTheDriverRead judges.
func TestTheDriverListsOnlyAfterReadingTheAttemptFinished(t *testing.T) {
	r := Regions[0]
	cs := caseByID(AllCases, "C10")
	number := r.Number(cs)
	campaignID := uuid.Must(uuid.NewV7())
	attempt := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: number,
		State: "DIALING", StartedAt: time.Date(2026, 11, 7, 1, 28, 17, 850e6, time.UTC)}
	contact := api.CampaignContact{Number: number, Status: "ACTIVE"}

	var mu sync.Mutex
	var log []string // the requests in the order the server answered them
	finished := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		a, ct := attempt, contact
		if finished {
			busy, cause := api.CampaignOutcome("BUSY"), "USER_BUSY"
			end := a.StartedAt.Add(2200 * time.Millisecond)
			next := end.Add(RetryIntervalSec * time.Second)
			a.State, a.Outcome, a.HangupCause, a.FinishedAt = attemptFinished, &busy, &cause, &end
			ct.Attempts, ct.LastOutcome, ct.NextAttemptAt = 1, &busy, &next
		}
		base := "/api/v1/campaigns/" + campaignID.String()
		var out any
		switch {
		case req.Method == http.MethodGet && req.URL.Path == base:
			out = api.Campaign{ID: campaignID}
		case req.Method == http.MethodGet && req.URL.Path == base+"/attempts":
			out = api.CampaignAttemptList{Items: []api.CampaignAttempt{a}, Total: 1}
			log = append(log, "attempts "+string(a.State))
		case req.Method == http.MethodGet && req.URL.Path == base+"/contacts":
			out = api.CampaignContactList{Items: []api.CampaignContact{ct}, Total: 1}
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/dnc-numbers":
			log = append(log, "list")
			w.WriteHeader(http.StatusCreated)
			return
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/dnc-numbers":
			out = api.DNCNumberList{Items: []api.DNCNumber{{Number: number}}, Total: 1}
		default:
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	d := &driver{c: &httpc{base: srv.URL, client: srv.Client()}, cfg: CampaignConfig{Cases: []Case{cs}}}
	d.regions = []*regionRun{{
		region: r, campaign: api.Campaign{ID: campaignID}, byNumber: map[string]Case{number: cs},
		afterFirst: map[string]api.CampaignContact{}, firstFinished: map[string]api.CampaignAttempt{}, listings: map[string]*Listing{},
	}}
	ctx := context.Background()
	step := func() {
		t.Helper()
		if err := d.observe(ctx); err != nil {
			t.Fatal(err)
		}
		if err := d.listMidRun(ctx); err != nil {
			t.Fatal(err)
		}
	}

	step() // the attempt is still ringing: nothing is listed
	mu.Lock()
	finished = true
	mu.Unlock()
	step() // it finished, with a retry due: listed once
	step() // and never again

	mu.Lock()
	defer mu.Unlock()
	want := []string{"attempts DIALING", "attempts FINISHED", "list", "attempts FINISHED"}
	if strings.Join(log, ", ") != strings.Join(want, ", ") {
		t.Fatalf("requests %q, want %q", log, want)
	}
	l := d.regions[0].listings[number]
	if l == nil || l.FirstAttempt == nil || l.ContactBefore == nil {
		t.Fatalf("listing %+v", l)
	}
	if l.FirstAttempt.CallID != attempt.CallID || l.FirstAttempt.State != attemptFinished ||
		deref(l.FirstAttempt.Outcome) != "BUSY" || deref(l.FirstAttempt.HangupCause) != "USER_BUSY" {
		t.Errorf("the listing records %+v as the attempt read before it", *l.FirstAttempt)
	}
	if l.ContactBefore.NextAttemptAt == nil || !l.ContactBefore.NextAttemptAt.Equal(l.NextAttemptAt) {
		t.Errorf("the listing records the contact %+v", *l.ContactBefore)
	}
	// What the driver recorded is what the check judges.
	cv := ContactView{Case: cs, Number: number, Attempts: []api.CampaignAttempt{*l.FirstAttempt}, Listing: l}
	dlg := calleesim.DialogRecord{Final: &calleesim.FinalRecord{Status: 486}}
	if ok, read := listedAfterFinal(cs, cv, []calleesim.DialogRecord{dlg}); !ok {
		t.Errorf("the driver's own listing does not hold the order: %s", read)
	}
}

// K3: the holds of R1 (the callee's BYE at 8000 ms, +/- 1000), R2 (the
// platform's BYE within 14000 ms) and R3 (within 6000 ms), read from the
// answer to the end on the simulator's monotonic clock.
func TestK3HoldsReadTheMonotonicClock(t *testing.T) {
	setHold := func(d *calleesim.DialogRecord, wall, mono float64) {
		d.End.At = d.Final.SentAt.Add(time.Duration(wall * float64(time.Millisecond)))
		d.End.AfterInviteMs = d.Final.AfterInviteMs + mono
		d.End.AfterStartMs = d.Final.AfterStartMs + mono
	}
	for _, call := range []struct {
		id, key string
		dialog  func(v *RunView) *calleesim.DialogRecord
		cases   []clockCase
	}{
		{"R1", "R1 (+12025550161): the callee's BYE came at its scripted 8000 ms", func(v *RunView) *calleesim.DialogRecord { return v.Direct[1].Dialog },
			[]clockCase{
				{"the lower bound", 8000, 7000, true},
				{"below it", 8000, 6999, false},
				{"the upper bound", 8000, 9000, true},
				{"above it", 8000, 9001, false},
				{"wall 5950 ms, monotonic 8000 ms (a -2050 ms step)", 8000 + step506, 8000, true},
				{"wall 8000 ms, monotonic 5950 ms", 8000, 8000 + step506, false},
			}},
		{"R2", "R2 (+12025550162): the platform's BYE came within 14000 ms", func(v *RunView) *calleesim.DialogRecord { return v.Direct[2].Dialog },
			[]clockCase{
				{"the bound", 1250, 14000, true},
				{"over it", 1250, 14001, false},
				{"wall 16050 ms, monotonic 14000 ms (a +2050 ms step)", 14000 - step506, 14000, true},
				{"wall 14000 ms, monotonic 16050 ms", 14000, 14000 - step506, false},
			}},
		{"R3", "R3 (+12025550163): the platform's BYE came within 6000 ms", func(v *RunView) *calleesim.DialogRecord { return &v.Dialogs[0] },
			[]clockCase{
				{"the bound", 1100, 6000, true},
				{"over it", 1100, 6001, false},
				{"#506's ring: wall 2814 ms, monotonic 4864 ms", 2814, 4864, true},
				{"wall 4864 ms, monotonic 6914 ms (a -2050 ms step)", 4864, 4864 - step506, false},
			}},
	} {
		t.Run(call.id, func(t *testing.T) {
			runClockCases(t, call.cases, func(t *testing.T, tc clockCase) []string {
				v, f := fakeK3(t, phonenum.FormE164)
				setHold(call.dialog(v), tc.wall, tc.mono)
				c, rows, _ := refusalAssess(v, f)
				for _, r := range rows {
					if r.ID == call.id && (r.HoldMs == nil || *r.HoldMs != tc.mono || r.HoldWallMs == nil || *r.HoldWallMs != tc.wall) {
						t.Errorf("row %s: answerToEndMs %v, answerToEndWallMs %v; want %v and %v", r.ID, r.HoldMs, r.HoldWallMs, tc.mono, tc.wall)
					}
				}
				return verdictOf(t, c.checks, c.failures, call.key)
			})
		})
	}
}

// K3's preconditions, the order the run was held to. The calls' events are
// read on the simulator's monotonic timeline, the driver's own moments on the
// driver's. In each case the wall clock moves one event across the other by a
// 2050 ms step and the monotonic values keep it where it was.
func TestK3PreconditionsReadTheMonotonicClocks(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		// move sets the event's wall stamp off by wall ms and its monotonic
		// value off by mono ms, from where fakeK3 put it.
		move       func(v *RunView, f *RefusalFacts, wall, mono float64)
		wall, mono float64
		pass       bool
	}{
		// The holder answered at 300 ms, R1's INVITE came at 1600 ms.
		{"holder answer: wall after R1's INVITE, monotonic before", "the holder was answered before the first refused INVITE",
			moveHolderAnswer, 1300 - step506, 0, true},
		{"holder answer: wall before R1's INVITE, monotonic after", "the holder was answered before the first refused INVITE",
			moveHolderAnswer, 0, 1300 - step506, false},
		// The driver saw the holder on the bot at 1000 ms and placed R1 at 1500 ms.
		{"holder on the bot: wall after R1 was placed, monotonic before", "the holder was on the bot before the first refused call was placed",
			moveHolderOnBot, 500 - step506, 0, true},
		{"holder on the bot: wall before R1 was placed, monotonic after", "the holder was on the bot before the first refused call was placed",
			moveHolderOnBot, 0, 500 - step506, false},
		// R1's callee hung up at 9900 ms, moved to 27000 ms; the holder ended at 28300 ms.
		{"last refused end: wall after the holder's end, monotonic before", "the holder was still up when the last refused call ended",
			moveR1End, 17100 - step506, 17100, true},
		{"last refused end: wall before the holder's end, monotonic after", "the holder was still up when the last refused call ended",
			moveR1End, 17100, 17100 - step506, false},
		// N1's INVITE came at 30000 ms, the holder ended at 28300 ms.
		{"N1's INVITE: wall before the holder's end, monotonic after", "the normal call was placed after the holder ended",
			moveN1Invite, step506, 0, true},
		{"N1's INVITE: wall after the holder's end, monotonic before", "the normal call was placed after the holder ended",
			moveN1Invite, 0, step506, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, f := fakeK3(t, phonenum.FormE164)
			tc.move(v, &f, tc.wall, tc.mono)
			c, _, _ := refusalAssess(v, f)
			failures := verdictOf(t, c.checks, c.failures, tc.key)
			if got := len(failures) == 0; got != tc.pass {
				t.Fatalf("passed = %v, want %v: %v", got, tc.pass, failures)
			}
		})
	}
}

func moveHolderAnswer(v *RunView, _ *RefusalFacts, wall, mono float64) {
	fin := v.Direct[0].Dialog.Final
	moveWall(&fin.SentAt, wall)
	fin.AfterInviteMs += mono
	fin.AfterStartMs += mono
}

func moveHolderOnBot(_ *RunView, f *RefusalFacts, wall, mono float64) {
	at := *f.HolderOnBotAt
	moveWall(&at, wall)
	f.HolderOnBotAt = &at
	*f.HolderOnBotAfterRunStartMs += mono
}

func moveR1End(v *RunView, _ *RefusalFacts, wall, mono float64) {
	end := v.Direct[1].Dialog.End
	moveWall(&end.At, wall)
	end.AfterInviteMs += mono
	end.AfterStartMs += mono
}

func moveN1Invite(v *RunView, _ *RefusalFacts, wall, mono float64) {
	d := v.Direct[3].Dialog
	moveWall(&d.InviteAt, wall)
	d.InviteAfterStartMs += mono
}

// K1's preconditions, read on the simulator's monotonic timeline. Each moves
// one event across another on the wall clock by a forward excursion of
// 5.8 s, the largest #515 records, and keeps it on the monotonic clock.
func TestK1PreconditionsReadTheMonotonicTimeline(t *testing.T) {
	const excursion = 5800.0
	for _, tc := range []struct {
		name, key  string
		move       func(v *RunView, wall, mono float64)
		wall, mono float64
		pass       bool
	}{
		// The last contact rang at 2 ms; the first contact answered at 5000 ms.
		{"last INVITE: wall after the first answer, monotonic before", "every INVITE before the first answer", moveContactInvite, excursion, 0, true},
		{"last INVITE: wall before the first answer, monotonic after", "every INVITE before the first answer", moveContactInvite, 0, excursion, false},
		// The second slot holder answered at 301 ms.
		{"holder answer: wall after the first contact answer, monotonic before", "both direct calls were answered", moveHolderAnswerK1, excursion, 0, true},
		{"holder answer: wall before the first contact answer, monotonic after", "both direct calls were answered", moveHolderAnswerK1, 0, excursion, false},
		// An OVERFLOW leg was hung up at 6530 ms; the first holder ended at 12300 ms.
		{"overflow end: wall after the holder's end, monotonic before", "both direct calls still held their slots", moveOverflowEnd, excursion, 0, true},
		{"overflow end: wall before the holder's end, monotonic after", "both direct calls still held their slots", moveOverflowEnd, 0, excursion, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := fakeK1(t, phonenum.FormE164)
			tc.move(v, tc.wall, tc.mono)
			c := overflowChecks(v)
			failures := verdictOf(t, c.checks, c.failures, tc.key)
			if got := len(failures) == 0; got != tc.pass {
				t.Fatalf("passed = %v, want %v: %v", got, tc.pass, failures)
			}
			if len(failures) > 0 && !strings.Contains(failures[0], "wall clock") {
				t.Errorf("the failure does not give the wall clock's reading: %s", failures[0])
			}
		})
	}
}

func moveContactInvite(v *RunView, wall, mono float64) {
	d := &v.Dialogs[2]
	moveWall(&d.InviteAt, wall)
	d.InviteAfterStartMs += mono
}

func moveHolderAnswerK1(v *RunView, wall, mono float64) {
	fin := v.Direct[1].Dialog.Final
	moveWall(&fin.SentAt, wall)
	fin.AfterStartMs += mono
	fin.AfterInviteMs += mono
}

func moveOverflowEnd(v *RunView, wall, mono float64) {
	end := v.Dialogs[1].End
	moveWall(&end.At, wall)
	end.AfterStartMs += mono
	end.AfterInviteMs += mono
}

// K2's peak of dialogs up at once is read on the simulator's monotonic
// timeline: a line handed straight on is not counted twice when a step of the
// wall clock puts the next INVITE before the end it followed, and two dialogs
// that overlapped are when the step makes them look apart.
func TestK2PeakReadsTheMonotonicTimeline(t *testing.T) {
	at := func(ms float64) time.Time { return testSimStart.Add(time.Duration(ms * float64(time.Millisecond))) }
	dialog := func(wallInvite, wallEnd, monoInvite, monoEnd float64) calleesim.DialogRecord {
		return calleesim.DialogRecord{
			InviteAt: at(wallInvite), InviteAfterStartMs: monoInvite,
			End: &calleesim.EndRecord{Method: calleesim.EndedWithCancel, At: at(wallEnd), AfterStartMs: monoEnd, AfterInviteMs: monoEnd - monoInvite},
		}
	}
	// Two lines up from 0 ms; the first ends at 8000 ms and a third dialog
	// takes its line 50 ms later on the monotonic clock.
	steady := []calleesim.DialogRecord{dialog(0, 8000, 0, 8000), dialog(5, 8005, 5, 8005)}
	for _, tc := range []struct {
		name  string
		third calleesim.DialogRecord
		peak  int
	}{
		{"wall 6000 ms (a -2050 ms step), monotonic 8050 ms: handed on", dialog(8050+step506, 16050+step506, 8050, 16050), 2},
		{"wall 8050 ms, monotonic 6000 ms: three at once", dialog(8050, 16050, 8050+step506, 16050+step506), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peak, unended := PeakConcurrent(append(append([]calleesim.DialogRecord{}, steady...), tc.third))
			if peak != tc.peak || unended != 0 {
				t.Fatalf("peak %d, unended %d; want %d and 0", peak, unended, tc.peak)
			}
		})
	}
}
