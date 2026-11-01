// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// stalledCommander is a switch that reads what it is sent and answers none of
// it: every command reaches its socket (sent) and comes back esl.ErrTimeout,
// the way the ESL client reports a reply that never came. answer, when set,
// replaces that for the commands it returns true for.
type stalledCommander struct {
	mu     sync.Mutex
	sent   []string
	answer func(cmd string) (reply string, err error, ok bool)
}

func (s *stalledCommander) API(cmd string) (string, error) {
	s.mu.Lock()
	s.sent = append(s.sent, cmd)
	answer := s.answer
	s.mu.Unlock()
	if answer != nil {
		if reply, err, ok := answer(cmd); ok {
			return reply, err
		}
	}
	return "", fmt.Errorf("%w: %s", esl.ErrTimeout, cmd)
}

func (s *stalledCommander) BgAPI(cmd string) (string, error) { return s.API(cmd) }
func (s *stalledCommander) IsUp() bool                       { return true }

func (s *stalledCommander) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, cmd := range s.sent {
		if strings.HasPrefix(cmd, prefix) {
			n++
		}
	}
	return n
}

// manualClock is the coordinator's time, moved by the test.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// stalledCall is a caller talking to an agent, on a switch that has stopped
// answering.
type stalledCall struct {
	c       *Coordinator
	cmd     *stalledCommander
	pub     *capturingPublisher
	clock   *manualClock
	callID  uuid.UUID
	agentID uuid.UUID
}

func onAStalledCall(t *testing.T) *stalledCall {
	t.Helper()
	cmd := &stalledCommander{}
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, NewAdapter(cmd, "aicc.test"), noAgents{}, pub)
	clock := &manualClock{now: testTime}
	c.now = clock.Now

	callID, agentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := registry.CreateCall(t.Context(), callID, events.CallTypeInbound, "en", true, testTime); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"caller-chan", "agent-chan"} {
		if err := registry.BindChannel(ch, callID); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Do(callID, func(call *Call) {
		caller := call.AddParty("caller-chan", "13800138000", testTime)
		caller.State = PartyTalking
		agent := call.AddParty("agent-chan", "1001", testTime)
		agent.State = PartyTalking
		agent.AgentID = &agentID
	}); err != nil {
		t.Fatal(err)
	}
	return &stalledCall{c: c, cmd: cmd, pub: pub, clock: clock, callID: callID, agentID: agentID}
}

// runTransferLate is the switch catching up on a transfer of the caller: the
// caller leaves the agent's bridge for the queue, and the agent's leg, left
// with nobody, is released.
func (s *stalledCall) runTransferLate() {
	at := testTime.Add(time.Minute)
	s.c.registry.Dispatch(SwitchEvent{Kind: KindChannelUnbridge, ChannelID: "caller-chan",
		OtherChannelID: "agent-chan", OccurredAt: at})
	s.c.registry.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "agent-chan",
		HangupCause: "NORMAL_CLEARING", OccurredAt: at})
}

// sync waits until the call's actor has applied what was dispatched before it.
func (s *stalledCall) sync(t *testing.T) {
	t.Helper()
	if _, err := s.c.registry.Snapshot(s.callID); err != nil {
		t.Fatalf("the call is gone: %v", err)
	}
}

// A transfer the switch did not answer (#303) keeps the caller's leg claimed,
// against the agent trying again and against the bot, until the switch settles
// it: late, by its events, or never, by the bound. Whenever the switch runs it,
// one transfer is all it ever has: N is when it catches up, on the clock the
// claim is measured on.
func TestATransferTheSwitchDidNotConfirmIsSentOnce(t *testing.T) {
	t.Parallel()
	never := time.Duration(-1)
	for _, n := range []time.Duration{500 * time.Millisecond, 5 * time.Second, 30 * time.Second, never} {
		name := n.String()
		if n == never {
			name = "never"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := onAStalledCall(t)
			ctx := t.Context()

			err := s.c.Transfer(ctx, s.callID, s.agentID, "7001")
			if !errors.Is(err, esl.ErrTimeout) {
				t.Fatalf("the unanswered transfer returned %v, want esl.ErrTimeout", err)
			}
			if err := s.c.Transfer(ctx, s.callID, s.agentID, "7001"); !errors.Is(err, ErrTransferInFlight) {
				t.Fatalf("a second transfer while the first is unconfirmed returned %v, want ErrTransferInFlight", err)
			}
			if err := s.c.TransferLeg("caller-chan", "7002", ""); !errors.Is(err, ErrTransferInFlight) {
				t.Fatalf("the bot's transfer of the same leg returned %v, want ErrTransferInFlight", err)
			}

			if n == never {
				// Nothing ever comes: the claim lapses at its bound, and only
				// then may the agent pass the caller on again.
				s.clock.advance(transferInFlightFor - time.Second)
				if err := s.c.Transfer(ctx, s.callID, s.agentID, "7001"); !errors.Is(err, ErrTransferInFlight) {
					t.Fatalf("a transfer inside the bound returned %v, want ErrTransferInFlight", err)
				}
				s.clock.advance(2 * time.Second)
				if err := s.c.Transfer(ctx, s.callID, s.agentID, "7001"); !errors.Is(err, esl.ErrTimeout) {
					t.Fatalf("a transfer after the bound returned %v, want it sent", err)
				}
				if got := s.cmd.count("uuid_transfer"); got != 2 {
					t.Fatalf("%d transfers sent, want the first and the one after the bound", got)
				}
				return
			}

			s.clock.advance(n)
			if err := s.c.Transfer(ctx, s.callID, s.agentID, "7001"); !errors.Is(err, ErrTransferInFlight) {
				t.Fatalf("a transfer %s later returned %v, want ErrTransferInFlight", n, err)
			}
			s.runTransferLate()
			waitFor(t, func() bool { return s.pub.has(events.TypePartyReleased) })

			// The agent has been passed on with the caller: nothing left to
			// transfer from, and nothing more was sent.
			if err := s.c.Transfer(ctx, s.callID, s.agentID, "7001"); !errors.Is(err, ErrNotCallParty) {
				t.Fatalf("the agent's transfer after the late one ran returned %v, want ErrNotCallParty", err)
			}
			if got := s.cmd.count("uuid_transfer"); got != 1 {
				t.Fatalf("%d transfers sent, want 1", got)
			}
			// The agent's own stream says their leg ended, once.
			released := 0
			for i, ev := range s.pub.all() {
				if ev.Type != events.TypePartyReleased {
					continue
				}
				released++
				s.pub.mu.Lock()
				scope := s.pub.scopes[i]
				s.pub.mu.Unlock()
				if len(scope.AgentIDs) != 1 || scope.AgentIDs[0] != s.agentID {
					t.Errorf("PARTY_RELEASED went to %v, want the agent alone", scope.AgentIDs)
				}
			}
			if released != 1 {
				t.Errorf("%d PARTY_RELEASED, want the agent's one", released)
			}
		})
	}
}

// The switch answering settles a transfer whichever way it answers: one it
// ran or refused is not on its way, and the next is sent. Only the reply that
// never came holds the leg.
func TestATransferTheSwitchAnsweredHoldsNothing(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]struct {
		reply string
		err   error
	}{
		"confirmed":       {reply: "+OK"},
		"no such channel": {reply: "-ERR No such channel!"},
		"link down":       {err: fmt.Errorf("%w: not sent", esl.ErrDown)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := onAStalledCall(t)
			s.cmd.answer = func(string) (string, error, bool) { return answer.reply, answer.err, true }
			for range 2 {
				_ = s.c.Transfer(t.Context(), s.callID, s.agentID, "7001")
			}
			if got := s.cmd.count("uuid_transfer"); got != 2 {
				t.Fatalf("%d transfers sent, want both: an answered one holds nothing", got)
			}
		})
	}
}

// The switch can run a transfer and lose only its reply: the caller's events
// arrive while the command is still waiting for it. The timeout that follows
// must not claim the leg again for a transfer that has already happened.
func TestATransferSettledWhileItsReplyWasAwaitedStaysSettled(t *testing.T) {
	t.Parallel()
	s := onAStalledCall(t)
	isFirst := true
	s.cmd.answer = func(cmd string) (string, error, bool) {
		if !isFirst {
			return "", nil, false
		}
		isFirst = false
		// The caller moves before the reply is given up on.
		s.c.registry.Dispatch(SwitchEvent{Kind: KindChannelUnbridge, ChannelID: "caller-chan",
			OtherChannelID: "agent-chan", OccurredAt: testTime})
		s.sync(t)
		return "", fmt.Errorf("%w: %s", esl.ErrTimeout, cmd), true
	}
	if err := s.c.Transfer(t.Context(), s.callID, s.agentID, "7001"); !errors.Is(err, esl.ErrTimeout) {
		t.Fatalf("transfer: %v", err)
	}
	if err := s.c.TransferLeg("caller-chan", "7002", ""); !errors.Is(err, esl.ErrTimeout) {
		t.Fatalf("a transfer after the first was seen to run returned %v, want it sent", err)
	}
	if got := s.cmd.count("uuid_transfer"); got != 2 {
		t.Fatalf("%d transfers sent, want 2", got)
	}
}

// A claim is the leg's, so it moves with the leg when two calls turn out to be
// one: the second transfer is refused on the call the leg now belongs to.
func TestATransferOnItsWayMovesWithItsLegInAMerge(t *testing.T) {
	t.Parallel()
	s := onAStalledCall(t)
	if err := s.c.Transfer(t.Context(), s.callID, s.agentID, "7001"); !errors.Is(err, esl.ErrTimeout) {
		t.Fatalf("transfer: %v", err)
	}
	kept := uuid.Must(uuid.NewV7())
	if _, err := s.c.registry.CreateCall(t.Context(), kept, events.CallTypeInbound, "en", true, testTime); err != nil {
		t.Fatal(err)
	}
	s.c.merge(t.Context(), kept, s.callID)
	if err := s.c.TransferLeg("caller-chan", "7002", ""); !errors.Is(err, ErrTransferInFlight) {
		t.Fatalf("a transfer of the moved leg returned %v, want ErrTransferInFlight", err)
	}
}
