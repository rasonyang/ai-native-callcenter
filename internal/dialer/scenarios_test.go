// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// script is how one contact's calls go, attempt by attempt, and what the
// ledger must say about the contact when they are done. The expectations are
// written out by hand from the design's tables (sections 7.1 and 7.2), not
// computed by the code under test.
type script struct {
	name string
	// ends[i] is how attempt i ends; after the last one it repeats.
	ends []string
	// failSync: the placer refuses these attempts (by index) before a channel.
	failSync map[int]bool

	status   campaign.ContactStatus
	attempts int // counted against maxAttempts
	failures int // counted against maxSystemRetries
	rows     int // attempt rows
	last     campaign.Outcome
}

var scripts = []script{
	{name: "answered", ends: []string{endAnswered},
		status: campaign.ContactDone, attempts: 1, rows: 1, last: campaign.OutcomeAnswered},
	{name: "never answers", ends: []string{endNoAnswer},
		status: campaign.ContactExhausted, attempts: 3, rows: 3, last: campaign.OutcomeNoAnswer},
	{name: "busy then answers", ends: []string{endBusy, endAnswered},
		status: campaign.ContactDone, attempts: 2, rows: 2, last: campaign.OutcomeAnswered},
	{name: "always rejects", ends: []string{endRejected},
		status: campaign.ContactExhausted, attempts: 3, rows: 3, last: campaign.OutcomeRejected},
	{name: "unreachable", ends: []string{endInvalid},
		status: campaign.ContactInvalidNumber, attempts: 0, rows: 1, last: campaign.OutcomeInvalidNumber},
	{name: "originate always fails", failSync: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true},
		status: campaign.ContactExhausted, failures: 5, rows: 5, last: campaign.OutcomeOriginateFailed},
	{name: "trunk fails once", ends: []string{endPlacement, endAnswered},
		status: campaign.ContactDone, attempts: 1, failures: 1, rows: 2, last: campaign.OutcomeAnswered},
	{name: "hangs up within a second", ends: []string{endHangup1s},
		status: campaign.ContactDone, attempts: 1, rows: 1, last: campaign.OutcomeAnswered},
	{name: "bot fails", ends: []string{endBotFailed},
		status: campaign.ContactDone, attempts: 1, rows: 1, last: campaign.OutcomeBotFailed},
}

func TestARealisticMixOfOutcomesIsReconciledCallByCall(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Mix", func(s *camSpec) { s.max = 40 })
	const n = 540
	specs := numbers("40", n)
	contacts := r.importContacts(c, specs)
	byNumber := map[string]script{}
	for i, s := range specs {
		byNumber[s.number] = scripts[i%len(scripts)]
	}

	r.placer.fail = func(req outbound.AIDialRequest) error {
		// Called with the placer's lock held.
		s := byNumber[req.To]
		if s.failSync[r.placer.placedToLocked(req.To)+r.placer.failures[req.To]] {
			if r.placer.failures == nil {
				r.placer.failures = map[string]int{}
			}
			r.placer.failures[req.To]++
			return errors.New("bgapi send failed")
		}
		return nil
	}
	r.start(c)

	for step := 0; step < 400; step++ {
		r.endAll(func(number string) string {
			s := byNumber[number]
			i := r.placer.placedTo(number) - 1 // the call that is ending; outside the placer, so take its lock
			return s.ends[min(i, len(s.ends)-1)]
		})
		next := r.nextDue(c)
		if next.IsZero() && len(r.placer.liveCalls()) == 0 {
			break
		}
		if next.After(r.clock.Now()) && len(r.placer.liveCalls()) == 0 {
			r.at(next)
		}
	}
	if live := r.placer.liveCalls(); len(live) != 0 {
		t.Fatalf("%d calls still live at the end", len(live))
	}
	r.noViolations()

	rows := map[uuid.UUID][]campaign.Attempt{}
	for _, a := range r.attempts(c) {
		rows[a.ContactID] = append(rows[a.ContactID], a)
	}
	failed := 0
	for _, ct := range r.contacts(c) {
		s := byNumber[ct.Number]
		got := rows[ct.ID]
		if ct.Status != s.status || ct.Attempts != s.attempts || ct.SystemFailures != s.failures ||
			len(got) != s.rows || ct.LastOutcome != s.last || ct.NextAttemptAt != nil {
			failed++
			if failed <= 5 {
				t.Errorf("%s (%s): status %s attempts %d failures %d rows %d last %s next %v; want %s %d %d %d %s none",
					ct.Number, s.name, ct.Status, ct.Attempts, ct.SystemFailures, len(got), ct.LastOutcome, ct.NextAttemptAt,
					s.status, s.attempts, s.failures, s.rows, s.last)
			}
		}
		for _, a := range got {
			if a.State != campaign.AttemptFinished {
				t.Errorf("%s has an attempt still %s", ct.Number, a.State)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d contacts disagree with the oracle", failed, len(contacts))
	}
	if got := r.campaignNow(c); got.Status != campaign.StatusCompleted {
		t.Errorf("campaign is %s, want COMPLETED when every contact is settled", got.Status)
	}
}

// placedToLocked is placedTo for callers that hold the placer's lock already
// or are called from inside it.
func (p *fakePlacer) placedToLocked(number string) int {
	n := 0
	for _, c := range p.calls {
		if c.Req.To == number {
			n++
		}
	}
	return n
}

// nextDue is the earliest time any contact of the campaign is due after now.
func (r *rig) nextDue(c campaign.Campaign) time.Time {
	r.t.Helper()
	var at *time.Time
	if err := r.st.Pool.QueryRow(context.Background(),
		`SELECT min(next_attempt_at) FROM campaign_contacts
		  WHERE campaign_id = $1 AND status = 'ACTIVE' AND next_attempt_at > $2`, c.ID, r.clock.Now()).Scan(&at); err != nil {
		r.t.Fatal(err)
	}
	if at == nil {
		return time.Time{}
	}
	return *at
}

func TestKillingTheActorMidCampaignDialsNoContactTwiceAndLosesNoAttempt(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Restart", func(s *camSpec) { s.max = 6 })
	specs := numbers("41", 30)
	r.importContacts(c, specs)
	r.start(c)
	for _, l := range r.placer.liveCalls()[:2] {
		r.end(l, endAnswered)
	}
	r.settle()
	live := r.placer.liveCalls()
	if len(live) != 6 {
		t.Fatalf("setup: %d live", len(live))
	}
	vanished, ended := live[0], live[1]

	r.stopDialer()
	// While no dialer runs: one call ends (its CDR finishes the attempt in the
	// ledger's own transaction) and one vanishes without a trace.
	r.end(ended, endNoAnswer)
	r.placer.drop(vanished.Req.CallID)
	totalBefore := r.placer.total()
	r.startDialer()
	r.settle()

	// The vanished attempt is UNKNOWN, counts against maxAttempts, and is not
	// redialled at once.
	a, err := r.st.Attempts().Get(context.Background(), vanished.Req.CallID)
	if err != nil || a.State != campaign.AttemptFinished || a.Outcome != campaign.OutcomeUnknown {
		t.Fatalf("vanished attempt = %+v, %v; want FINISHED/UNKNOWN", a, err)
	}
	ct := r.contactOf(c, vanished.Req.To)
	if ct.Attempts != 1 || ct.Status != campaign.ContactActive || ct.NextAttemptAt == nil ||
		ct.NextAttemptAt.Before(r.clock.Now().Add(23*time.Hour)) {
		t.Errorf("vanished contact = %+v, want 1 attempt and a retry a day away", ct)
	}
	if r.placer.placedTo(vanished.Req.To) != 1 {
		t.Error("the vanished contact was redialled at once")
	}
	// The calls that live on are still in flight: the cap holds across the restart.
	if got := len(r.placer.liveCalls()); got != 6 {
		t.Errorf("%d live after the restart, want the cap 6 (4 adopted, 2 new)", got)
	}
	if r.placer.total() != totalBefore+2 {
		t.Errorf("%d calls placed since the restart, want 2 to refill the cap", r.placer.total()-totalBefore)
	}

	// Run it out.
	for step := 0; step < 200; step++ {
		r.endAll(func(string) string { return endAnswered })
		if next := r.nextDue(c); len(r.placer.liveCalls()) == 0 {
			if next.IsZero() {
				break
			}
			r.at(next)
		}
	}
	r.noViolations()
	rows := map[uuid.UUID]int{}
	for _, a := range r.attempts(c) {
		rows[a.ContactID]++
		if a.State != campaign.AttemptFinished {
			t.Errorf("attempt %s is %s at the end", a.ID, a.State)
		}
	}
	for _, ct := range r.contacts(c) {
		if got := r.placer.placedTo(ct.Number); got != rows[ct.ID] {
			t.Errorf("%s: %d placements but %d attempts: an originate without a row, or a row without an originate",
				ct.Number, got, rows[ct.ID])
		}
	}
}

func TestAnAttemptWithACDRInTheLedgerIsFinishedFromItAfterARestart(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Ledger", func(s *camSpec) { s.max = 2 })
	r.importContacts(c, numbers("42", 2))
	r.start(c)
	call := r.placer.liveCalls()[0]
	r.stopDialer()
	// The CDR exists but the attempt was never finished from it (a crash
	// between the two): put the attempt back as the crash would have left it.
	if err := r.st.Ledger().InsertCDR(context.Background(), cdrFor(call.Req.CallID, call.Req.To, endBusy, call.At, r.clock.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Pool.Exec(context.Background(),
		`UPDATE campaign_attempts SET state='DIALING', outcome=NULL, finished_at=NULL, is_touched=false, hangup_cause=NULL, talk_sec=NULL
		  WHERE call_id=$1`, call.Req.CallID); err != nil {
		t.Fatal(err)
	}
	r.placer.drop(call.Req.CallID)
	r.startDialer()
	r.settle()
	a, err := r.st.Attempts().Get(context.Background(), call.Req.CallID)
	if err != nil || a.State != campaign.AttemptFinished || a.Outcome != campaign.OutcomeBusy {
		t.Errorf("attempt = %+v, %v; want FINISHED/BUSY from the CDR, not UNKNOWN", a, err)
	}
}

func TestADialingAttemptWithNeitherCDRNorCallIsReconciledAfterRingTimeoutPlusThirtySeconds(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Backstop", func(s *camSpec) { s.max = 2; s.ring = 30 })
	r.importContacts(c, numbers("43", 2))
	r.start(c)
	live := r.placer.liveCalls()
	lost, talking := live[0], live[1]
	r.placer.drop(lost.Req.CallID) // its events were lost: no CDR will come

	r.tick(59 * time.Second)
	if a, _ := r.st.Attempts().Get(context.Background(), lost.Req.CallID); a.State != campaign.AttemptDialing {
		t.Fatalf("reconciled after 59 s: %+v", a)
	}
	r.tick(2 * time.Second)
	a, _ := r.st.Attempts().Get(context.Background(), lost.Req.CallID)
	if a.State != campaign.AttemptFinished || a.Outcome != campaign.OutcomeUnknown {
		t.Errorf("lost attempt = %+v, want UNKNOWN past ring timeout + 30 s", a)
	}
	// A call that is still live is somebody talking: left alone, however long.
	r.tick(20 * time.Minute)
	if a, _ := r.st.Attempts().Get(context.Background(), talking.Req.CallID); a.State != campaign.AttemptDialing {
		t.Errorf("a live call was reconciled away: %+v", a)
	}
	if p := r.progress(c); p.InFlight != 1 {
		t.Errorf("in flight = %d, want the one live call", p.InFlight)
	}
}

func TestOneNumberInTwoCampaignsIsDialledByOneAtATimeAndHeldForTheFrequencyWindow(t *testing.T) {
	r := newRig(t, opts{})
	a := r.newCampaign("A", func(s *camSpec) { s.max = 10 })
	b := r.newCampaign("B", func(s *camSpec) { s.max = 10 })
	shared := numbers("44", 4) // dialled by both
	r.importContacts(a, shared)
	r.importContacts(b, append(append([]contactSpec{}, shared...), numbers("45", 2)...))
	r.start(a)
	r.start(b)

	// A holds the four shared numbers; B dials only its own two.
	isShared := map[string]bool{}
	for _, s := range shared {
		isShared[s.number] = true
	}
	for _, l := range r.placer.liveCalls() {
		if l.Req.CampaignID == b.ID && isShared[l.Req.To] {
			t.Fatalf("B dialled %s while A has it in flight", l.Req.To)
		}
	}
	if got := len(r.placer.liveCalls()); got != 6 {
		t.Fatalf("%d live, want 4 from A and 2 from B", got)
	}
	r.noViolations()

	// A reaches two of them (NO_ANSWER and BUSY count against maxAttempts) and
	// fails to reach the other two (our own trunk): those are not a touch.
	touchedAt := r.clock.Now()
	var callsA []placed
	for _, l := range r.placer.liveCalls() {
		if l.Req.CampaignID == a.ID {
			callsA = append(callsA, l)
		}
	}
	r.end(callsA[0], endNoAnswer)
	r.end(callsA[1], endBusy)
	r.end(callsA[2], endPlacement)
	r.end(callsA[3], endPlacement)
	r.settle()

	for _, l := range callsA[:2] {
		ct := r.contactOf(b, l.Req.To)
		want := touchedAt.Add(24 * time.Hour)
		if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(want) {
			t.Errorf("B's %s due %v, want %v: the touch plus the frequency window", l.Req.To, ct.NextAttemptAt, want)
		}
		if r.placer.placedTo(l.Req.To) != 1 {
			t.Errorf("%s was dialled again within the window", l.Req.To)
		}
	}
	// The ORIGINATE_FAILED ones do not hold B: it dials them at once.
	for _, l := range callsA[2:] {
		if r.placer.placedTo(l.Req.To) != 2 {
			t.Errorf("%s: %d placements, want 2: A's failure to place does not hold B back", l.Req.To, r.placer.placedTo(l.Req.To))
		}
	}
	r.noViolations()

	// A day later B may.
	r.endAll(func(string) string { return endAnswered })
	r.at(touchedAt.Add(24 * time.Hour))
	for _, l := range callsA[:2] {
		if r.placer.placedTo(l.Req.To) != 2 {
			t.Errorf("%s not dialled by B after the window", l.Req.To)
		}
	}
	r.noViolations()
}

func TestADoNotCallNumberAddedWhileRunningSkipsTheQueuedAndSparesTheRinging(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("DNC", func(s *camSpec) { s.max = 2 })
	specs := numbers("46", 6)
	r.importContacts(c, specs)
	r.start(c)
	live := r.placer.liveCalls()
	ringing, other := live[0], live[1]
	queued := specs[5].number

	for _, n := range []string{ringing.Req.To, queued} {
		if _, _, err := r.st.DNC().Add(context.Background(), n, "MANUAL", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	r.settle()

	if ct := r.contactOf(c, queued); ct.Status != campaign.ContactDoNotCall || ct.LastOutcome != campaign.OutcomeDNCSkipped {
		t.Errorf("queued contact = %s/%s, want DNC/DNC_SKIPPED", ct.Status, ct.LastOutcome)
	}
	if r.placer.placedTo(queued) != 0 {
		t.Error("a do-not-call number was dialled")
	}
	// The ringing one is not hung up (still live, attempt still in flight) and
	// is DNC from now on.
	if !r.placer.isLive(ringing.Req.CallID) {
		t.Error("the ringing call was dropped")
	}
	if a, _ := r.st.Attempts().Get(context.Background(), ringing.Req.CallID); a.State != campaign.AttemptDialing {
		t.Errorf("ringing attempt = %+v", a)
	}
	if ct := r.contactOf(c, ringing.Req.To); ct.Status != campaign.ContactDoNotCall {
		t.Errorf("ringing contact = %s, want DNC", ct.Status)
	}
	// When it ends unanswered, no retry follows.
	r.end(ringing, endNoAnswer)
	r.end(other, endAnswered)
	r.settle()
	r.tick(72 * time.Hour)
	if r.placer.placedTo(ringing.Req.To) != 1 {
		t.Errorf("the do-not-call contact was retried")
	}
	if ct := r.contactOf(c, ringing.Req.To); ct.Status != campaign.ContactDoNotCall || ct.NextAttemptAt != nil {
		t.Errorf("ringing contact after its call = %+v", ct)
	}
	r.noViolations()
}

func TestTheTrunkBreakerTripsOnTheNthFailureAndNotTheOneBefore(t *testing.T) {
	for _, mode := range []string{"cdr", "sync"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, opts{trunkN: 3})
			c := r.newCampaign("Trunk", func(s *camSpec) { s.max = 1 })
			r.importContacts(c, numbers("47", 30))
			if mode == "sync" {
				r.placer.setFail(func(outbound.AIDialRequest) error { return errors.New("gateway down") })
			}
			r.start(c)

			if mode == "cdr" {
				for i := 0; i < 2; i++ {
					r.end(r.placer.liveCalls()[0], endPlacement)
					r.settle()
				}
			}
			// Two failures in a row: not yet.
			if mode == "sync" {
				// The sync mode fails every dial; it is the third that trips.
				got := r.campaignNow(c)
				if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseTrunkFailure {
					t.Fatalf("status %s/%s, want PAUSED/TRUNK_FAILURE", got.Status, got.PauseReason)
				}
				if n := len(r.attempts(c)); n != 3 {
					t.Errorf("%d attempts before the pause, want exactly 3: the pass must stop at the breaker", n)
				}
				return
			}
			if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
				t.Fatalf("paused after 2 failures: %s/%s", got.Status, got.PauseReason)
			}
			r.end(r.placer.liveCalls()[0], endPlacement)
			r.settle()
			got := r.campaignNow(c)
			if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseTrunkFailure {
				t.Fatalf("status %s/%s after the third failure, want PAUSED/TRUNK_FAILURE", got.Status, got.PauseReason)
			}
			placed := r.placer.total()
			r.tick(6 * time.Hour)
			if r.placer.total() != placed {
				t.Error("a paused campaign dialled")
			}

			// Resume is manual, and the run that paused it is not held against it.
			if _, err := r.dialer().Resume(context.Background(), c.ID); err != nil {
				t.Fatal(err)
			}
			r.settle()
			r.end(r.placer.liveCalls()[0], endPlacement)
			r.settle()
			if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
				t.Errorf("a single failure after resume paused the campaign again: %s", got.Status)
			}
		})
	}
}

// A deployment with no outbound endpoint is paused by the first attempt it
// refuses, not after the list has spent its system retries: the refusal is the
// same for every contact, so there is nothing to retry (#293). The attempt is
// voided, which is what "never happened" means in the ledger.
func TestACampaignThatCannotDialIsPausedOnItsFirstAttempt(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("No endpoint", nil)
	r.importContacts(c, numbers("49", 20))
	r.placer.setFail(func(outbound.AIDialRequest) error { return outbound.ErrNoOutboundEndpoint })
	r.start(c)

	got := r.campaignNow(c)
	if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseTrunkFailure {
		t.Fatalf("status %s/%s, want PAUSED/TRUNK_FAILURE", got.Status, got.PauseReason)
	}
	if n := len(r.attempts(c)); n != 0 {
		t.Errorf("%d attempt rows, want none: a refusal before a channel voids the attempt", n)
	}
	if ct := r.contactOf(c, numbers("49", 1)[0].number); ct.Status != campaign.ContactActive || ct.Attempts != 0 {
		t.Errorf("the refused contact = %s/%d attempts, want ACTIVE with none counted", ct.Status, ct.Attempts)
	}

	// Paused is paused: neither the rest of this pass nor hours of clock dial.
	placed := r.placer.total()
	r.tick(6 * time.Hour)
	if r.placer.total() != placed {
		t.Errorf("a paused campaign dialled %d more time(s)", r.placer.total()-placed)
	}

	// Resume is an operator's decision, and with the endpoint still missing the
	// next attempt pauses it again rather than retrying the list.
	if _, err := r.dialer().Resume(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if got := r.campaignNow(c); got.Status != campaign.StatusPaused {
		t.Errorf("a resumed campaign with no endpoint is %s, want PAUSED", got.Status)
	}
	if n := len(r.attempts(c)); n != 0 {
		t.Errorf("%d attempt rows after a resume, want none", n)
	}
	r.noViolations()
}

// The case the start refusal cannot cover: the campaign was RUNNING when the
// application stopped, and the endpoint is gone when it comes back. The new
// dialer reads it from the database and pauses it on its first attempt, rather
// than dialling the list into nothing until the retries run out (#293 review).
func TestACampaignRunningBeforeARestartIsPausedWhenTheEndpointIsGone(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Was running", nil)
	r.importContacts(c, numbers("47", 10))
	if _, err := r.st.Campaigns().Start(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	r.stopDialer() // the application went down; nothing has dialled yet
	r.placer.setFail(func(outbound.AIDialRequest) error { return outbound.ErrNoOutboundEndpoint })
	r.startDialer()
	r.rearm(c)
	r.dialer().Wake()
	r.settle()

	got := r.campaignNow(c)
	if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseTrunkFailure {
		t.Fatalf("status %s/%s, want PAUSED/TRUNK_FAILURE", got.Status, got.PauseReason)
	}
	if n := len(r.attempts(c)); n != 0 {
		t.Errorf("%d attempt rows, want none", n)
	}
}

func TestAnAnsweredCallBreaksTheTrunkRun(t *testing.T) {
	r := newRig(t, opts{trunkN: 3})
	c := r.newCampaign("Run", func(s *camSpec) { s.max = 1 })
	r.importContacts(c, numbers("48", 30))
	r.start(c)
	for _, kind := range []string{endPlacement, endPlacement, endAnswered, endPlacement, endPlacement} {
		r.end(r.placer.liveCalls()[0], kind)
		r.settle()
	}
	if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
		t.Errorf("status = %s: failures separated by an answered call are not consecutive", got.Status)
	}
}

func TestTheProviderBreakerCountsAnsweredCallsOnly(t *testing.T) {
	run := func(t *testing.T, kinds ...string) campaign.Campaign {
		r := newRig(t, opts{providerM: 2})
		c := r.newCampaign("Provider", func(s *camSpec) { s.max = 1 })
		r.importContacts(c, numbers("49", 30))
		r.start(c)
		for _, kind := range kinds {
			r.end(r.placer.liveCalls()[0], kind)
			r.settle()
		}
		return r.campaignNow(c)
	}
	t.Run("trips at M consecutive, unanswered calls between do not count", func(t *testing.T) {
		got := run(t, endBotFailed, endNoAnswer, endBotFailed)
		if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseProviderFailure {
			t.Errorf("status %s/%s, want PAUSED/PROVIDER_FAILURE", got.Status, got.PauseReason)
		}
	})
	t.Run("not at M minus one", func(t *testing.T) {
		if got := run(t, endBotFailed, endNoAnswer); got.Status != campaign.StatusRunning {
			t.Errorf("status = %s after one failure", got.Status)
		}
	})
	t.Run("an answered call that worked ends the run", func(t *testing.T) {
		if got := run(t, endBotFailed, endAnswered, endBotFailed); got.Status != campaign.StatusRunning {
			t.Errorf("status = %s", got.Status)
		}
	})
}

func TestAnAlreadyPlacedCallIsLeftInFlight(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Idempotent", func(s *camSpec) { s.max = 1 })
	r.importContacts(c, numbers("50", 1))
	r.placer.fail = func(outbound.AIDialRequest) error { return fmt.Errorf("wrapped: %w", outbound.ErrAlreadyPlaced) }
	r.start(c)
	att := r.attempts(c)
	if len(att) != 1 || att[0].State != campaign.AttemptDialing {
		t.Fatalf("attempts = %+v, want one still DIALING: the call exists", att)
	}
	if p := r.progress(c); p.InFlight != 1 {
		t.Errorf("in flight = %d", p.InFlight)
	}
}
