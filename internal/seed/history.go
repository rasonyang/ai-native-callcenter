// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// QueueEventRow is one queue movement to insert.
type QueueEventRow struct {
	OccurredAt time.Time
	CallID     uuid.UUID
	QueueID    uuid.UUID
	Event      string
	AgentID    *uuid.UUID
	WaitMs     int
}

// StateLogRow is one presence interval to insert.
type StateLogRow struct {
	AgentID   uuid.UUID
	State     string
	Reason    *string
	EnteredAt time.Time
	ExitedAt  *time.Time
}

// ContactRow is one customer record to insert.
type ContactRow struct {
	PhoneNumber string
	Name        string
	Company     string
	Tags        []string
	Notes       string
}

// WrapUpRow is one agent's after-call work on a seeded call.
type WrapUpRow struct {
	CallID          uuid.UUID
	AgentID         uuid.UUID
	DispositionCode string
	Note            string
}

// Plan is the whole seven-day history, generation separated from insertion
// so determinism is testable without a database.
type Plan struct {
	CDRs        []store.CDR
	QueueEvents []QueueEventRow
	StateLogs   []StateLogRow
	Contacts    []ContactRow
	WrapUps     []WrapUpRow
}

// demoContacts is the cast behind the numbers that called. The names are
// attached to whichever seeded callers turn up, so a contact always has real
// history behind it and the cockpit's caller card is never empty on a demo.
var demoContacts = []struct{ name, company, tag, note string }{
	{"Zhang Wei", "NovaNet", "VIP", "Prefers callbacks after 16:00."},
	{"Emily Carter", "Harbor Foods", "Enterprise", "Two lines on the same account."},
	{"Li Na", "Kestrel Design", "", "Billing questions only; technical goes to her colleague."},
	{"Sofia Reyes", "Blue Ridge Ltd", "Enterprise", ""},
	{"Huang Lei", "NovaNet", "", "Asked to be called on the mobile, not the office line."},
	{"Grace Lin", "Meridian Travel", "VIP", "Renewal due next quarter."},
	{"Sun Qiang", "Yuhua Logistics", "", ""},
	{"Wang Fang", "Pine Street Clinic", "", "Hard of hearing; speak slowly."},
}

// seedContactsAndWrapUps gives the history the two things an agent's own
// screens read: who the callers are, and what the agent wrote up afterwards.
//
// It runs after the calls are laid out and draws from the same stream, so the
// calls themselves are byte-identical to what this generator produced before
// either existed.
func (p *Plan) seedContactsAndWrapUps(dispositions []string, rng *rand.Rand) {
	seen := map[string]bool{}
	for _, cdr := range p.CDRs {
		if len(p.Contacts) >= len(demoContacts) {
			break
		}
		if cdr.CallType != "INBOUND" || cdr.FromNumber == "" || seen[cdr.FromNumber] {
			continue
		}
		seen[cdr.FromNumber] = true
		person := demoContacts[len(p.Contacts)]
		contact := ContactRow{
			PhoneNumber: cdr.FromNumber, Name: person.name,
			Company: person.company, Notes: person.note,
		}
		if person.tag != "" {
			contact.Tags = []string{person.tag}
		}
		p.Contacts = append(p.Contacts, contact)
	}

	if len(dispositions) == 0 {
		return
	}
	for _, cdr := range p.CDRs {
		if cdr.PrimaryAgentID == nil {
			continue
		}
		row := WrapUpRow{
			CallID:          cdr.CallID,
			AgentID:         *cdr.PrimaryAgentID,
			DispositionCode: dispositions[rng.Intn(len(dispositions))],
		}
		// Only some calls get a note, and only one that fits what was filed:
		// a demo whose note contradicts its own disposition teaches the
		// screen to be ignored.
		if notes := demoNotes[row.DispositionCode]; len(notes) > 0 && rng.Intn(3) == 0 {
			row.Note = notes[rng.Intn(len(notes))]
		}
		p.WrapUps = append(p.WrapUps, row)
	}
}

// demoNotes are the kind of thing an agent actually types, per disposition —
// keyed by code so the note agrees with what was filed. A vocabulary this map
// does not know simply gets no notes, which is the honest default for an
// installation that renamed its own.
var demoNotes = map[string][]string{
	"RESOLVED": {
		"Walked them through the reset; confirmed the line came back.",
		"Explained the billing cycle; nothing further needed.",
		"Refund raised, reference sent by SMS.",
	},
	"FOLLOW_UP_REQUIRED": {
		"Calling back tomorrow after 16:00 as agreed.",
		"Escalated to L2 — hardware fault suspected.",
		"Waiting on the photo of the meter before we can proceed.",
	},
	"NO_ANSWER": {
		"No audio either way; asked them to redial.",
		"Line dropped before they said anything.",
	},
	"OTHER": {
		"Reached the wrong department; redirected them.",
		"Silent call, no response to greeting.",
	},
}

// The demo numbering plan. DIDs are plain text on a CDR, so the history can
// name them without the dids table existing yet.
var demoDIDs = []struct {
	number   string
	language string
	queue    string
}{
	{"95011", "en", "support-en"},
	{"95012", "zh", "support-zh"},
}

// planHistory lays out the last seven days. Everything derives from the
// passed rng and the day grid, so one seed always tells the same story.
func planHistory(now time.Time, agents []uuid.UUID, queues []QueueRef,
	dispositions []string, rng *rand.Rand) Plan {
	var plan Plan
	queueByName := map[string]QueueRef{}
	for _, q := range queues {
		queueByName[q.Name] = q
	}

	for dayOffset := 6; dayOffset >= 0; dayOffset-- {
		day := now.AddDate(0, 0, -dayOffset)
		dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())

		volume := 45 + rng.Intn(26)
		if wd := dayStart.Weekday(); wd == time.Saturday || wd == time.Sunday {
			volume = 20 + rng.Intn(11)
		}

		for i := 0; i < volume; i++ {
			startedAt := dayStart.Add(businessMoment(rng))
			if startedAt.After(now) {
				continue // today's afternoon has not happened yet
			}
			plan.add(oneCall(startedAt, agents, queueByName, rng))
		}

		for _, agentID := range agents {
			if dayStart.Weekday() == time.Saturday || dayStart.Weekday() == time.Sunday {
				continue
			}
			plan.StateLogs = append(plan.StateLogs, shift(dayStart, agentID, rng)...)
		}
	}
	plan.seedContactsAndWrapUps(dispositions, rng)
	return plan
}

func (p *Plan) add(cdr store.CDR, events []QueueEventRow) {
	p.CDRs = append(p.CDRs, cdr)
	p.QueueEvents = append(p.QueueEvents, events...)
}

// businessMoment picks a time of day weighted towards the two busy ridges a
// support line actually has.
func businessMoment(rng *rand.Rand) time.Duration {
	hour := 9 + rng.Intn(9) // 09..17
	if rng.Intn(3) == 0 {   // a third of calls pile onto the ridges
		if rng.Intn(2) == 0 {
			hour = 10
		} else {
			hour = 15
		}
	}
	return time.Duration(hour)*time.Hour +
		time.Duration(rng.Intn(3600))*time.Second
}

// oneCall writes one believable story into a CDR and its queue movements.
func oneCall(startedAt time.Time, agents []uuid.UUID, queues map[string]QueueRef, rng *rand.Rand) (store.CDR, []QueueEventRow) {
	did := demoDIDs[rng.Intn(len(demoDIDs))]
	callID := deterministicUUID(rng)
	caller := "138" + digits(rng, 8)

	cdr := store.CDR{
		CallID:     callID,
		StartedAt:  startedAt,
		CallType:   "INBOUND",
		Language:   did.language,
		FromNumber: caller,
		ToNumber:   did.number,
		DID:        did.number,
		Status:     store.CDRStatusAnswered,
		Tech:       map[string]any{"isSeeded": true},
	}

	queue, hasQueue := queues[did.queue]
	roll := rng.Intn(100)
	switch {
	case roll < 55: // the bot contains the call
		bot := 25 + rng.Intn(150)
		cdr.AnsweredAt = startedAt
		cdr.BotSec = bot
		cdr.TotalSec = bot
		cdr.IsContained = true
		cdr.HangupCause = "NORMAL_CLEARING"
		cdr.Legs = []store.Leg{{Kind: "BOT", Label: "novanet_support", DurationSec: bot}}
		cdr.EndedAt = startedAt.Add(secs(bot))
		return cdr, nil

	case roll < 75 && hasQueue && len(agents) > 0: // bot hands over, an agent answers
		bot := 20 + rng.Intn(60)
		wait := 5 + rng.Intn(40)
		talk := 60 + rng.Intn(420)
		agent := agents[rng.Intn(len(agents))]
		cdr.AnsweredAt = startedAt
		cdr.BotSec = bot
		cdr.QueueWaitSec = wait
		cdr.RingSec = 3 + rng.Intn(5)
		cdr.TalkSec = talk
		cdr.TotalSec = bot + wait + cdr.RingSec + talk
		cdr.QueueID = &queue.ID
		cdr.AgentIDs = []uuid.UUID{agent}
		cdr.PrimaryAgentID = &agent
		cdr.HangupCause = "NORMAL_CLEARING"
		cdr.Legs = []store.Leg{
			{Kind: "BOT", Label: "novanet_support", DurationSec: bot},
			{Kind: "QUEUE", Label: queue.Name, DurationSec: wait},
			{Kind: "AGENT", Label: "", DurationSec: talk},
		}
		cdr.EndedAt = startedAt.Add(secs(cdr.TotalSec))
		joined := startedAt.Add(secs(bot))
		bridged := joined.Add(secs(wait))
		return cdr, []QueueEventRow{
			{OccurredAt: joined, CallID: callID, QueueID: queue.ID, Event: "JOINED"},
			{OccurredAt: bridged, CallID: callID, QueueID: queue.ID, Event: "BRIDGED",
				AgentID: &agent, WaitMs: wait * 1000},
		}

	case roll < 85 && hasQueue: // gives up in the queue
		bot := 15 + rng.Intn(40)
		wait := 30 + rng.Intn(90)
		cdr.AnsweredAt = startedAt
		cdr.BotSec = bot
		cdr.QueueWaitSec = wait
		cdr.TotalSec = bot + wait
		cdr.QueueID = &queue.ID
		// The bot answered before the queue did; abandonment is a reason on
		// an answered call, not a status of its own.
		cdr.MissedReason = "ABANDONED_WAITING"
		cdr.HangupCause = "ORIGINATOR_CANCEL"
		cdr.Legs = []store.Leg{
			{Kind: "BOT", Label: "novanet_support", DurationSec: bot},
			{Kind: "QUEUE", Label: queue.Name, DurationSec: wait},
		}
		cdr.EndedAt = startedAt.Add(secs(cdr.TotalSec))
		joined := startedAt.Add(secs(bot))
		return cdr, []QueueEventRow{
			{OccurredAt: joined, CallID: callID, QueueID: queue.ID, Event: "JOINED"},
			{OccurredAt: joined.Add(secs(wait)), CallID: callID, QueueID: queue.ID,
				Event: "LEFT", WaitMs: wait * 1000},
		}

	case roll < 90: // hangs up on the bot almost immediately
		short := 2 + rng.Intn(3)
		cdr.AnsweredAt = startedAt
		cdr.BotSec = short
		cdr.TotalSec = short
		cdr.MissedReason = "SHORT_ABANDONED"
		cdr.HangupCause = "ORIGINATOR_CANCEL"
		cdr.Legs = []store.Leg{{Kind: "BOT", Label: "novanet_support", DurationSec: short}}
		cdr.EndedAt = startedAt.Add(secs(short))
		return cdr, nil

	default: // the platform dialed out and the bot ran the conversation
		bot := 30 + rng.Intn(120)
		cdr.CallType = "OUTBOUND"
		cdr.FromNumber = did.number
		cdr.ToNumber = caller
		cdr.AnsweredAt = startedAt
		cdr.BotSec = bot
		cdr.TotalSec = bot
		cdr.IsContained = true
		cdr.HangupCause = "NORMAL_CLEARING"
		cdr.Legs = []store.Leg{{Kind: "BOT", Label: "novanet_support", DurationSec: bot}}
		cdr.EndedAt = startedAt.Add(secs(bot))
		return cdr, nil
	}
}

// shift is one agent's plausible weekday: sign in, work, wrap up what they
// took, lunch, work, leave.
//
// The after-call intervals are what the agent's own Today card divides by, so
// a seeded day without them shows an occupancy of pure talk and an average
// wrap-up of zero — numbers that look like a broken screen rather than a quiet
// morning.
func shift(dayStart time.Time, agentID uuid.UUID, rng *rand.Rand) []StateLogRow {
	login := dayStart.Add(8*time.Hour + 50*time.Minute + secs(rng.Intn(900)))
	lunch := dayStart.Add(12 * time.Hour).Add(secs(rng.Intn(1800)))
	back := lunch.Add(45 * time.Minute).Add(secs(rng.Intn(600)))
	logout := dayStart.Add(18 * time.Hour).Add(secs(rng.Intn(1200)))
	lunchReason, acwReason := "LUNCH", "AFTER_CALL_WORK"

	rows := []StateLogRow{{AgentID: agentID, State: "READY", EnteredAt: login, ExitedAt: nil}}
	// A handful of wrap-ups through the morning, each cutting the ready
	// interval before it in two.
	at := login
	for i := 0; i < 3+rng.Intn(4); i++ {
		at = at.Add(secs(600 + rng.Intn(1800)))
		if !at.Before(lunch) {
			break
		}
		ends := at.Add(secs(25 + rng.Intn(90)))
		if !ends.Before(lunch) {
			break
		}
		wrapUpAt, wrapUpEnds := at, ends
		rows[len(rows)-1].ExitedAt = &wrapUpAt
		rows = append(rows,
			StateLogRow{AgentID: agentID, State: "NOT_READY", Reason: &acwReason,
				EnteredAt: wrapUpAt, ExitedAt: &wrapUpEnds},
			StateLogRow{AgentID: agentID, State: "READY", EnteredAt: wrapUpEnds, ExitedAt: nil})
		at = ends
	}
	rows[len(rows)-1].ExitedAt = &lunch

	return append(rows,
		StateLogRow{AgentID: agentID, State: "NOT_READY", Reason: &lunchReason, EnteredAt: lunch, ExitedAt: &back},
		StateLogRow{AgentID: agentID, State: "READY", EnteredAt: back, ExitedAt: &logout},
		StateLogRow{AgentID: agentID, State: "LOGGED_OUT", EnteredAt: logout, ExitedAt: nil})
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

func digits(rng *rand.Rand, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('0' + rng.Intn(10))
	}
	return string(out)
}

// deterministicUUID derives ids from the seeded stream, so the whole history
// is byte-stable across installs.
func deterministicUUID(rng *rand.Rand) uuid.UUID {
	var b [16]byte
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return uuid.UUID(b)
}
