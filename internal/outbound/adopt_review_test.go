// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// deadBot adds a customer leg bridged to an old bot leg. The returned ids are
// the customer's and the bot's.
func deadBot(rig *recoveryRig, mutate func(cust, bot *chanSpec)) (string, string) {
	call, cust, bot := uuid.New(), uuid.New(), uuid.New()
	c := chanSpec{id: cust, callState: "ACTIVE", answered: true, application: "bridge",
		age: 90 * time.Second, peer: bot.String(), vars: aiVars(call, "95099", "en", "18688886666")}
	b := chanSpec{id: bot, callState: "ACTIVE", answered: true, application: "bridge",
		age: 85 * time.Second, peer: cust.String(),
		vars: map[string]string{"aicc_call_id": call.String(), "aicc_did": "95099", "aicc_dest": "95099"}}
	if mutate != nil {
		mutate(&c, &b)
	}
	rig.sw.add(t0, c)
	rig.sw.add(t0, b)
	return cust.String(), bot.String()
}

// On a reconnect the in-process sessions survived, and the table read here is
// not atomic with a session opening: no bot call is ended for want of one.
func TestAReconnectNeverReapsABotCall(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.cold = false
	cust, bot := deadBot(rig, nil)

	rep := rig.reconcile(t)

	for _, id := range []string{cust, bot} {
		if cause, ok := rig.sw.killed(id); ok {
			t.Errorf("%s was hung up (%s) on a reconnect", id, cause)
		}
	}
	if rep.OrphanedBot != 0 {
		t.Errorf("report = %+v", rep)
	}
}

// The grace is measured from the bridge, not from when the bot leg was created:
// a bot leg that rang for ten seconds and was bridged a moment ago is young.
func TestTheGraceRunsFromTheBridgeNotFromTheBotLegsCreation(t *testing.T) {
	rig := newRecoveryRig(t)
	cust, bot := deadBot(rig, func(c, b *chanSpec) {
		b.age = 12 * time.Second // rang for ten seconds
		c.bridgedAgo, b.bridgedAgo = 2*time.Second, 2*time.Second
	})

	rep := rig.reconcile(t)

	for _, id := range []string{cust, bot} {
		if cause, ok := rig.sw.killed(id); ok {
			t.Errorf("%s was hung up (%s) two seconds after its bridge", id, cause)
		}
	}
	if rep.Held != 1 {
		t.Errorf("report = %+v, want the call held", rep)
	}
}

// Never reap on missing data.
func TestABridgeWhoseAgeCannotBeToldIsLeftAloneWithAWarning(t *testing.T) {
	rig := newRecoveryRig(t)
	warns := &warnings{}
	prev := slog.Default()
	slog.SetDefault(slog.New(warns))
	defer slog.SetDefault(prev)
	rig.svc.log = slog.New(warns)
	cust, bot := deadBot(rig, func(c, b *chanSpec) { c.noBridgeTimes, b.noBridgeTimes = true, true })

	rig.reconcile(t)

	for _, id := range []string{cust, bot} {
		if cause, ok := rig.sw.killed(id); ok {
			t.Errorf("%s was hung up (%s) on missing data", id, cause)
		}
	}
	if warns.matching("how long the bot bridge has existed") != 1 {
		t.Errorf("warnings = %v", warns.msgs)
	}
}

// The session may open while the rest of the table is being read; it is asked
// about again immediately before the hangup.
func TestASessionThatOpensAtTheLastMomentSavesTheCall(t *testing.T) {
	rig := newRecoveryRig(t)
	var asks atomic.Int32
	rig.hasBotFn = func(uuid.UUID) bool { return asks.Add(1) > 1 }
	cust, bot := deadBot(rig, nil)

	rig.reconcile(t)

	if asks.Load() < 2 {
		t.Fatalf("the session was asked about %d times, want a second look before the hangup", asks.Load())
	}
	for _, id := range []string{cust, bot} {
		if cause, ok := rig.sw.killed(id); ok {
			t.Errorf("%s was hung up (%s) though its session had opened", id, cause)
		}
	}
}

// A pointer to a peer that is still listed but no longer points back is a
// bridge that ended, not a bridge.
func TestAStalePeerPointerIsNotABridge(t *testing.T) {
	rig := newRecoveryRig(t)
	cust, bot := deadBot(rig, func(c, b *chanSpec) { b.peer = "" })

	rep := rig.reconcile(t)

	for _, id := range []string{cust, bot} {
		if cause, ok := rig.sw.killed(id); ok {
			t.Errorf("%s was hung up (%s) on a pointer the peer does not return", id, cause)
		}
	}
	if rep.OrphanedBot != 0 {
		t.Errorf("report = %+v", rep)
	}
}

// Elapsed time is the switch's. This machine's clock an hour ahead changes
// nothing about how much of the window is left.
func TestElapsedTimeComesFromTheSwitchClockNotOurs(t *testing.T) {
	rig := newRecoveryRig(t)
	id := uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 48 * time.Second,
		vars: aiVars(uuid.New(), "95099", "en", "18688886666")})
	rig.clock.advance(time.Hour)

	rep := rig.reconcile(t)

	if rep.Rearmed != 1 {
		t.Fatalf("report = %+v, want the leg re-armed", rep)
	}
	if left := rig.pendingLeg(id.String()).expiresAt.Sub(rig.clock.now()); left != 12*time.Second {
		t.Errorf("12 s of the window should be left, got %v", left)
	}
}

func TestARingingLegWhoseAgeTheSwitchDidNotGiveIsHungUpNotGuessed(t *testing.T) {
	rig := newRecoveryRig(t)
	id := uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second, noSwitchClock: true,
		vars: aiVars(uuid.New(), "95099", "en", "18688886666")})

	rig.reconcile(t)

	if cause, ok := rig.sw.killed(id.String()); !ok || cause != ReconcileCause {
		t.Errorf("killed=%v cause=%q", ok, cause)
	}
	if rig.pendingLeg(id.String()) != nil {
		t.Error("a leg with an unmeasurable window was armed")
	}
}
