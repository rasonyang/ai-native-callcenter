// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"fmt"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// unansweredBridge is an AI call answered on a switch that stalls on the
// bridge to the bot: the command is sent and its reply never comes. The
// service's windows run on manual timers.
func unansweredBridge(t *testing.T) (*Service, *fakeSwitch, *voice.BotSlots, *manualTimers, string, string) {
	t.Helper()
	s, sw, slots := slotted(t, 2, 0, time.Second)
	timers := &manualTimers{}
	s.timers = timers.after
	sw.bridgeErr = fmt.Errorf("%w: uuid_transfer", esl.ErrTimeout)
	key, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitBridges(t, sw, 1)
	waitUntil(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.bridges[leg] != nil
	})
	return s, sw, slots, timers, key, leg
}

func botLegOf(t *testing.T, s *Service, leg string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	pb := s.bridges[leg]
	if pb == nil {
		t.Fatal("no bridge is pending")
	}
	return pb.botLeg
}

// The bridge to the bot timed out (#303). Its slot stays reserved, and the
// switch decides: a bridge run late (N after the timeout, inside the window)
// reaches the bot, whose INVITE takes the slot it was kept for; one run after
// the window, or never, finds the reservation withdrawn, and the answered leg
// is hung up with BOT_BRIDGE_UNCONFIRMED stamped on it. Either way the slots
// are all free once the call is over.
func TestABridgeToTheBotTheSwitchDidNotConfirmIsSettledByTheSwitch(t *testing.T) {
	t.Parallel()
	never := time.Duration(-1)
	for _, n := range []time.Duration{500 * time.Millisecond, 5 * time.Second, 30 * time.Second, never} {
		name := n.String()
		if n == never {
			name = "never"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, sw, slots, timers, key, leg := unansweredBridge(t)
			if got := slots.Reserved(); got != 1 {
				t.Fatalf("%d reserved after the bridge timed out, want the leg's slot kept", got)
			}
			if h := sw.hangupList(); len(h) != 0 {
				t.Fatalf("an unconfirmed bridge hung the leg up at once: %+v", h)
			}

			isInWindow := n != never && n < bridgeUnconfirmedFor
			if isInWindow {
				// The switch catches up: the bot leg it dials appears, and
				// its INVITE reaches the bot.
				s.HandleSwitchEvent(telephony.SwitchEvent{
					Kind: telephony.KindChannelCreate, ChannelID: botLegOf(t, s, leg)})
				if slots.IsWithdrawn(key) || !slots.Consume(key) {
					t.Fatal("the late INVITE did not find its reservation")
				}
				timers.fire(t) // stopped: the bridge is settled
				if h := sw.hangupList(); len(h) != 0 {
					t.Fatalf("a bridge that ran late was hung up: %+v", h)
				}
				slots.Free() // the conversation ends
			} else {
				timers.fire(t) // the window ends with no bot leg
				h := sw.hangupList()
				if len(h) != 1 || h[0] != (hungUp{leg, "NORMAL_CLEARING"}) {
					t.Fatalf("hangups = %+v, want the answered leg once", h)
				}
				sw.mu.Lock()
				vars := append([][3]string(nil), sw.vars...)
				sw.mu.Unlock()
				if len(vars) != 1 || vars[0] != [3]string{leg, telephony.VarHangupCause, BridgeUnconfirmedCause} {
					t.Fatalf("stamped %+v, want %s before the hangup", vars, BridgeUnconfirmedCause)
				}
				// A bridge the switch runs even later is refused at the bot.
				if !slots.IsWithdrawn(key) {
					t.Fatal("an INVITE after the window would still be let in")
				}
				if n != never {
					s.HandleSwitchEvent(telephony.SwitchEvent{
						Kind: telephony.KindChannelCreate, ChannelID: "late-bot-leg"})
				}
				hangup(s, leg)
			}
			if r, u := slots.Reserved(), slots.InUse(); r != 0 || u != 0 {
				t.Errorf("after the call: %d reserved, %d in use; want none", r, u)
			}
		})
	}
}

// An INVITE that took the slot is the bot's own account that the bridge ran,
// even when the switch's event for the bot leg was not seen: the window
// ending then leaves the call alone.
func TestABridgeWhoseInviteTookTheSlotIsNotEndedAtTheWindow(t *testing.T) {
	t.Parallel()
	_, sw, slots, timers, key, _ := unansweredBridge(t)
	if !slots.Consume(key) {
		t.Fatal("the INVITE found no reservation")
	}
	timers.fire(t)
	if h := sw.hangupList(); len(h) != 0 {
		t.Fatalf("a call whose bot answered was hung up: %+v", h)
	}
	slots.Free()
	if r, u := slots.Reserved(), slots.InUse(); r != 0 || u != 0 {
		t.Errorf("%d reserved, %d in use; want none", r, u)
	}
}

// The customer hanging up while the bridge is unconfirmed settles it: the
// slot goes back with them and nothing is hung up later.
func TestACustomerWhoLeavesSettlesAnUnconfirmedBridge(t *testing.T) {
	t.Parallel()
	s, sw, slots, timers, _, leg := unansweredBridge(t)
	hangup(s, leg)
	timers.fire(t)
	if h := sw.hangupList(); len(h) != 0 {
		t.Fatalf("a gone customer was hung up: %+v", h)
	}
	if r := slots.Reserved(); r != 0 {
		t.Errorf("%d reserved after the customer left", r)
	}
}

// A bridge the switch refused is a failure as before: the slot goes back at
// once and nothing waits.
func TestABridgeTheSwitchRefusedGivesTheSlotBackAtOnce(t *testing.T) {
	t.Parallel()
	s, sw, slots := slotted(t, 1, 0, time.Second)
	sw.bridgeErr = fmt.Errorf("uuid_transfer: -ERR No such channel!")
	_, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitBridges(t, sw, 1)
	waitUntil(t, func() bool { return slots.Reserved() == 0 })
	s.mu.Lock()
	pending := len(s.bridges)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatal("a refused bridge is waited for")
	}
}

// A bridge the adapter refused to build, because a value would be read by the
// inline grammar as syntax, is refused like any other: nothing was sent,
// nothing waits for it, and its bot slot goes back at once.
func TestABridgeTheAdapterRefusedGivesTheSlotBackAtOnce(t *testing.T) {
	t.Parallel()
	s, sw, slots := slotted(t, 1, 0, time.Second)
	sw.bridgeErr = fmt.Errorf("%w: %q holds %q", telephony.ErrUnsafeValue, "en'", "'")
	_, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitBridges(t, sw, 1)
	waitUntil(t, func() bool { return slots.Reserved() == 0 })
	s.mu.Lock()
	pending := len(s.bridges)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatal("a refused bridge is waited for")
	}
	if h := sw.hangupList(); len(h) != 0 {
		t.Errorf("a refused bridge hung the leg up: %+v", h)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
