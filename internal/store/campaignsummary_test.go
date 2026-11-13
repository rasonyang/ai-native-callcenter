// SPDX-License-Identifier: Apache-2.0

package store

// A campaign's outcome summary (GET /campaigns/{campaignId}/outcome-summary)
// against the data of #476's verification: three campaigns, 3,000 contacts and
// about 7,000 attempts, written through the paths production writes them by:
// the write-ahead row, the lead the call records, the finish routine, the
// dialer's do-not-call skip and the callback work queue. Every count is held
// to three things: what the builder wrote, a direct count of the rows, and the
// total of the listing filtered the same way.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// fictionalNumber is the n-th number (0-99) of an area code's 555-0100 to
// 555-0199 block, which the North American plan reserves for fiction in every
// area code: no seeded number belongs to anybody.
func fictionalNumber(areaCode, n int) string {
	return fmt.Sprintf("+1%d555%04d", areaCode, 100+n)
}

// summaryTally is what the builder wrote into one campaign: the oracle the
// summary is checked against, kept without reading anything back.
type summaryTally struct {
	attempts    map[campaign.Outcome]int
	lastOutcome map[campaign.Outcome]int
	leads       map[campaign.LeadIntent]int
	said        int
	listed      int
	// midCall are DNC contacts whose number somebody else listed while
	// their call was in flight: DNC, and in neither opt-out count.
	midCall   int
	callbacks map[campaign.CallbackStatus]int
	contacts  int
	untried   int
	inFlight  int
}

func newSummaryTally() *summaryTally {
	return &summaryTally{
		attempts: map[campaign.Outcome]int{}, lastOutcome: map[campaign.Outcome]int{},
		leads: map[campaign.LeadIntent]int{}, callbacks: map[campaign.CallbackStatus]int{},
	}
}

// campaignPlan is how one campaign's 1,000 contacts end.
type campaignPlan struct {
	high          int // HIGH leads; highBotFailed of them on a call whose bot failed afterwards
	highBotFailed int
	said          int // DO_NOT_CALL leads
	skips         int // numbers already listed when they came up
	skipSource    string
	midCall       int // listed by somebody else while ringing
	medium        int
	low           int
	none          int
	noLead        int // answered, no lead recorded
	botFailed     int
	overflow      int
	invalid       int
	exhausted     int
	pending       int // tried, waiting for a retry
	inFlight      int // of the pending, ringing as the seed ends
	// skipNumbers, when set, are the numbers of the skipped contacts: another
	// campaign's opt-outs, which this one finds on the list.
	skipNumbers []string
}

type seedContact struct {
	id     uuid.UUID
	number string
}

type summarySeed struct {
	st        *Store
	cs        *CampaignStore
	attempts  *AttemptStore
	campaigns []campaign.Campaign
	want      map[uuid.UUID]*summaryTally
	// saidNumbers are each campaign's DO_NOT_CALL numbers, in campaign order.
	saidNumbers [][]string
	// highCalls are the calls of each campaign's HIGH leads.
	highCalls map[uuid.UUID][]uuid.UUID
	rng       *rand.Rand
	clock     time.Time
	agent     uuid.UUID
}

// openSummarySeed builds the seed. It takes a few seconds: every attempt is
// two transactions, as in production.
func openSummarySeed(t *testing.T) *summarySeed {
	t.Helper()
	ctx := context.Background()
	st, cs := openCampaigns(t)
	st.OutboundHours = dayrange.Range{Start: 8 * 60, End: 21 * 60}
	s := &summarySeed{
		st: st, cs: cs, attempts: st.Attempts(), want: map[uuid.UUID]*summaryTally{},
		highCalls: map[uuid.UUID][]uuid.UUID{},
		rng:       rand.New(rand.NewPCG(476, 2026)),
		clock:     time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
	}
	didID, _ := seedDID(t, st, "95099", false)
	var queueID uuid.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO queues (id, name, ext_number) VALUES ($1, 'follow-up', '7001') RETURNING id`,
		uuid.Must(uuid.NewV7())).Scan(&queueID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	s.agent = uuid.Must(uuid.NewV7())
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (id, username, password_hash, display_name, role) VALUES ($1, 'follow-up', 'x', 'Follow-up Agent', 'AGENT')`,
		s.agent); err != nil {
		t.Fatalf("agent: %v", err)
	}

	plans := []campaignPlan{
		{high: 30, highBotFailed: 3, said: 20, skips: 20, skipSource: DNCSourceManual,
			medium: 50, low: 70, none: 100, noLead: 60, botFailed: 112, overflow: 46, invalid: 30,
			exhausted: 100, pending: 150, inFlight: 2},
		{high: 30, highBotFailed: 4, said: 10, skips: 20, skipSource: DNCSourceImport, midCall: 3,
			medium: 45, low: 75, none: 95, noLead: 55, botFailed: 113, overflow: 45, invalid: 25,
			exhausted: 105, pending: 150, inFlight: 2},
		{high: 30, highBotFailed: 2, said: 10, skips: 20,
			medium: 55, low: 65, none: 105, noLead: 65, botFailed: 110, overflow: 44, invalid: 35,
			exhausted: 95, pending: 145, inFlight: 1},
	}
	for i, p := range plans {
		if i == 2 {
			// The third campaign holds twenty of the first one's opt-outs:
			// their numbers are on the list by the time it dials them.
			p.skipNumbers = s.saidNumbers[0][:20]
		}
		s.run(t, i, didID, queueID, p)
	}
	s.workCallbacks(t)
	return s
}

// tick is the next attempt's start: one minute after the last.
func (s *summarySeed) tick() time.Time {
	s.clock = s.clock.Add(time.Minute)
	return s.clock
}

// pick returns one of the weighted choices.
func pick[T any](r *rand.Rand, choices []T, weights []int) T {
	total := 0
	for _, w := range weights {
		total += w
	}
	n := r.IntN(total)
	for i, w := range weights {
		if n < w {
			return choices[i]
		}
		n -= w
	}
	return choices[len(choices)-1]
}

var (
	reachedRetries = []campaign.Outcome{campaign.OutcomeNoAnswer, campaign.OutcomeBusy, campaign.OutcomeRejected, campaign.OutcomeUnknown}
	reachedWeights = []int{60, 27, 10, 3}
	systemRetries  = []campaign.Outcome{campaign.OutcomeTransient, campaign.OutcomeOriginateFailed}
	systemWeights  = []int{65, 35}
)

// retries are the attempts that came to nothing before a contact's last: rings
// nobody answered and, now and then, the network's failures. reached of them
// reached the customer's line, and at most four of either kind, so the contact
// is still ACTIVE when its settling attempt is begun (the policy allows five).
func (s *summarySeed) retries(reached int) []campaign.Outcome {
	var out []campaign.Outcome
	system := 0
	for range reached {
		if system < 4 && s.rng.IntN(100) < 9 {
			out = append(out, pick(s.rng, systemRetries, systemWeights))
			system++
		}
		out = append(out, pick(s.rng, reachedRetries, reachedWeights))
	}
	return out
}

// someRetries is a terminal contact's history: mostly none or one or two.
func (s *summarySeed) someRetries() []campaign.Outcome {
	return s.retries(pick(s.rng, []int{0, 1, 2, 3, 4}, []int{30, 30, 20, 14, 6}))
}

var outcomeCause = map[campaign.Outcome]string{
	campaign.OutcomeAnswered: "NORMAL_CLEARING", campaign.OutcomeBotFailed: "BOT_START_FAILED",
	campaign.OutcomeOverflow: "OVERFLOW", campaign.OutcomeNoAnswer: "NO_ANSWER", campaign.OutcomeBusy: "USER_BUSY",
	campaign.OutcomeRejected: "CALL_REJECTED", campaign.OutcomeUnknown: "CHANNEL_VANISHED",
	campaign.OutcomeTransient: "NORMAL_TEMPORARY_FAILURE", campaign.OutcomeOriginateFailed: "GATEWAY_DOWN",
	campaign.OutcomeInvalidNumber: "UNALLOCATED_NUMBER", campaign.OutcomeDNCSkipped: "NUMBER_ON_DNC",
}

// leadNotes are what the bot writes for each intent: synthetic sentences.
var leadNotes = map[campaign.LeadIntent]string{
	campaign.LeadHigh:      "Wants the renewal offer sent by email and a call back on Friday.",
	campaign.LeadMedium:    "Interested, asked to be called after the holiday.",
	campaign.LeadLow:       "Happy with the current plan for now.",
	campaign.LeadNone:      "No interest in changing plans.",
	campaign.LeadDoNotCall: "Asked not to be called again.",
}

// attempt places one call of ct and ends it with out. A lead, when given, is
// recorded while the call is up, as the bot records it.
func (s *summarySeed) attempt(t *testing.T, w *summaryTally, ct seedContact, out campaign.Outcome, lead campaign.LeadIntent) campaign.Attempt {
	t.Helper()
	ctx := context.Background()
	at := s.tick()
	a, err := s.attempts.Begin(ctx, ct.id, at)
	if err != nil {
		t.Fatalf("begin %s: %v", ct.number, err)
	}
	if lead != "" {
		if ok, err := s.attempts.SetLead(ctx, a.CallID, lead, leadNotes[lead]); err != nil || !ok {
			t.Fatalf("lead %s: %v %v", ct.number, ok, err)
		}
	}
	talk := 0
	if campaign.IsAnsweredOutcome(out) {
		talk = 20 + s.rng.IntN(280)
	}
	if _, err := s.attempts.Finish(ctx, a.CallID, campaign.Ending{
		Outcome: out, HangupCause: outcomeCause[out], TalkSec: talk, EndedAt: at.Add(40 * time.Second),
	}); err != nil {
		t.Fatalf("finish %s %s: %v", ct.number, out, err)
	}
	w.attempts[out]++
	return a
}

// history runs a contact's attempts in order and tallies where it ended.
func (s *summarySeed) history(t *testing.T, w *summaryTally, ct seedContact, outs []campaign.Outcome, last campaign.Outcome, lead campaign.LeadIntent) campaign.Attempt {
	t.Helper()
	for _, o := range outs {
		s.attempt(t, w, ct, o, "")
	}
	a := s.attempt(t, w, ct, last, lead)
	w.lastOutcome[last]++
	if lead != "" {
		w.leads[lead]++
	}
	return a
}

func (s *summarySeed) run(t *testing.T, index int, didID, queueID uuid.UUID, p campaignPlan) {
	t.Helper()
	ctx := context.Background()
	in := newCampaign(didID, fmt.Sprintf("Renewal wave %d", index+1))
	in.TimeZone = "America/New_York"
	in.CallingWindow = attemptWindow
	in.FollowUpQueueID = &queueID
	in.RetryPolicy.MaxAttempts = 5
	in.RetryPolicy.MaxSystemRetries = 5
	c, err := s.cs.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	// 1,000 contacts from ten area codes, the skipped ones' numbers taken
	// from skipNumbers when the plan has them.
	var numbers []string
	for area := range 10 {
		for n := range 100 {
			numbers = append(numbers, fictionalNumber(201+10*index+area, n))
		}
	}
	numbers = append(slices.Clone(p.skipNumbers), numbers[:1000-len(p.skipNumbers)]...)
	contacts := make([]seedContact, len(numbers))
	rows := make([]campaign.NewContact, len(numbers))
	for i, n := range numbers {
		contacts[i] = seedContact{id: uuid.Must(uuid.NewV7()), number: n}
		rows[i] = campaign.NewContact{ID: contacts[i].id, Number: n, NumberRaw: n,
			Slots: map[string]string{"name": fmt.Sprintf("Customer %04d", i)}}
	}
	if _, err := s.cs.ImportContacts(ctx, c.ID, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cs.Start(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	w := newSummaryTally()
	w.contacts = len(contacts)
	s.campaigns = append(s.campaigns, c)
	s.want[c.ID] = w

	skipped := contacts[:p.skips]
	rest := slices.Clone(contacts[p.skips:])
	s.rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	take := func(n int) []seedContact {
		out := rest[:n]
		rest = rest[n:]
		return out
	}

	// The opt-outs first, so that their numbers are on the list for the
	// campaigns that come after.
	var said []string
	for _, ct := range take(p.said) {
		s.history(t, w, ct, s.someRetries(), campaign.OutcomeAnswered, campaign.LeadDoNotCall)
		said = append(said, ct.number)
		w.said++
	}
	s.saidNumbers = append(s.saidNumbers, said)

	for i, ct := range take(p.high) {
		last := campaign.OutcomeAnswered
		if i < p.highBotFailed {
			last = campaign.OutcomeBotFailed
		}
		a := s.history(t, w, ct, s.someRetries(), last, campaign.LeadHigh)
		s.highCalls[c.ID] = append(s.highCalls[c.ID], a.CallID)
	}
	for _, lead := range []struct {
		intent campaign.LeadIntent
		n      int
	}{{campaign.LeadMedium, p.medium}, {campaign.LeadLow, p.low}, {campaign.LeadNone, p.none}} {
		for _, ct := range take(lead.n) {
			s.history(t, w, ct, s.someRetries(), campaign.OutcomeAnswered, lead.intent)
		}
	}
	for _, ending := range []struct {
		outcome campaign.Outcome
		n       int
	}{
		{campaign.OutcomeAnswered, p.noLead}, {campaign.OutcomeBotFailed, p.botFailed},
		{campaign.OutcomeOverflow, p.overflow},
	} {
		for _, ct := range take(ending.n) {
			s.history(t, w, ct, s.someRetries(), ending.outcome, "")
		}
	}
	// A bad number is found out on its first ring.
	for _, ct := range take(p.invalid) {
		s.history(t, w, ct, nil, campaign.OutcomeInvalidNumber, "")
	}

	// Already listed: rung up to twice before somebody listed the number,
	// then skipped by the dialer. Another campaign's opt-outs are skipped the
	// first time they come up.
	for _, ct := range skipped {
		if p.skipNumbers == nil {
			for _, o := range s.retries(s.rng.IntN(3)) {
				s.attempt(t, w, ct, o, "")
			}
			if _, _, err := s.st.DNC().Add(ctx, ct.number, p.skipSource, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		s.history(t, w, ct, nil, campaign.OutcomeDNCSkipped, "")
		w.listed++
	}

	// Listed by somebody else while the call rang: the call is not hung up
	// and the contact is DNC when it ends.
	for _, ct := range take(p.midCall) {
		for _, o := range s.retries(1) {
			s.attempt(t, w, ct, o, "")
		}
		at := s.tick()
		a, err := s.attempts.Begin(ctx, ct.id, at)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.st.DNC().Add(ctx, ct.number, DNCSourceManual, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.st.Dialer().MarkDialingDNC(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.attempts.Finish(ctx, a.CallID, campaign.Ending{
			Outcome: campaign.OutcomeNoAnswer, HangupCause: "NO_ANSWER", EndedAt: at.Add(40 * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		w.attempts[campaign.OutcomeNoAnswer]++
		w.lastOutcome[campaign.OutcomeNoAnswer]++
		w.midCall++
	}

	// Exhausted: five rings that came to nothing, or now and then five
	// failures of the network.
	for i, ct := range take(p.exhausted) {
		if i%20 == 0 {
			outs := slices.DeleteFunc(s.retries(s.rng.IntN(3)), func(o campaign.Outcome) bool {
				return campaign.ClassOf(o) == campaign.ClassSystem
			})
			outs = append(outs, campaign.OutcomeTransient, campaign.OutcomeOriginateFailed,
				campaign.OutcomeTransient, campaign.OutcomeOriginateFailed)
			s.history(t, w, ct, outs, campaign.OutcomeTransient, "")
			continue
		}
		outs := s.retries(5)
		s.history(t, w, ct, outs[:len(outs)-1], outs[len(outs)-1], "")
	}

	// Pending: tried one to four times and waiting for the next try; a few
	// are ringing as the seed ends.
	for i, ct := range take(p.pending) {
		outs := s.retries(1 + s.rng.IntN(4))
		s.history(t, w, ct, outs[:len(outs)-1], outs[len(outs)-1], "")
		if i < p.inFlight {
			if _, err := s.attempts.Begin(ctx, ct.id, s.tick()); err != nil {
				t.Fatal(err)
			}
			w.inFlight++
		}
	}
	w.untried = len(rest)
}

// workCallbacks moves the callbacks the HIGH leads filed through the agents'
// work queue: of each campaign's thirty, twelve stay open, eight are claimed,
// six are done and four dismissed (claimed first, as an agent does).
func (s *summarySeed) workCallbacks(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	ledger := s.st.Ledger()
	plan := []campaign.CallbackStatus{}
	for st, n := range map[campaign.CallbackStatus]int{
		campaign.CallbackStatusOpen: 12, campaign.CallbackStatusClaimed: 8,
		campaign.CallbackStatusDone: 6, campaign.CallbackStatusDismissed: 4,
	} {
		for range n {
			plan = append(plan, st)
		}
	}
	slices.Sort(plan)
	for _, c := range s.campaigns {
		calls := s.highCalls[c.ID]
		if len(calls) != len(plan) {
			t.Fatalf("%d HIGH calls, want %d", len(calls), len(plan))
		}
		order := slices.Clone(plan)
		s.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for i, callID := range calls {
			got, total, err := ledger.ListCallbacks(ctx, "", &callID, 10, 0)
			if err != nil || total != 1 {
				t.Fatalf("the HIGH call %s filed %d callbacks (%v), want 1", callID, total, err)
			}
			id := got[0].ID
			switch order[i] {
			case campaign.CallbackStatusClaimed:
				_, err = ledger.ClaimCallback(ctx, id, s.agent)
			case campaign.CallbackStatusDone, campaign.CallbackStatusDismissed:
				if _, err = ledger.ClaimCallback(ctx, id, s.agent); err == nil {
					_, err = ledger.HandleCallback(ctx, id, string(order[i]), s.agent)
				}
			}
			if err != nil {
				t.Fatalf("callback %s to %s: %v", id, order[i], err)
			}
			s.want[c.ID].callbacks[order[i]]++
		}
	}
}

func TestACampaignsOutcomeSummaryCountsWhatItsCallsCameTo(t *testing.T) {
	ctx := context.Background()
	s := openSummarySeed(t)

	t.Run("the seed is the one the issue asks for", func(t *testing.T) {
		var contacts, said, listed, high int
		attempts := map[campaign.Outcome]int{}
		leads := map[campaign.LeadIntent]int{}
		callbacks := map[campaign.CallbackStatus]int{}
		for _, w := range s.want {
			contacts += w.contacts
			said += w.said
			listed += w.listed
			high += w.leads[campaign.LeadHigh]
			for o, n := range w.attempts {
				attempts[o] += n
			}
			for l, n := range w.leads {
				leads[l] += n
			}
			for st, n := range w.callbacks {
				callbacks[st] += n
			}
		}
		total := 0
		for _, n := range attempts {
			total += n
		}
		share := func(outs ...campaign.Outcome) float64 {
			n := 0
			for _, o := range outs {
				n += attempts[o]
			}
			return 100 * float64(n) / float64(total)
		}
		t.Logf("%d campaigns, %d contacts, %d finished attempts: %v", len(s.campaigns), contacts, total, attempts)
		if len(s.campaigns) != 3 || contacts != 3000 || total < 6500 || total > 7500 {
			t.Errorf("%d campaigns, %d contacts, %d attempts; want 3, 3000 and about 7000", len(s.campaigns), contacts, total)
		}
		for _, o := range campaign.AllOutcomes() {
			if attempts[o] == 0 {
				t.Errorf("no attempt ended %s", o)
			}
		}
		if v := share(campaign.OutcomeNoAnswer, campaign.OutcomeBusy); v < 57 || v > 63 {
			t.Errorf("NO_ANSWER or BUSY is %.1f%% of the attempts, want about 60%%", v)
		}
		if v := share(campaign.OutcomeBotFailed); v < 4.5 || v > 5.5 {
			t.Errorf("BOT_FAILED is %.1f%%, want about 5%%", v)
		}
		if v := share(campaign.OutcomeOverflow); v < 1.7 || v > 2.3 {
			t.Errorf("OVERFLOW is %.1f%%, want about 2%%", v)
		}
		for _, l := range campaign.AllLeadIntents() {
			if leads[l] == 0 {
				t.Errorf("no lead is %s", l)
			}
		}
		if said != 40 || listed != 60 || high != 90 {
			t.Errorf("%d opt-outs said, %d skips of listed numbers, %d HIGH leads; want 40, 60 and 90", said, listed, high)
		}
		if len(callbacks) != 4 {
			t.Errorf("callbacks are in %d states, want all four: %v", len(callbacks), callbacks)
		}
	})

	t.Run("every count is what was written, a direct count and the listing's total", func(t *testing.T) {
		for _, c := range s.campaigns {
			w := s.want[c.ID]
			sum, err := s.cs.OutcomeSummary(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if sum.CampaignID != c.ID {
				t.Errorf("summary of %s names %s", c.ID, sum.CampaignID)
			}
			s.checkSummary(t, c, w, sum)
		}
	})

	t.Run("a revised call record moves its attempt from one outcome to the other once", func(t *testing.T) {
		c := s.campaigns[0]
		callID := s.highCalls[c.ID][len(s.highCalls[c.ID])-1] // answered: the BOT_FAILED ones come first
		a, err := s.attempts.Get(ctx, callID)
		if err != nil || a.Outcome != campaign.OutcomeAnswered || a.LeadIntent != campaign.LeadHigh {
			t.Fatalf("attempt = %+v %v, want an answered HIGH lead", a, err)
		}
		before := s.summaries(t)
		// The fuller record says the bot failed on the answered call.
		revised := summaryCDR(a, CDRStatusFailed, 3*time.Minute)
		for range 2 { // the second is the identical record again
			if err := s.st.Ledger().InsertCDR(ctx, revised); err != nil {
				t.Fatal(err)
			}
		}
		s.wantMoved(t, before, c.ID, map[string]int{
			"attempts/ANSWERED": -1, "attempts/BOT_FAILED": 1,
			"lastOutcome/ANSWERED": -1, "lastOutcome/BOT_FAILED": 1,
		})
		if _, n, err := s.st.Ledger().ListCallbacks(ctx, "", &callID, 10, 0); err != nil || n != 1 {
			t.Errorf("the revised call has %d callbacks (%v), want the one it filed", n, err)
		}
	})

	t.Run("a revised call record that changes the attempt's lead moves the lead once", func(t *testing.T) {
		c := s.campaigns[0]
		contacts, _, err := s.cs.ListContacts(ctx, c.ID, campaign.ContactFilter{LeadIntent: campaign.LeadMedium, Limit: 1})
		if err != nil || len(contacts) != 1 || contacts[0].LastCallID == nil {
			t.Fatalf("a MEDIUM lead: %+v %v", contacts, err)
		}
		callID := *contacts[0].LastCallID
		a, err := s.attempts.Get(ctx, callID)
		if err != nil || a.Outcome != campaign.OutcomeAnswered {
			t.Fatalf("attempt = %+v %v", a, err)
		}
		before := s.summaries(t)
		// No write path changes a finished attempt's lead today (SetLead is
		// refused once the attempt is FINISHED), so the row is changed by
		// hand, as a later revision path would, and fuller records of the
		// call run the finish routine again.
		if _, err := s.st.Pool.Exec(ctx,
			`UPDATE campaign_attempts SET lead_intent = 'HIGH', lead_notes = $2 WHERE call_id = $1`,
			callID, leadNotes[campaign.LeadHigh]); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			cdr := summaryCDR(a, CDRStatusAnswered, time.Duration(4+i)*time.Minute)
			if err := s.st.Ledger().InsertCDR(ctx, cdr); err != nil {
				t.Fatal(err)
			}
		}
		s.wantMoved(t, before, c.ID, map[string]int{
			"lead/MEDIUM": -1, "lead/HIGH": 1, "callbacks/OPEN": 1,
		})
		if _, n, err := s.st.Ledger().ListCallbacks(ctx, "", &callID, 10, 0); err != nil || n != 1 {
			t.Errorf("the call has %d callbacks after three revisions (%v), want 1", n, err)
		}
	})

	t.Run("an unknown campaign is not found", func(t *testing.T) {
		if _, err := s.cs.OutcomeSummary(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, campaign.ErrNotFound) {
			t.Errorf("unknown campaign: %v", err)
		}
	})
}

// checkSummary holds every count of one campaign's summary to the tally, to a
// direct count of the rows and to the listing filtered the same way.
func (s *summarySeed) checkSummary(t *testing.T, c campaign.Campaign, w *summaryTally, sum campaign.OutcomeSummary) {
	t.Helper()
	ctx := context.Background()
	direct := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := s.st.Pool.QueryRow(ctx, query, append([]any{c.ID}, args...)...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	contactsListed := func(f campaign.ContactFilter) int {
		t.Helper()
		f.Limit = 1
		_, total, err := s.cs.ListContacts(ctx, c.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		return int(total)
	}
	agree := func(what string, got, wrote, counted, listed int) {
		t.Helper()
		if got != wrote || got != counted || got != listed {
			t.Errorf("%s %s: summary %d, written %d, counted %d, listed %d", c.Name, what, got, wrote, counted, listed)
		}
	}

	finished := 0
	for _, o := range campaign.AllOutcomes() {
		_, listed, err := s.cs.ListAttempts(ctx, c.ID, campaign.AttemptFilter{Outcome: o, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		agree("attempts "+string(o), sum.AttemptsByOutcome[o], w.attempts[o],
			direct(`SELECT count(*) FROM campaign_attempts WHERE campaign_id = $1 AND outcome = $2`, string(o)), int(listed))
		agree("contacts last "+string(o), sum.ContactsByLastOutcome[o], w.lastOutcome[o],
			direct(`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND last_outcome = $2`, string(o)),
			contactsListed(campaign.ContactFilter{LastOutcome: o}))
		finished += sum.AttemptsByOutcome[o]
	}
	for _, l := range campaign.AllLeadIntents() {
		agree("contacts lead "+string(l), sum.ContactsByLeadIntent[l], w.leads[l],
			direct(`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND lead_intent = $2`, string(l)),
			contactsListed(campaign.ContactFilter{LeadIntent: l}))
	}
	agree("opt-outs said in conversation", sum.OptOuts.SaidInConversation, w.said,
		direct(`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND status = 'DNC' AND lead_intent = 'DO_NOT_CALL'`),
		contactsListed(campaign.ContactFilter{Status: campaign.ContactDoNotCall, LeadIntent: campaign.LeadDoNotCall}))
	agree("opt-outs already listed", sum.OptOuts.AlreadyListed, w.listed,
		direct(`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND status = 'DNC' AND last_outcome = 'DNC_SKIPPED'`),
		contactsListed(campaign.ContactFilter{Status: campaign.ContactDoNotCall, LastOutcome: campaign.OutcomeDNCSkipped}))

	// The callbacks, found the way the leads tab finds them: each HIGH
	// contact's lastCallId, asked of listCallbacks with the status.
	listedCallbacks := map[campaign.CallbackStatus]int{}
	for offset := 0; ; offset += 100 {
		page, _, err := s.cs.ListContacts(ctx, c.ID, campaign.ContactFilter{LeadIntent: campaign.LeadHigh, Limit: 100, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		for _, ct := range page {
			for _, st := range campaign.AllCallbackStatuses() {
				_, n, err := s.st.Ledger().ListCallbacks(ctx, string(st), ct.LastCallID, 10, 0)
				if err != nil {
					t.Fatal(err)
				}
				listedCallbacks[st] += int(n)
			}
		}
		if len(page) < 100 {
			break
		}
	}
	for _, st := range campaign.AllCallbackStatuses() {
		agree("callbacks "+string(st), sum.CallbacksByStatus[st], w.callbacks[st],
			direct(`SELECT count(*) FROM callbacks cb JOIN campaign_attempts a ON a.call_id = cb.call_id
			        WHERE a.campaign_id = $1 AND cb.status = $2`, string(st)),
			listedCallbacks[st])
	}

	// The totals the breakdowns add up to.
	_, all, err := s.cs.ListAttempts(ctx, c.ID, campaign.AttemptFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if finished != int(all)-w.inFlight {
		t.Errorf("%s: %d finished attempts counted, %d listed of which %d in flight", c.Name, finished, all, w.inFlight)
	}
	tried := 0
	for _, n := range sum.ContactsByLastOutcome {
		tried += n
	}
	got, err := s.cs.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tried != got.Contacts.Total-w.untried || got.Contacts.Total != w.contacts {
		t.Errorf("%s: %d contacts have a last outcome; %d contacts, %d never tried", c.Name, tried, got.Contacts.Total, w.untried)
	}
	if dnc := sum.OptOuts.SaidInConversation + sum.OptOuts.AlreadyListed + w.midCall; dnc != got.Contacts.DNC {
		t.Errorf("%s: %d said + %d listed + %d listed mid-call, but %d contacts are DNC",
			c.Name, sum.OptOuts.SaidInConversation, sum.OptOuts.AlreadyListed, w.midCall, got.Contacts.DNC)
	}
}

// summaryCDR is a fuller record of an answered campaign call that lasted for
// long, written as the ledger writes one: status FAILED is the bot path saying
// the conversation could not go on.
func summaryCDR(a campaign.Attempt, status string, long time.Duration) CDR {
	return CDR{
		CallID: a.CallID, StartedAt: a.StartedAt, AnsweredAt: a.StartedAt.Add(5 * time.Second),
		EndedAt: a.StartedAt.Add(long), CallType: "OUTBOUND", FromNumber: "95099", ToNumber: a.Number,
		DID: "95099", Status: status, HangupCause: "NORMAL_CLEARING", BotSec: int(long.Seconds()) - 5,
		Legs: []Leg{{Kind: "TRUNK", Label: "customer"}},
	}
}

// summaries reads every campaign's summary, flattened to one count per key.
func (s *summarySeed) summaries(t *testing.T) map[uuid.UUID]map[string]int {
	t.Helper()
	out := map[uuid.UUID]map[string]int{}
	for _, c := range s.campaigns {
		sum, err := s.cs.OutcomeSummary(context.Background(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		flat := map[string]int{
			"optOuts/said": sum.OptOuts.SaidInConversation, "optOuts/listed": sum.OptOuts.AlreadyListed,
		}
		for _, o := range campaign.AllOutcomes() {
			flat["attempts/"+string(o)] = sum.AttemptsByOutcome[o]
			flat["lastOutcome/"+string(o)] = sum.ContactsByLastOutcome[o]
		}
		for _, l := range campaign.AllLeadIntents() {
			flat["lead/"+string(l)] = sum.ContactsByLeadIntent[l]
		}
		for _, st := range campaign.AllCallbackStatuses() {
			flat["callbacks/"+string(st)] = sum.CallbacksByStatus[st]
		}
		out[c.ID] = flat
	}
	return out
}

// wantMoved checks that, since before, exactly the counts in moved changed in
// campaign id, by exactly those amounts, and nothing in any other campaign.
func (s *summarySeed) wantMoved(t *testing.T, before map[uuid.UUID]map[string]int, id uuid.UUID, moved map[string]int) {
	t.Helper()
	after := s.summaries(t)
	for _, c := range s.campaigns {
		got := map[string]int{}
		for _, k := range slices.Sorted(maps.Keys(after[c.ID])) {
			if d := after[c.ID][k] - before[c.ID][k]; d != 0 {
				got[k] = d
			}
		}
		want := map[string]int{}
		if c.ID == id {
			want = moved
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s moved %v, want %v", c.Name, got, want)
		}
	}
}
