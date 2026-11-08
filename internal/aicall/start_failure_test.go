// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/pprof"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// A bot leg that answered and never started a conversation writes no row of
// its own (#334): the human path writes it, told so by the absence of the
// aicc_bot_started marker, which runCall stamps only after session.Start
// succeeded. These tests drive a real UAS with INVITEs, as the switch does,
// through each place the start can end.

const (
	startFailCallerChannel = "chan-1"
	didWithQueue           = "95001"
	didWithoutQueue        = "95003"
	didNoFlowWithQueue     = "95004"
	didNoFlowNoQueue       = "95005"
	didMissing             = "95999"
)

// startFailCatalog is fakeCatalog whose number list can fail.
type startFailCatalog struct {
	fakeCatalog
	didsErr error
}

func (c *startFailCatalog) DIDs(ctx context.Context) ([]catalog.DID, error) {
	if c.didsErr != nil {
		return nil, c.didsErr
	}
	return c.fakeCatalog.DIDs(ctx)
}

// startFailFlows serves the published spec, or fails.
type startFailFlows struct {
	spec *flow.Spec
	err  error
}

func (f startFailFlows) PublishedSpec(context.Context, uuid.UUID) (*flow.Spec, error) {
	return f.spec, f.err
}

// startErrModel is a model whose Start fails.
type startErrModel struct {
	*fakeModel
	err error
}

func (m startErrModel) Start(context.Context, provider.SessionConfig) error { return m.err }

// startExit is one way the start of a call can end before a conversation.
type startExit struct {
	name string
	// didFor maps whether the scenario wants a fallback queue to the number
	// the INVITE carries.
	didFor func(withQueue bool) string
	// isDIDResolved: the number is known, so a rescue can find its queue.
	isDIDResolved bool
	catalogErr    error
	flowsErr      error
	sessions      func(provider.Profile, *slog.Logger) (provider.VoiceSession, error)
	// wantLog is the orchestrator's word for the end.
	wantLog string
}

func startExits() []startExit {
	withFlow := func(q bool) string {
		if q {
			return didWithQueue
		}
		return didWithoutQueue
	}
	okModel := func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
		return newFakeModel(), nil
	}
	return []startExit{
		{name: "the number is unknown", didFor: func(bool) string { return didMissing }, sessions: okModel},
		{name: "the catalogue cannot be read", didFor: withFlow, catalogErr: errors.New("catalogue down"), sessions: okModel},
		{name: "the number has no flow", didFor: func(q bool) string {
			if q {
				return didNoFlowWithQueue
			}
			return didNoFlowNoQueue
		}, isDIDResolved: true, sessions: okModel},
		{name: "the published spec cannot be read", didFor: withFlow, isDIDResolved: true,
			flowsErr: errors.New("no published revision"), sessions: okModel},
		{name: "no provider client", didFor: withFlow, isDIDResolved: true,
			sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
				return nil, errors.New("no api key")
			}},
		{name: "the session cannot be built", didFor: withFlow, isDIDResolved: true,
			sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) { return nil, nil }},
		{name: "the provider handshake fails", didFor: withFlow, isDIDResolved: true,
			sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
				return startErrModel{newFakeModel(), fmt.Errorf("handshake: %w", context.DeadlineExceeded)}, nil
			}},
	}
}

// startScenario is who the call is for.
type startScenario struct {
	name       string
	withQueue  bool
	headers    string
	wantOps    func(isDIDResolved bool) []string
	wantReason string
}

func startScenarios() []startScenario {
	placed := "X-Aicc-Placed-To-Customer: true\r\nX-Aicc-Call-Type: OUTBOUND\r\nX-Aicc-Language: en\r\n"
	return []startScenario{
		{name: "inbound with a fallback queue", withQueue: true,
			wantOps: func(ok bool) []string {
				if !ok {
					return nil
				}
				return []string{"handOn chan-1", "transfer chan-1→7001"}
			}},
		{name: "inbound without a fallback queue", withQueue: false,
			wantOps: func(bool) []string { return nil }},
		{name: "placed to a customer with a fallback queue", withQueue: true, headers: placed,
			wantReason: "BOT_LEG_FAILED",
			wantOps: func(ok bool) []string {
				if !ok {
					return []string{"stamp chan-1", "prompt chan-1 en"}
				}
				return []string{"stamp chan-1", "handOn chan-1", "transfer chan-1→7001"}
			}},
		{name: "placed to a customer without a queue", withQueue: false, headers: placed,
			wantReason: "BOT_LEG_FAILED",
			wantOps:    func(bool) []string { return []string{"stamp chan-1", "prompt chan-1 en"} }},
		{name: "a campaign call", withQueue: true,
			headers:    placed + "X-Aicc-Campaign-Id: " + "c-1\r\nX-Aicc-Attempt-Id: a-1\r\n",
			wantReason: "BOT_LEG_FAILED",
			wantOps:    func(bool) []string { return []string{"stamp chan-1"} }},
	}
}

// startRig is an orchestrator on a real UAS with every edge a fake.
type startRig struct {
	o      *Orchestrator
	sw     *orderedSwitch
	ledger *fakeLedger
	logs   *syncBuffer
}

func newStartRig(t *testing.T, exit startExit, spec *flow.Spec) *startRig {
	t.Helper()
	open := catalog.Queue{ID: uuid.Must(uuid.NewV7()), Name: "support", ExtNumber: "7001", IsEnabled: true}
	flowID := uuid.Must(uuid.NewV7())
	cat := &startFailCatalog{
		fakeCatalog: fakeCatalog{
			queues: []catalog.Queue{open},
			dids: []catalog.DID{
				{Number: didWithQueue, Language: "en", IsEnabled: true, FlowID: &flowID, FallbackQueueID: &open.ID},
				{Number: didWithoutQueue, Language: "en", IsEnabled: true, FlowID: &flowID},
				{Number: didNoFlowWithQueue, Language: "en", IsEnabled: true, FallbackQueueID: &open.ID},
				{Number: didNoFlowNoQueue, Language: "en", IsEnabled: true},
			},
		},
		didsErr: exit.catalogErr,
	}
	sw := &orderedSwitch{}
	ledger := newFakeLedger()
	logs := &syncBuffer{}
	uasCfg := voice.DefaultConfig()
	uasCfg.SIPHost, uasCfg.SIPPort = "127.0.0.1", 0
	o, err := NewOrchestrator(OrchestratorConfig{
		Catalog:  cat,
		Flows:    startFailFlows{spec: spec, err: exit.flowsErr},
		Switch:   sw,
		Profile:  provider.OpenAIProfile(),
		UAS:      uasCfg,
		Ledger:   ledger,
		Sessions: exit.sessions,
		Logger:   slog.New(newJSONHandler(logs)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &startRig{o: o, sw: sw, ledger: ledger, logs: logs}
}

// nonStampOps is what the switch was told to do about the caller, without the
// channel variables.
func (r *startRig) nonVariableOps() []string {
	return slices.DeleteFunc(r.sw.recorded(), func(op string) bool { return strings.HasPrefix(op, "set ") })
}

// markerIndex is where aicc_bot_started was set among the switch's commands.
func (r *startRig) markerIndex() int {
	return r.sw.indexOf("set " + startFailCallerChannel + " aicc_bot_started=true")
}

func TestABotLegThatNeverStartedAConversationWritesNoRowAndIsRescued(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	for _, exit := range startExits() {
		for _, sc := range startScenarios() {
			t.Run(exit.name+"/"+sc.name, func(t *testing.T) {
				rig := newStartRig(t, exit, spec)
				if err := rig.o.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(rig.o.Stop)

				callID := uuid.Must(uuid.NewV7())
				extra := "X-Aicc-Did: " + exit.didFor(sc.withQueue) + "\r\n" +
					"X-Aicc-Channel-Id: " + startFailCallerChannel + "\r\n" +
					"X-AICC-Call-ID: " + callID.String() + "\r\n" + sc.headers
				sendInviteWith(t, rig.o.uas.LocalPort(), "sip-"+callID.String(), extra)
				awaitLogLine(t, rig.logs, "ai call could not run")
				awaitLogLine(t, rig.logs, "sip bye sent")

				// onCallStarted logs "ai call could not run" after runCall has
				// returned, so the deferred finish has already run: the absence
				// of a row is final.
				if got := rig.ledger.rows(); len(got) != 0 {
					t.Errorf("the bot wrote %d rows for a conversation that never started: %+v", len(got), got)
				}
				if i := rig.markerIndex(); i >= 0 {
					t.Errorf("aicc_bot_started was stamped for a session that never started: %v", rig.sw.recorded())
				}
				wantOps := sc.wantOps(exit.isDIDResolved)
				if got := rig.nonVariableOps(); !slices.Equal(got, wantOps) {
					t.Errorf("rescue ops = %v, want %v", got, wantOps)
				}
				wantStamp := slices.Contains(wantOps, "stamp chan-1")
				if got := rig.sw.indexOf("stamp chan-1") >= 0; got != wantStamp {
					t.Errorf("BOT_LEG_FAILED stamped = %v, want %v", got, wantStamp)
				}

				// The exits after a recorder exists say why they wrote nothing.
				lines := parseLogLines(t, rig.logs)
				var saidStandDown bool
				for _, l := range lines {
					if l.msg() == "the conversation never started; the human path writes this call's row" {
						saidStandDown = true
					}
				}
				if got, want := saidStandDown, exit.isDIDResolved && exit.flowsErr == nil &&
					!strings.Contains(exit.name, "no flow"); got != want {
					t.Errorf("stand-down line logged = %v, want %v", got, want)
				}
				assertEveryDialogLineCarries(t, lines, "sip-"+callID.String(), callID)
			})
		}
	}
}

// assertEveryDialogLineCarries checks that each line about the dialog holds
// the domain callId, the line of an INVITE that carried one.
func assertEveryDialogLineCarries(t *testing.T, lines []logLine, sipCallID string, callID uuid.UUID) {
	t.Helper()
	var n int
	for _, l := range lines {
		if strings.HasPrefix(l.msg(), "sip uas") {
			continue
		}
		if sip, _ := l.attrs["sipCallId"].(string); sip != sipCallID {
			continue
		}
		n++
		if got, _ := l.attrs["callId"].(string); got != callID.String() {
			t.Errorf("%q carries callId %q, want %q: %s", l.msg(), got, callID, l.raw)
		}
	}
	if n == 0 {
		t.Error("no line about the dialog was checked")
	}
}

// The rescue says what it did, or why it did nothing, on the dialog's call id.
func TestTheRescueNamesEveryOutcomeWithTheCallID(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	failing := startExit{name: "x", isDIDResolved: true,
		sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
			return nil, errors.New("no api key")
		}}
	for _, tc := range []struct {
		name    string
		did     string
		headers string
		catErr  error
		want    string
	}{
		{"inbound with no fallback queue", didWithoutQueue, "", nil, "no fallback queue for the number"},
		{"inbound with an unknown number", didMissing, "", nil, "could not read the number"},
		{"inbound with an unreadable catalogue", didWithQueue, "", errors.New("catalogue down"), "could not read the number"},
		{"inbound with a fallback queue", didWithQueue, "", nil, "transferring the caller to the fallback queue"},
		{"placed with a campaign", didWithQueue,
			"X-Aicc-Placed-To-Customer: true\r\nX-Aicc-Campaign-Id: c-1\r\n", nil, "campaign call"},
		{"placed with no queue", didWithoutQueue, "X-Aicc-Placed-To-Customer: true\r\n", nil, "no fallback queue; playing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit := failing
			exit.catalogErr = tc.catErr
			rig := newStartRig(t, exit, spec)
			if err := rig.o.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(rig.o.Stop)
			callID := uuid.Must(uuid.NewV7())
			sendInviteWith(t, rig.o.uas.LocalPort(), "sip-"+callID.String(),
				"X-Aicc-Did: "+tc.did+"\r\nX-Aicc-Channel-Id: chan-1\r\nX-AICC-Call-ID: "+callID.String()+"\r\n"+tc.headers)
			awaitLogLine(t, rig.logs, "sip bye sent")

			var found bool
			for _, l := range parseLogLines(t, rig.logs) {
				if !strings.Contains(l.msg(), tc.want) {
					continue
				}
				found = true
				if got, _ := l.attrs["callId"].(string); got != callID.String() {
					t.Errorf("%q: callId = %q, want %q", l.msg(), got, callID)
				}
				if got, _ := l.attrs["sipCallId"].(string); got != "sip-"+callID.String() {
					t.Errorf("%q: sipCallId = %q", l.msg(), got)
				}
			}
			if !found {
				t.Errorf("no log line containing %q:\n%s", tc.want, rig.logs.String())
			}
		})
	}
}

// Exit 5: the leg answered and its media did not start. runCall never runs, so
// nothing is written or stamped by the bot, and the rescue is the same.
func TestAMediaFailureWritesNoBotRowAndStampsNoMarker(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range startScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			rig := newStartRig(t, startExits()[4], spec)
			callID := uuid.Must(uuid.NewV7())
			h := map[string]string{
				headerChannelID: startFailCallerChannel, headerDID: didWithoutQueue, headerCallID: callID.String(),
				headerLanguage: "en",
			}
			if sc.withQueue {
				h[headerDID] = didWithQueue
			}
			if strings.Contains(sc.headers, "Placed-To-Customer") {
				h[headerPlacedToCustomer] = "true"
				h[headerCallType] = "OUTBOUND"
			}
			if strings.Contains(sc.headers, "Campaign") {
				h[headerCampaignID] = "c-1"
			}
			dialog := &voice.Dialog{
				SIPCallID: "sip-" + callID.String(), CustomHeaders: h,
				Stopped: make(chan struct{}), RTP: voice.NewRTPSession(0, media.LawMu, 101, nil),
			}
			rig.o.uas.OnCallFailed(dialog, context.DeadlineExceeded)

			if rows := rig.ledger.rows(); len(rows) != 0 {
				t.Errorf("rows = %v, want none", rows)
			}
			if rig.markerIndex() >= 0 {
				t.Errorf("the marker was stamped: %v", rig.sw.recorded())
			}
			if got, want := rig.nonVariableOps(), sc.wantOps(true); !slices.Equal(got, want) {
				t.Errorf("ops = %v, want %v", got, want)
			}
			lines := parseLogLines(t, rig.logs)
			var failed bool
			for _, l := range lines {
				if l.msg() == "bot leg failed before media" {
					failed = true
				}
			}
			if !failed {
				t.Error("no 'bot leg failed before media' line")
			}
			for _, l := range lines {
				if got, _ := l.attrs["callId"].(string); got != callID.String() {
					t.Errorf("%q: callId = %q, want %q", l.msg(), got, callID)
				}
			}
		})
	}
}

// Controls: a conversation that started stamps the marker, only after Start,
// and keeps writing its own row.
func TestAStartedConversationStampsTheMarkerAfterStartAndWritesItsRow(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		headers  string
		wantRows int
	}{
		{"an inbound call the bot keeps", "", 1},
		{"a call an agent placed to the bot number", "X-Aicc-Call-Type: OUTBOUND\r\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var startedAt time.Time
			model := newFakeModel()
			exit := startExit{sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
				return &startRecordingModel{fakeModel: model, onStart: func() { startedAt = time.Now() }}, nil
			}}
			rig := newStartRig(t, exit, spec)
			if err := rig.o.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(rig.o.Stop)
			callID := uuid.Must(uuid.NewV7())
			baseline := runCallFrames(t)
			sendInviteWith(t, rig.o.uas.LocalPort(), "sip-"+callID.String(),
				"X-Aicc-Did: "+didWithQueue+"\r\nX-Aicc-Channel-Id: chan-1\r\nX-AICC-Call-ID: "+callID.String()+"\r\n"+tc.headers)
			awaitLogLine(t, rig.logs, "ai conversation started")

			if rig.markerIndex() < 0 {
				t.Fatalf("aicc_bot_started was not stamped: %v", rig.sw.recorded())
			}
			if startedAt.IsZero() {
				t.Error("the marker was stamped but Start never ran")
			}
			// The marker follows the stamps that began the call.
			if flowAt := rig.sw.indexOf("set chan-1 aicc_flow_id"); flowAt < 0 || flowAt > rig.markerIndex() {
				t.Errorf("the marker came before the call's flow stamp: %v", rig.sw.recorded())
			}

			rig.o.Stop()
			awaitLogLine(t, rig.logs, "sip bye sent")
			var rows []store.CDR
			if tc.wantRows > 0 {
				// One finish, one write: the row appearing is the call's end.
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) && len(rows) < tc.wantRows {
					rows = rig.ledger.rows()
					time.Sleep(5 * time.Millisecond)
				}
			} else {
				// The silent case: finish returns without a log line or a write,
				// and nothing per call follows it. Wait for this test's runCall
				// frame to go, counted against the frames already on the stacks
				// before the call began.
				awaitRunCallFramesAtMost(t, baseline)
			}
			rows = rig.ledger.rows()
			if got := len(rows); got != tc.wantRows {
				t.Fatalf("rows = %d, want %d", got, tc.wantRows)
			}
			if tc.wantRows == 1 {
				if legs := rows[0].Legs; len(legs) != 1 || legs[0].Kind != "BOT" {
					t.Errorf("legs = %+v, want the bot alone", legs)
				}
			}
		})
	}
}

// startRecordingModel notes when Start ran.
type startRecordingModel struct {
	*fakeModel
	onStart func()
}

func (m *startRecordingModel) Start(ctx context.Context, cfg provider.SessionConfig) error {
	m.onStart()
	return m.fakeModel.Start(ctx, cfg)
}

// A recorder whose session never started writes nothing and says why.
func TestARecorderWhoseSessionNeverStartedStandsDown(t *testing.T) {
	ledger := newFakeLedger()
	logs := &syncBuffer{}
	recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	recorder.finish(ledger, testFacts(), slog.New(newJSONHandler(logs)))
	if rows := ledger.rows(); len(rows) != 0 {
		t.Fatalf("rows = %v, want none", rows)
	}
	awaitLogLine(t, logs, "the conversation never started; the human path writes this call's row")

	// A failure after the start is still the bot's row.
	recorder.markStarted()
	recorder.markFailed("PROVIDER_ERROR")
	recorder.finish(ledger, testFacts(), discard())
	if rows := ledger.rows(); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 for a started session that failed", len(rows))
	}
}

// A reason without a summary is still the bot's reason.
func TestABotReasonWithoutASummaryIsKept(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newStartedRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	recorder.mu.Lock()
	recorder.transferReason = "SESSION_LIMIT"
	recorder.mu.Unlock()
	recorder.finish(ledger, testFacts(), discard())
	rows := ledger.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	ud := rows[0].UserData
	if got := ud["botReason"]; got != "SESSION_LIMIT" {
		t.Errorf("botReason = %v, want SESSION_LIMIT (userData %v)", got, ud)
	}
	if _, has := ud["botSummary"]; has {
		t.Errorf("botSummary written without a summary: %v", ud)
	}
}

// runCallFrames counts the Orchestrator.runCall frames on all goroutines.
func runCallFrames(t *testing.T) int {
	t.Helper()
	var b strings.Builder
	if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
		t.Fatal(err)
	}
	return strings.Count(b.String(), "(*Orchestrator).runCall(")
}

// awaitRunCallFramesAtMost waits until at most n runCall frames remain. It is
// used only where no per-call signal follows runCall's deferred finish (an
// agent-placed call, which finish skips silently); n is the count taken before
// the call began, so frames other tests left behind do not hold it up.
func awaitRunCallFramesAtMost(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runCallFrames(t) <= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the call was still running")
}

func newJSONHandler(b *syncBuffer) slog.Handler {
	return slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// A call the bot hands to a person: the handover's stamps follow the marker.
func TestAHandoverStampsFollowTheStartedMarker(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	model := newFakeModel()
	rig := newStartRig(t, startExit{sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
		return model, nil
	}}, spec)
	if err := rig.o.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rig.o.Stop)
	callID := uuid.Must(uuid.NewV7())
	sendInviteWith(t, rig.o.uas.LocalPort(), "sip-"+callID.String(),
		"X-Aicc-Did: "+didWithQueue+"\r\nX-Aicc-Channel-Id: chan-1\r\nX-AICC-Call-ID: "+callID.String()+"\r\n")
	awaitLogLine(t, rig.logs, "ai conversation started")

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_1",
		ToolName: flow.ToolTransferToAgent, ToolArgs: `{"queue":"support","reason":"BILLING","summary":"wants a refund"}`}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}

	deadline := time.Now().Add(3 * time.Second)
	for rig.sw.indexOf("set chan-1 aicc_bot_summary") < 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	summaryAt := rig.sw.indexOf("set chan-1 aicc_bot_summary")
	if summaryAt < 0 {
		t.Fatalf("the handover never stamped its summary: %v", rig.sw.recorded())
	}
	if m := rig.markerIndex(); m < 0 || m > summaryAt {
		t.Errorf("the marker (%d) must precede the handover's summary (%d): %v", m, summaryAt, rig.sw.recorded())
	}
}
