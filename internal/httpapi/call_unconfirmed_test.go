// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// stalledSwitch reads every command and answers none: each one is on its
// socket, in the order it was written, to be run when the switch catches up,
// and the caller is told esl.ErrTimeout as the ESL client tells it.
type stalledSwitch struct {
	mu     sync.Mutex
	socket []string
}

func (s *stalledSwitch) API(cmd string) (string, error) {
	s.mu.Lock()
	s.socket = append(s.socket, cmd)
	s.mu.Unlock()
	return "", fmt.Errorf("%w: %s", esl.ErrTimeout, cmd)
}

func (s *stalledSwitch) BgAPI(cmd string) (string, error) { return s.API(cmd) }
func (s *stalledSwitch) IsUp() bool                       { return true }

type nullHub struct{}

func (nullHub) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event { return ev }

// 50 calls, each with an agent who presses hang up three times and transfer
// three times, in a seeded random order and all at once, while the switch is
// stalled (#303). Every answer is one the operation declares and none is 500;
// each caller is transferred at most once whatever the switch later runs; and
// when the switch catches up and runs everything in the order it was written,
// every call ends exactly once, which is one CDR per call.
func TestAStalledSwitchNeitherFailsNorDuplicatesCallControl(t *testing.T) {
	t.Parallel()
	const calls = 50
	rng := rand.New(rand.NewSource(303))

	sw := &stalledSwitch{}
	registry := telephony.NewRegistry(nullHub{})
	t.Cleanup(registry.Shutdown)
	var finishedMu sync.Mutex
	finished := map[uuid.UUID]int{}
	registry.OnCallFinished = func(snap telephony.Snapshot) {
		finishedMu.Lock()
		finished[snap.CallID]++
		finishedMu.Unlock()
	}
	coordinator := telephony.NewCoordinator(registry, telephony.NewAdapter(sw, "aicc.test"), nil, nullHub{})
	srv := &Server{calls: coordinator, agents: dialerPresence{}}

	type seat struct {
		callID, agentID uuid.UUID
		caller, agent   string
	}
	seats := make([]seat, calls)
	byChannel := map[string]int{}
	at := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	for i := range seats {
		st := seat{callID: uuid.Must(uuid.NewV7()), agentID: uuid.Must(uuid.NewV7()),
			caller: fmt.Sprintf("caller-%d", i), agent: fmt.Sprintf("agent-%d", i)}
		seats[i] = st
		byChannel[st.caller], byChannel[st.agent] = i, i
		if _, err := registry.CreateCall(t.Context(), st.callID, events.CallTypeInbound, "en", true, at); err != nil {
			t.Fatal(err)
		}
		for _, ch := range []string{st.caller, st.agent} {
			if err := registry.BindChannel(ch, st.callID); err != nil {
				t.Fatal(err)
			}
		}
		if err := registry.Do(st.callID, func(call *telephony.Call) {
			caller := call.AddParty(st.caller, "13800138000", at)
			caller.State = telephony.PartyTalking
			agent := call.AddParty(st.agent, "1001", at)
			agent.State = telephony.PartyTalking
			agent.AgentID = &st.agentID
		}); err != nil {
			t.Fatal(err)
		}
	}

	type press struct {
		seat       int
		isTransfer bool
	}
	var presses []press
	for i := range seats {
		for range 3 {
			presses = append(presses, press{i, false}, press{i, true})
		}
	}
	rng.Shuffle(len(presses), func(a, b int) { presses[a], presses[b] = presses[b], presses[a] })

	type answer struct {
		press
		status int
		code   string
	}
	answers := make([]answer, len(presses))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for n, p := range presses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st := seats[p.seat]
			agent := AuthContext{Kind: SubjectUser, SubjectID: st.agentID, AgentID: st.agentID,
				scopes: grantedScopes(auth.RoleAgent)}
			body := ""
			if p.isTransfer {
				body = `{"destination":"7001"}`
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+st.callID.String(), strings.NewReader(body))
			r = r.WithContext(contextWithAuth(r.Context(), agent))
			w := httptest.NewRecorder()
			if p.isTransfer {
				srv.TransferCall(w, r, st.callID)
			} else {
				srv.HangupCall(w, r, st.callID)
			}
			var e errorEnvelope
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			answers[n] = answer{p, w.Code, string(e.Error.Code)}
		}()
	}
	close(start)
	wg.Wait()

	accepted := make([]int, calls)
	for _, a := range answers {
		switch {
		case !a.isTransfer && a.status != http.StatusAccepted:
			t.Errorf("a hangup on a stalled switch answered %d %s, want 202", a.status, a.code)
		case a.isTransfer && a.status == http.StatusAccepted:
			accepted[a.seat]++
		case a.isTransfer && (a.status != http.StatusConflict || a.code != string(CodeTransferInProgress)):
			t.Errorf("a repeated transfer answered %d %s, want 409 TRANSFER_IN_PROGRESS", a.status, a.code)
		}
	}
	for i, n := range accepted {
		if n != 1 {
			t.Errorf("call %d: %d transfers accepted, want exactly one", i, n)
		}
	}

	// The switch catches up and runs what it read, in order. A transfer moves
	// the caller out of the agent's bridge and releases the agent's leg; a
	// kill ends its leg, and the caller bridged to it with it
	// (hangup_after_bridge); a command for a leg already gone does nothing.
	sw.mu.Lock()
	socket := append([]string(nil), sw.socket...)
	sw.mu.Unlock()
	up := map[string]bool{}
	bridged := map[int]bool{}
	for i, st := range seats {
		up[st.caller], up[st.agent] = true, true
		bridged[i] = true
	}
	transfers := make([]int, calls)
	hangup := func(ch string) {
		if !up[ch] {
			return
		}
		up[ch] = false
		registry.Dispatch(telephony.SwitchEvent{Kind: telephony.KindChannelHangup, ChannelID: ch,
			HangupCause: "NORMAL_CLEARING", OccurredAt: at.Add(time.Minute)})
	}
	for _, cmd := range socket {
		fields := strings.Fields(cmd)
		i := byChannel[fields[1]]
		st := seats[i]
		switch fields[0] {
		case "uuid_transfer":
			transfers[i]++
			if up[st.caller] && bridged[i] {
				bridged[i] = false
				registry.Dispatch(telephony.SwitchEvent{Kind: telephony.KindChannelUnbridge,
					ChannelID: st.caller, OtherChannelID: st.agent, OccurredAt: at.Add(time.Minute)})
				hangup(st.agent)
			}
		case "uuid_kill":
			hangup(fields[1])
			if bridged[i] {
				hangup(st.caller)
			}
		default:
			t.Fatalf("the switch was sent %q", cmd)
		}
	}
	for i, n := range transfers {
		if n > 1 {
			t.Errorf("call %d: the switch ran %d transfers of one caller", i, n)
		}
	}
	// Callers moved to the queue hang up there in the end.
	for _, st := range seats {
		hangup(st.caller)
	}

	deadline := time.Now().Add(5 * time.Second)
	for registry.Count() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := registry.Count(); n != 0 {
		t.Fatalf("%d calls never ended", n)
	}
	finishedMu.Lock()
	defer finishedMu.Unlock()
	for i, st := range seats {
		if finished[st.callID] != 1 {
			t.Errorf("call %d ended %d times, want once", i, finished[st.callID])
		}
	}
}
