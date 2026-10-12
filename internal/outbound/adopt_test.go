// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// A switch that holds channels, and answers the commands reconciliation uses
// the way FreeSWITCH does: `show channels as json` lists rows without channel
// variables, `uuid_dump <uuid> json` returns one channel's headers and
// variable_* entries, and `uuid_kill` ends a channel.
type liveSwitch struct {
	mu       sync.Mutex
	order    []string
	rows     map[string]map[string]any // by channel id; what `show channels` lists
	dumps    map[string]string         // by channel id; the raw reply of uuid_dump
	kills    map[string]string         // channel id -> cause
	killLog  []string
	commands []string
	// extraRows are listed verbatim, malformed or not.
	extraRows []string
	// isStubborn keeps killed channels in the listing, as a switch still
	// clearing them would.
	isStubborn bool
	// truncateAt cuts the listing after this many bytes when positive.
	truncateAt int
}

func newLiveSwitch() *liveSwitch {
	return &liveSwitch{
		rows: map[string]map[string]any{}, dumps: map[string]string{}, kills: map[string]string{},
	}
}

func (l *liveSwitch) IsUp() bool { return true }

func (l *liveSwitch) BgAPI(cmd string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.commands = append(l.commands, cmd)
	return "job-1", nil
}

func (l *liveSwitch) API(cmd string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case cmd == "show channels as json":
		return l.listing(), nil
	case strings.HasPrefix(cmd, "uuid_dump "):
		id := strings.Fields(cmd)[1]
		if d, ok := l.dumps[id]; ok {
			if _, killed := l.kills[id]; killed && !l.isStubborn {
				return "-ERR No such channel!", nil
			}
			return d, nil
		}
		return "-ERR No such channel!", nil
	case strings.HasPrefix(cmd, "uuid_kill "):
		f := strings.Fields(cmd)
		l.kills[f[1]] = f[2]
		l.killLog = append(l.killLog, f[1]+" "+f[2])
		return "+OK", nil
	}
	l.commands = append(l.commands, cmd)
	return "+OK", nil
}

func (l *liveSwitch) listing() string {
	var rows []string
	for _, id := range l.order {
		if _, killed := l.kills[id]; killed && !l.isStubborn {
			continue
		}
		b, _ := json.Marshal(l.rows[id])
		rows = append(rows, string(b))
	}
	rows = append(rows, l.extraRows...)
	out := fmt.Sprintf(`{"row_count":%d,"rows":[%s]}`, len(rows), strings.Join(rows, ","))
	if l.truncateAt > 0 && l.truncateAt < len(out) {
		out = out[:l.truncateAt]
	}
	return out
}

func (l *liveSwitch) killed(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cause, ok := l.kills[id]
	return cause, ok
}

func (l *liveSwitch) bridgeCommands() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, c := range l.commands {
		if strings.HasPrefix(c, "uuid_transfer ") {
			out = append(out, c)
		}
	}
	return out
}

// chanSpec is one channel as the switch holds it.
type chanSpec struct {
	id          uuid.UUID
	inbound     bool
	callState   string
	answered    bool
	application string
	age         time.Duration
	vars        map[string]string
	peer        string
	// bridgedAgo overrides when the bridge began (default: shortly after the
	// channel was created). noBridgeTimes leaves out every timestamp that says
	// when it began, and noSwitchClock leaves out the dump's own clock.
	bridgedAgo    time.Duration
	noBridgeTimes bool
	noSwitchClock bool
}

func (l *liveSwitch) add(now time.Time, c chanSpec) {
	created := now.Add(-c.age)
	state := "ringing"
	if c.answered {
		state = "answered"
	}
	dir := "outbound"
	if c.inbound {
		dir = "inbound"
	}
	h := map[string]string{
		"Unique-ID":                   c.id.String(),
		"Call-Direction":              dir,
		"Answer-State":                state,
		"Channel-Name":                "sofia/external/" + c.id.String()[:8],
		"Caller-Channel-Created-Time": strconv.FormatInt(created.UnixMicro(), 10),
		"Caller-Context":              "aicc",
	}
	if !c.noSwitchClock {
		h["Event-Date-Timestamp"] = strconv.FormatInt(now.UnixMicro(), 10)
	}
	if c.answered && !c.noBridgeTimes {
		answered := created.Add(time.Second)
		if answered.After(now) {
			answered = now
		}
		h["Caller-Channel-Answered-Time"] = strconv.FormatInt(answered.UnixMicro(), 10)
	}
	if c.peer != "" {
		h["Other-Leg-Unique-ID"] = c.peer
		if !c.noBridgeTimes {
			bridged := created.Add(2 * time.Second)
			if c.bridgedAgo > 0 {
				bridged = now.Add(-c.bridgedAgo)
			} else if bridged.After(now) {
				bridged = now
			}
			h["Caller-Channel-Bridged-Time"] = strconv.FormatInt(bridged.UnixMicro(), 10)
		}
	}
	for k, v := range c.vars {
		h["variable_"+k] = v
	}
	if dest := c.vars["aicc_dest"]; dest != "" {
		h["Caller-Destination-Number"] = dest
	}
	raw, _ := json.Marshal(h)
	id := c.id.String()
	l.order = append(l.order, id)
	l.dumps[id] = string(raw)
	l.rows[id] = map[string]any{
		"uuid": id, "direction": dir, "created_epoch": strconv.FormatInt(created.Unix(), 10),
		"state": "CS_EXECUTE", "callstate": c.callState, "application": c.application,
	}
}

// eventLog records what the registry published.
type eventLog struct {
	mu  sync.Mutex
	got []events.Event
}

func (e *eventLog) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
	return ev
}

func (e *eventLog) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.got)
}

type noAgentsAtAll struct{}

func (noAgentsAtAll) AgentAtExtension(string) (uuid.UUID, bool)                { return uuid.Nil, false }
func (noAgentsAtAll) AgentByCallcenterName(string) (uuid.UUID, bool)           { return uuid.Nil, false }
func (noAgentsAtAll) BenchForNoAnswer(context.Context, uuid.UUID)              {}
func (noAgentsAtAll) SetOnCall(context.Context, uuid.UUID, bool, uuid.UUID)    {}
func (noAgentsAtAll) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID) {}

// warnings records WARN records so a test can count what was skipped.
type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnings) WithGroup(string) slog.Handler            { return w }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		w.mu.Lock()
		w.msgs = append(w.msgs, r.Message)
		w.mu.Unlock()
	}
	return nil
}

func (w *warnings) matching(sub string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, m := range w.msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// The clock the service reads, moved by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var t0 = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// recoveryRig is a service, a coordinator and a switch wired together the way
// the application wires them.
type recoveryRig struct {
	svc      *Service
	coord    *telephony.Coordinator
	registry *telephony.Registry
	sw       *liveSwitch
	pub      *eventLog
	clock    *testClock
	sessions map[uuid.UUID]bool
	// cold is true until the first pass has run, as in the application.
	cold bool
	// hasBotFn, when set, replaces the session lookup.
	hasBotFn func(uuid.UUID) bool
}

func newRecoveryRig(t *testing.T) *recoveryRig {
	t.Helper()
	sw := newLiveSwitch()
	adapter := telephony.NewAdapter(sw, "aicc.test")
	pub := &eventLog{}
	registry := telephony.NewRegistry(pub)
	t.Cleanup(registry.Shutdown)
	coord := telephony.NewCoordinator(registry, adapter, noAgentsAtAll{}, pub)

	flowID := uuid.New()
	dids := fakeDIDs{
		{Number: "95099", Language: "en", FlowID: &flowID, IsEnabled: true, AllowInbound: true},
		{Number: "95088", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowInbound: true},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}
	svc := New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s"}, adapter, dids,
		provisioned("1007", "1002"),
		func(context.Context, uuid.UUID) (bool, error) { return false, nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetBotAbandoner(coord.AbandonBot)
	clock := &testClock{t: t0}
	svc.now = clock.now
	return &recoveryRig{svc: svc, coord: coord, registry: registry, sw: sw, pub: pub,
		clock: clock, sessions: map[uuid.UUID]bool{}, cold: true}
}

func (r *recoveryRig) hasBot(id uuid.UUID) bool {
	if r.hasBotFn != nil {
		return r.hasBotFn(id)
	}
	return r.sessions[id]
}

// reconcile is one pass, as the application runs it on a connect.
func (r *recoveryRig) reconcile(t *testing.T) RecoveryReport {
	t.Helper()
	live, err := r.coord.AdoptLiveChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rep := r.svc.Recover(context.Background(), live, r.hasBot, r.cold)
	r.cold = false
	r.svc.MarkReconciled()
	return rep
}

func (r *recoveryRig) pendingLeg(id string) *pendingLeg {
	r.svc.mu.Lock()
	defer r.svc.mu.Unlock()
	return r.svc.pending[id]
}

// aiVars are the variables DialAI stamps on a customer leg.
func aiVars(callID uuid.UUID, did, language, to string) map[string]string {
	return map[string]string{
		"aicc_call_id": callID.String(), "aicc_did": did, "aicc_language": language,
		telephony.VarPlacedBy: telephony.PlacedByDialAI, telephony.VarPlacedTo: to,
	}
}

func waitForTransfer(t *testing.T, sw *liveSwitch, channel string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range sw.bridgeCommands() {
			if strings.HasPrefix(c, "uuid_transfer "+channel+" ") {
				return c
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no transfer was issued for %s", channel)
	return ""
}

//
// The 500-channel world.
//

type world struct {
	rig     *recoveryRig
	rng     *rand.Rand
	byClass map[string][]string // class -> subject channel ids
	bots    map[string]string   // customer channel -> its bot leg
	callOf  map[string]uuid.UUID
	rep     RecoveryReport
	warns   *warnings
	total   int
}

func (w *world) id() uuid.UUID {
	id, _ := uuid.NewRandomFromReader(w.rng)
	return id
}

func (w *world) seconds(lo, hi int) time.Duration {
	return time.Duration(lo+w.rng.Intn(hi-lo+1)) * time.Second
}

func (w *world) number() string { return "1868888" + strconv.Itoa(1000+w.rng.Intn(9000)) }

func (w *world) note(class string, id uuid.UUID, call uuid.UUID) {
	w.byClass[class] = append(w.byClass[class], id.String())
	w.callOf[id.String()] = call
}

// buildWorld generates the switch's table from a seed and runs one pass.
func buildWorld(t *testing.T, seed int64) *world {
	t.Helper()
	rig := newRecoveryRig(t)
	w := &world{rig: rig, rng: rand.New(rand.NewSource(seed)), byClass: map[string][]string{},
		bots: map[string]string{}, callOf: map[string]uuid.UUID{}, warns: &warnings{}}
	sw, now := rig.sw, t0

	langOf := func() (string, string) {
		if w.rng.Intn(2) == 0 {
			return "95099", "en"
		}
		return "95088", "zh"
	}

	for range 60 { // ringing AI legs with window left
		did, lang := langOf()
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, callState: "RINGING", age: w.seconds(3, 50),
			vars: aiVars(c, did, lang, w.number())})
		w.note("ringAIOK", id, c)
	}
	for range 25 { // ringing AI legs whose window ran out while we were away
		did, lang := langOf()
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, callState: "EARLY", age: w.seconds(61, 300),
			vars: aiVars(c, did, lang, w.number())})
		w.note("ringAISpent", id, c)
	}
	for range 15 { // a DID that no longer exists: nothing to rebuild the bridge from
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, callState: "RINGING", age: w.seconds(3, 40),
			vars: aiVars(c, "90000", "en", w.number())})
		w.note("ringAIUnknownDID", id, c)
	}
	for range 20 { // ringing click-to-dial agent legs
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, callState: "RINGING", age: w.seconds(1, 30), vars: map[string]string{
			"aicc_call_id": c.String(), telephony.VarPlacedBy: telephony.PlacedByDial,
			"aicc_destination": w.number(), "aicc_extension": "1007",
		}})
		w.note("ringClick", id, c)
	}
	for range 40 { // answered while nobody watched, still parked
		did, lang := langOf()
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "park",
			age: w.seconds(10, 120), vars: aiVars(c, did, lang, w.number())})
		w.note("answeredParked", id, c)
	}

	bridged := func(class string, n int, inbound bool, botAge func() time.Duration, hasSession bool, ba time.Duration) {
		for range n {
			did, lang := langOf()
			c, cust, bot := w.id(), w.id(), w.id()
			custVars := aiVars(c, did, lang, w.number())
			if inbound {
				custVars = map[string]string{"aicc_call_id": c.String(), "aicc_did": did, "aicc_language": lang}
			}
			sw.add(now, chanSpec{id: cust, inbound: inbound, callState: "ACTIVE", answered: true,
				application: "bridge", age: 90 * time.Second, vars: custVars, peer: bot.String(), bridgedAgo: ba})
			sw.add(now, chanSpec{id: bot, callState: "ACTIVE", answered: true, application: "bridge",
				age: botAge(), peer: cust.String(), bridgedAgo: ba, vars: map[string]string{
					"aicc_call_id": c.String(), "aicc_did": did, "aicc_dest": did,
				}})
			w.note(class, cust, c)
			w.bots[cust.String()] = bot.String()
			if hasSession {
				rig.sessions[c] = true
			}
		}
	}
	old := func() time.Duration { return w.seconds(30, 80) }
	bridged("botDeadOutbound", 30, false, old, false, 0)
	bridged("botDeadInbound", 20, true, old, false, 0)
	bridged("botLive", 25, false, old, true, 0)
	bridged("botYoung", 10, true, func() time.Duration { return time.Second }, false, time.Second)

	for range 25 { // agent phone bridged to a customer who is not the bot
		c, agent, cust := w.id(), w.id(), w.id()
		sw.add(now, chanSpec{id: agent, callState: "ACTIVE", answered: true, application: "bridge",
			age: 60 * time.Second, peer: cust.String(),
			vars: map[string]string{"aicc_call_id": c.String(), "aicc_extension": "1007"}})
		sw.add(now, chanSpec{id: cust, inbound: true, callState: "ACTIVE", answered: true,
			application: "bridge", age: 80 * time.Second, peer: agent.String(),
			vars: map[string]string{"aicc_call_id": c.String()}})
		w.note("agentBridged", agent, c)
	}
	for range 20 { // a caller waiting in a queue
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, inbound: true, callState: "ACTIVE", answered: true,
			application: "callcenter", age: w.seconds(5, 200),
			vars: map[string]string{"aicc_call_id": c.String()}})
		w.note("queueWaiting", id, c)
	}
	for range 50 { // somebody else's channels
		id := w.id()
		sw.add(now, chanSpec{id: id, inbound: w.rng.Intn(2) == 0, callState: "ACTIVE", answered: true,
			application: "bridge", age: w.seconds(1, 500), vars: map[string]string{"sip_from_user": "x"}})
		w.note("foreign", id, uuid.Nil)
	}
	for range 15 { // supervisors listening in
		c, id := w.id(), w.id()
		sw.add(now, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "eavesdrop",
			age: w.seconds(1, 100), vars: map[string]string{
				"aicc_call_id": c.String(), telephony.ObserverVar: "LISTEN",
			}})
		w.note("eavesdrop", id, c)
	}

	// Rows and dumps that cannot be used.
	for range 10 { // already hanging up
		id := w.id()
		sw.add(now, chanSpec{id: id, callState: "HANGUP", age: time.Minute,
			vars: aiVars(w.id(), "95099", "en", w.number())})
		w.note("hangingUp", id, uuid.Nil)
	}
	for range 8 { // listed, gone by the time it is read
		id := w.id()
		sw.add(now, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second,
			vars: aiVars(w.id(), "95099", "en", w.number())})
		delete(sw.dumps, id.String())
		w.note("vanished", id, uuid.Nil)
	}
	for range 5 { // a dump that is not JSON
		id := w.id()
		sw.add(now, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second,
			vars: aiVars(w.id(), "95099", "en", w.number())})
		sw.dumps[id.String()] = `{"Unique-ID": "` + id.String() + `", "variable_aicc_call`
		w.note("unreadableDump", id, uuid.Nil)
	}
	for range 6 {
		sw.extraRows = append(sw.extraRows, `{"direction":"inbound","callstate":"ACTIVE","created_epoch":"1760000000"}`)
	}
	for range 4 {
		sw.extraRows = append(sw.extraRows, `{"uuid":"not-a-uuid","callstate":"ACTIVE"}`)
	}
	for range 6 {
		sw.extraRows = append(sw.extraRows, fmt.Sprintf(`{"uuid":%q,"callstate":"BANANA"}`, w.id().String()))
	}
	for range 3 {
		sw.extraRows = append(sw.extraRows, `"not an object"`)
	}

	// Shuffle the table so classes interleave.
	w.rng.Shuffle(len(sw.order), func(i, j int) { sw.order[i], sw.order[j] = sw.order[j], sw.order[i] })
	w.total = len(sw.order) + len(sw.extraRows)

	prev := slog.Default()
	slog.SetDefault(slog.New(w.warns))
	defer slog.SetDefault(prev)
	w.rep = rig.reconcile(t)
	return w
}

func (w *world) mustBeKilled(t *testing.T, class, cause string) {
	t.Helper()
	if len(w.byClass[class]) == 0 {
		t.Fatalf("the world has no %s channels", class)
	}
	for _, id := range w.byClass[class] {
		got, ok := w.rig.sw.killed(id)
		if !ok {
			t.Errorf("%s %s was not hung up", class, id)
		} else if got != cause {
			t.Errorf("%s %s hung up with %s, want %s", class, id, got, cause)
		}
	}
}

func (w *world) mustBeLeftAlone(t *testing.T, class string) {
	t.Helper()
	if len(w.byClass[class]) == 0 {
		t.Fatalf("the world has no %s channels", class)
	}
	for _, id := range w.byClass[class] {
		if cause, ok := w.rig.sw.killed(id); ok {
			t.Errorf("%s %s was hung up (%s) and should have been left alone", class, id, cause)
		}
		if w.rig.pendingLeg(id) != nil {
			t.Errorf("%s %s was armed and should not have been", class, id)
		}
	}
}

func (w *world) mustBeAdopted(t *testing.T, class string) {
	t.Helper()
	for _, id := range w.byClass[class] {
		if !w.rig.registry.Owns(id) {
			t.Errorf("%s %s was not adopted into the registry", class, id)
		}
	}
}

func (w *world) mustNotBeAdopted(t *testing.T, class string) {
	t.Helper()
	for _, id := range w.byClass[class] {
		if w.rig.registry.Owns(id) {
			t.Errorf("%s %s belongs to somebody else and was adopted", class, id)
		}
	}
}

// The seed is fixed so a failure reproduces.
const worldSeed = 157

func TestTheGeneratedWorldHasAtLeastFiveHundredChannels(t *testing.T) {
	w := buildWorld(t, worldSeed)
	if w.total < 500 {
		t.Fatalf("the generated switch holds %d rows, want at least 500", w.total)
	}
}

func TestRingingAILegWithWindowLeftIsRearmed(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "ringAIOK")
	for _, id := range w.byClass["ringAIOK"] {
		if w.rig.pendingLeg(id) == nil {
			t.Errorf("ringing AI leg %s was not re-armed", id)
		}
		if cause, ok := w.rig.sw.killed(id); ok {
			t.Errorf("ringing AI leg %s was hung up (%s)", id, cause)
		}
	}
}

func TestRearmedLegsKeepWhatIsLeftOfTheirWindow(t *testing.T) {
	w := buildWorld(t, worldSeed)
	for _, id := range w.byClass["ringAIOK"] {
		leg := w.rig.pendingLeg(id)
		if leg == nil {
			t.Fatalf("ringing AI leg %s was not re-armed", id)
		}
		// The window runs from the channel's creation, not from now.
		created := leg.expiresAt.Add(-w.rig.svc.cfg.AnswerWindow)
		age := t0.Sub(created)
		if age < 3*time.Second || age > 50*time.Second {
			t.Errorf("leg %s: window starts %v before now, want its real age", id, age)
		}
		if _, killed := w.rig.sw.killed(id); killed {
			t.Errorf("leg %s was re-armed and also hung up", id)
		}
	}
}

func TestARearmedAILegBridgesTheBotWhenAnswered(t *testing.T) {
	w := buildWorld(t, worldSeed)
	id := w.byClass["ringAIOK"][0]
	call := w.callOf[id]
	w.rig.svc.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelAnswer, ChannelID: id})
	cmd := waitForTransfer(t, w.rig.sw, id)

	did, lang := "95099", "en"
	if strings.Contains(w.rig.sw.dumps[id], `"aicc_did":"95088"`) {
		did, lang = "95088", "zh"
	}
	for _, want := range []string{
		"sofia/gateway/aicc_bot/" + did,
		"sip_h_X-AICC-Call-ID=" + call.String(),
		"sip_h_X-AICC-Channel-ID=" + id,
		"sip_h_X-AICC-Language=" + lang, // the language survives adoption
		"sip_h_X-AICC-Placed-To-Customer=true",
		"sip_h_X-AICC-Call-Type=OUTBOUND",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("bridge command lacks %q:\n%s", want, cmd)
		}
	}
	if _, killed := w.rig.sw.killed(id); killed {
		t.Error("an answered, re-armed leg was hung up")
	}
}

func TestRingingAILegWithWindowSpentIsHungUp(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "ringAISpent")
	w.mustBeKilled(t, "ringAISpent", LateAnswerCause)
}

func TestRingingAILegWithAnUnknownDIDIsHungUpNotGuessed(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeKilled(t, "ringAIUnknownDID", ReconcileCause)
	for _, id := range w.byClass["ringAIUnknownDID"] {
		if w.rig.pendingLeg(id) != nil {
			t.Errorf("leg %s with an unrecoverable bridge target was armed", id)
		}
	}
}

func TestRingingClickToDialLegIsRearmedToTheDestination(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "ringClick")
	id := w.byClass["ringClick"][0]
	if w.rig.pendingLeg(id) == nil {
		t.Fatalf("click-to-dial leg %s was not re-armed", id)
	}
	w.rig.svc.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelAnswer, ChannelID: id})
	cmd := waitForTransfer(t, w.rig.sw, id)
	if !strings.Contains(cmd, "XML aicc") {
		t.Errorf("click-to-dial leg went somewhere other than the dialplan: %s", cmd)
	}
}

func TestAnsweredParkedLegIsHungUpNow(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "answeredParked")
	w.mustBeKilled(t, "answeredParked", ReconcileCause)
}

func TestCustomerLegOnAnOutboundBridgeToAGoneBotIsHungUpWithTheBotLeg(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "botDeadOutbound")
	w.mustBeKilled(t, "botDeadOutbound", ReconcileCause)
	for _, cust := range w.byClass["botDeadOutbound"] {
		if cause, ok := w.rig.sw.killed(w.bots[cust]); !ok || cause != ReconcileCause {
			t.Errorf("bot leg of %s: killed=%v cause=%s", cust, ok, cause)
		}
	}
}

func TestInboundCallerOnAGoneBotIsHungUpToo(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeKilled(t, "botDeadInbound", ReconcileCause)
}

func TestCustomerLegOnALiveBotIsLeftAlone(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "botLive")
	w.mustBeLeftAlone(t, "botLive")
	for _, cust := range w.byClass["botLive"] {
		if _, ok := w.rig.sw.killed(w.bots[cust]); ok {
			t.Errorf("the bot leg of %s was hung up though its session exists", cust)
		}
	}
}

func TestABotLegTooYoungToHaveASessionIsGivenGrace(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeLeftAlone(t, "botYoung")
}

func TestAgentPhoneBridgedToACustomerIsAdoptedAndNotTouched(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "agentBridged")
	w.mustBeLeftAlone(t, "agentBridged")
}

func TestAQueuedCallerIsAdoptedAndNotTouched(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeAdopted(t, "queueWaiting")
	w.mustBeLeftAlone(t, "queueWaiting")
}

func TestChannelsWithoutACallIDAreLeftAlone(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeLeftAlone(t, "foreign")
	w.mustNotBeAdopted(t, "foreign")
}

func TestSupervisorEavesdropLegsAreLeftAlone(t *testing.T) {
	w := buildWorld(t, worldSeed)
	w.mustBeLeftAlone(t, "eavesdrop")
	w.mustNotBeAdopted(t, "eavesdrop")
}

func TestChannelsAlreadyHangingUpOrGoneAreNotActedOn(t *testing.T) {
	w := buildWorld(t, worldSeed)
	for _, class := range []string{"hangingUp", "vanished", "unreadableDump"} {
		w.mustBeLeftAlone(t, class)
		w.mustNotBeAdopted(t, class)
	}
}

func TestMalformedRowsAreSkippedWithAWarningAndNeverAbortThePass(t *testing.T) {
	w := buildWorld(t, worldSeed)
	// The pass ran to the end: classes listed after the malformed rows were
	// still handled.
	w.mustBeKilled(t, "answeredParked", ReconcileCause)

	if got := w.warns.matching("no usable uuid"); got != 6+4 {
		t.Errorf("rows with no usable uuid warned %d times, want 10", got)
	}
	if got := w.warns.matching("state this code does not know"); got != 6 {
		t.Errorf("rows in an unknown state warned %d times, want 6", got)
	}
	if got := w.warns.matching("not an object"); got != 3 {
		t.Errorf("rows that are not objects warned %d times, want 3", got)
	}
	if got := w.warns.matching("dump is unreadable"); got != 5 {
		t.Errorf("unreadable dumps warned %d times, want 5", got)
	}
}

func TestEveryClassIsCountedInTheReport(t *testing.T) {
	w := buildWorld(t, worldSeed)
	want := RecoveryReport{
		Rearmed:        len(w.byClass["ringAIOK"]) + len(w.byClass["ringClick"]),
		WindowSpent:    len(w.byClass["ringAISpent"]),
		AnsweredParked: len(w.byClass["answeredParked"]),
		OrphanedBot:    len(w.byClass["botDeadOutbound"]) + len(w.byClass["botDeadInbound"]),
		Unrecoverable:  len(w.byClass["ringAIUnknownDID"]),
		Held:           len(w.byClass["botYoung"]),
	}
	got := w.rep
	got.Left = 0 // left-alone includes the bot legs and everything else; checked by class above
	if got != want {
		t.Errorf("report = %+v, want %+v", got, want)
	}
}

//
// Passes.
//

func TestAPassIsIdempotent(t *testing.T) {
	w := buildWorld(t, worldSeed)
	rig := w.rig

	killsAfterFirst := len(rig.sw.killLog)
	eventsAfterFirst := rig.pub.count()
	transfersAfterFirst := len(rig.sw.bridgeCommands())
	pendingAfterFirst := rig.svc.PendingCount()
	callsAfterFirst := rig.registry.Count()

	// The switch is still clearing what was hung up, so it lists it again.
	rig.sw.isStubborn = true
	rig.reconcile(t)

	if got := len(rig.sw.killLog); got != killsAfterFirst {
		t.Errorf("a second pass hung up %d more channels", got-killsAfterFirst)
	}
	if got := rig.pub.count(); got != eventsAfterFirst {
		t.Errorf("a second pass published %d events about states that did not change", got-eventsAfterFirst)
	}
	if got := len(rig.sw.bridgeCommands()); got != transfersAfterFirst {
		t.Errorf("a second pass issued %d more bridge commands", got-transfersAfterFirst)
	}
	if got := rig.svc.PendingCount(); got != pendingAfterFirst {
		t.Errorf("a second pass changed the armed legs from %d to %d", pendingAfterFirst, got)
	}
	if got := rig.registry.Count(); got != callsAfterFirst {
		t.Errorf("a second pass changed the registry from %d to %d calls", callsAfterFirst, got)
	}
}

// A switch that is slow to clear a channel it was told to end still lists it
// on the next pass; the pass must not end it a second time.
func TestAChannelStillBeingClearedIsNotHungUpTwice(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.sw.isStubborn = true
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "park",
		age: 30 * time.Second, vars: aiVars(call, "95099", "en", "18688886666")})

	rig.reconcile(t)
	rig.reconcile(t)
	rig.reconcile(t)

	if got := len(rig.sw.killLog); got != 1 {
		t.Errorf("channel was hung up %d times, want once: %v", got, rig.sw.killLog)
	}
}

// What the live process holds is not armed again, whatever the switch says.
func TestALegTheLiveProcessAlreadyHoldsIsNotArmedAgain(t *testing.T) {
	rig := newRecoveryRig(t)
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second,
		vars: aiVars(call, "95099", "en", "18688886666")})

	ran := 0
	rig.svc.armUntil(id.String(), t0.Add(50*time.Second), func() { ran++ })
	before := rig.pendingLeg(id.String())

	rep := rig.reconcile(t)

	if rep.Rearmed != 0 || rep.Held != 1 {
		t.Errorf("report = %+v, want the leg held and nothing re-armed", rep)
	}
	if rig.pendingLeg(id.String()) != before {
		t.Error("the live process's own armed continuation was replaced")
	}
	if !before.expiresAt.Equal(t0.Add(50 * time.Second)) {
		t.Errorf("the live window moved to %v", before.expiresAt)
	}
}

// A customer who answers during an ESL blip: the answer is lost, the process
// still holds what to do on it, and the window is open. They get the bot.
func TestALegAnsweredDuringABlipInsideItsWindowGetsTheBot(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.cold = false // a reconnect: the process kept its memory
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "park",
		age: 20 * time.Second, vars: aiVars(call, "95099", "en", "18688886666")})
	ran := make(chan struct{}, 1)
	rig.svc.armUntil(id.String(), t0.Add(40*time.Second), func() { ran <- struct{}{} })

	rep := rig.reconcile(t)

	select {
	case <-ran:
	default:
		t.Fatal("the held continuation was not run")
	}
	if rep.Resumed != 1 {
		t.Errorf("report = %+v, want one resumed", rep)
	}
	if cause, ok := rig.sw.killed(id.String()); ok {
		t.Errorf("a customer who answered inside the window was hung up (%s)", cause)
	}
	if rig.pendingLeg(id.String()) != nil {
		t.Error("the continuation is still armed after it ran")
	}
	// And a second pass does not run it again.
	rig.reconcile(t)
	select {
	case <-ran:
		t.Error("the continuation ran twice")
	default:
	}
}

// The same blip, but the window has closed: a clean hangup, with the late
// answer's cause, and nothing left armed.
func TestALegAnsweredDuringABlipAfterItsWindowIsHungUp(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.cold = false
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "park",
		age: 70 * time.Second, vars: aiVars(call, "95099", "en", "18688886666")})
	rig.svc.armUntil(id.String(), t0.Add(-10*time.Second), func() { t.Error("a spent continuation ran") })

	rig.reconcile(t)

	if cause, ok := rig.sw.killed(id.String()); !ok || cause != LateAnswerCause {
		t.Errorf("killed=%v cause=%q, want %s", ok, cause, LateAnswerCause)
	}
	if rig.pendingLeg(id.String()) != nil {
		t.Error("a hung-up leg is still armed")
	}
}

// 12 s of the window left when re-armed, answered at 20 s: the window is over,
// so the answer is not bridged and the leg is ended.
func TestARearmedLegAnsweredAfterItsWindowIsNotBridged(t *testing.T) {
	rig := newRecoveryRig(t)
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 48 * time.Second,
		vars: aiVars(call, "95099", "en", "18688886666")})

	rep := rig.reconcile(t)
	if rep.Rearmed != 1 {
		t.Fatalf("report = %+v, want the leg re-armed", rep)
	}
	leg := rig.pendingLeg(id.String())
	if left := leg.expiresAt.Sub(t0); left != 12*time.Second {
		t.Fatalf("re-armed with %v left, want 12s", left)
	}

	rig.clock.advance(20 * time.Second)
	rig.svc.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelAnswer, ChannelID: id.String()})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := rig.sw.killed(id.String()); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if cause, ok := rig.sw.killed(id.String()); !ok || cause != LateAnswerCause {
		t.Errorf("killed=%v cause=%q, want a hangup with %s", ok, cause, LateAnswerCause)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(rig.sw.bridgeCommands()); n != 0 {
		t.Errorf("a late answer was bridged to the bot (%d commands)", n)
	}
}

// And with the same leg answered inside what was left, the bot arrives.
func TestARearmedLegAnsweredInsideItsWindowIsBridged(t *testing.T) {
	rig := newRecoveryRig(t)
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 48 * time.Second,
		vars: aiVars(call, "95099", "en", "18688886666")})
	rig.reconcile(t)

	rig.clock.advance(8 * time.Second)
	rig.svc.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelAnswer, ChannelID: id.String()})
	waitForTransfer(t, rig.sw, id.String())
}

// A truncated listing yields the rows before the cut and does not fail.
func TestATruncatedListingAdoptsWhatArrived(t *testing.T) {
	rig := newRecoveryRig(t)
	var ids []uuid.UUID
	for range 5 {
		id := uuid.New()
		ids = append(ids, id)
		rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second,
			vars: aiVars(uuid.New(), "95099", "en", "18688886666")})
	}
	full := rig.sw.listing()
	rig.sw.truncateAt = strings.Index(full, ids[3].String()) + 20 // cut inside the fourth row

	prev := slog.Default()
	warns := &warnings{}
	slog.SetDefault(slog.New(warns))
	defer slog.SetDefault(prev)
	rep := rig.reconcile(t)

	if rep.Rearmed != 3 {
		t.Errorf("re-armed %d, want the 3 rows before the cut", rep.Rearmed)
	}
	if warns.matching("cut off") == 0 {
		t.Error("a truncated listing did not warn")
	}
}

//
// The admission gate.
//

func TestADialBeforeTheFirstReconcileWaitsForIt(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.svc.HoldUntilReconciled()

	done := make(chan error, 1)
	go func() {
		_, err := rig.svc.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95099"})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("DialAI returned (%v) before the first reconcile finished", err)
	case <-time.After(100 * time.Millisecond):
	}
	rig.sw.mu.Lock()
	n := len(rig.sw.commands)
	rig.sw.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d commands reached the switch before the first reconcile finished", n)
	}

	rig.svc.MarkReconciled()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DialAI never proceeded after the reconcile finished")
	}
}

func TestClickToDialWaitsForTheFirstReconcileToo(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.svc.HoldUntilReconciled()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := rig.svc.Dial(ctx, AgentDialRequest{AgentExtension: "1007", To: "18688886666"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the request's own deadline while the gate is shut", err)
	}
}

func TestADialIsRefusedWhenTheFirstReconcileNeverFinishes(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.svc.HoldUntilReconciled()
	rig.svc.readyWait = 30 * time.Millisecond
	_, err := rig.svc.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95099"})
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}

// Only the first reconcile gates; a service nobody asked to hold is open.
func TestADialIsNotHeldWhenNoGateWasSet(t *testing.T) {
	rig := newRecoveryRig(t)
	if _, err := rig.svc.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95099"}); err != nil {
		t.Fatal(err)
	}
}

func TestMarkingReconciledTwiceIsHarmless(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.svc.HoldUntilReconciled()
	rig.svc.MarkReconciled()
	rig.svc.MarkReconciled()
	if _, err := rig.svc.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95099"}); err != nil {
		t.Fatal(err)
	}
}

//
// Job tracking (jobs.go) and a leg it never placed.
//

// An adopted ringing leg has no tracked originate. Neither the backstop nor a
// stray failure result may write a placement failure for it.
func TestARearmedLegHasNoJobSoThePlacementFailurePathCannotMisfire(t *testing.T) {
	rig := newRecoveryRig(t)
	ledger := &recordingLedger{cdrs: map[uuid.UUID][]store.CDR{}}
	rig.svc.SetFailureLedger(ledger)

	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second,
		vars: aiVars(call, "95099", "en", "18688886666")})
	rig.reconcile(t)

	if rig.svc.JobCount() != 0 {
		t.Errorf("an adopted leg is tracked as %d jobs", rig.svc.JobCount())
	}
	// The result of the pre-restart originate may still arrive, for a job id
	// this process never issued.
	body := "-ERR USER_BUSY\n"
	rig.svc.HandleSwitchEvent(jobEvent("job-from-before-the-restart", body))
	time.Sleep(50 * time.Millisecond)
	if ledger.total() != 0 {
		t.Errorf("a result for a job this process never issued wrote %d ledger rows", ledger.total())
	}
	if rig.pendingLeg(id.String()) == nil {
		t.Error("a stray job result disarmed the adopted leg")
	}
}

// The registry hears each adopted call once and keeps its real timings.
func TestAdoptedCallsKeepTheirSwitchTimings(t *testing.T) {
	rig := newRecoveryRig(t)
	call, cust, bot := uuid.New(), uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: cust, callState: "ACTIVE", answered: true, application: "bridge",
		age: 90 * time.Second, peer: bot.String(), vars: aiVars(call, "95099", "en", "18688886666")})
	rig.sw.add(t0, chanSpec{id: bot, callState: "ACTIVE", answered: true, application: "bridge",
		age: 85 * time.Second, peer: cust.String(),
		vars: map[string]string{"aicc_call_id": call.String(), "aicc_did": "95099", "aicc_dest": "95099"}})
	rig.sessions[call] = true

	rig.reconcile(t)

	snap, err := rig.registry.Snapshot(call)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(-90 * time.Second); !snap.CreatedAt.Equal(want) {
		t.Errorf("call began %v, want the channel's own creation %v", snap.CreatedAt, want)
	}
	if len(snap.Parties) != 2 {
		t.Errorf("parties = %d, want the customer and the bot", len(snap.Parties))
	}
}
