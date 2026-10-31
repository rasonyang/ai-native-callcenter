// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// The originate owns a leg until it writes its +OK; a transfer that lands
// earlier is overwritten by its &park() (#333). So an originated leg's
// continuation runs only when the answer and the +OK are both in.

func hasPending(s *Service, leg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[leg] != nil
}

// gateService is a service whose originates get distinct job ids, on a clock
// the test moves.
func gateService(t *testing.T) (*Service, *jobSwitch, *recordingLedger, *fakeClock) {
	t.Helper()
	s, sw, ledger := jobService(t)
	return s, sw, ledger, clocked(s)
}

func placeAt(t *testing.T, p placer, s *Service, sw *jobSwitch) string {
	t.Helper()
	return p.place(t, s, sw.fakeSwitch)
}

func jobOf(sw *jobSwitch, leg string) string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	for id, l := range sw.legs {
		if l == leg {
			return id
		}
	}
	return ""
}

func okEvent(jobID, leg string) telephony.SwitchEvent {
	return telephony.SwitchEvent{Kind: telephony.KindBackgroundJob, JobID: jobID, JobChannelID: leg, IsJobOK: true}
}

func errEvent(jobID, leg, cause string) telephony.SwitchEvent {
	return telephony.SwitchEvent{Kind: telephony.KindBackgroundJob, JobID: jobID, JobChannelID: leg, JobCause: cause}
}

func create(s *Service, leg string) {
	s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg})
}

func TestTheContinuationWaitsForTheOriginateAfterTheAnswer(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, _ := gateService(t)
			var answered atomic.Int32
			s.SetAnswerObserver(func(uuid.UUID) { answered.Add(1) })
			leg := placeAt(t, p, s, sw)
			create(s, leg)

			answerOnly(s, leg)
			if p.acted(sw.fakeSwitch) != 0 || !hasPending(s, leg) {
				t.Fatalf("acted %d, armed %v: the leg was sent on while the originate still owned it",
					p.acted(sw.fakeSwitch), hasPending(s, leg))
			}
			s.HandleSwitchEvent(okEvent(jobOf(sw, leg), leg))
			p.wait(t, sw.fakeSwitch)
			if hasPending(s, leg) {
				t.Error("the entry outlived its continuation")
			}
			// A repeated answer or result sends nothing more.
			answerOnly(s, leg)
			s.HandleSwitchEvent(okEvent(jobOf(sw, leg), leg))
			released(s, leg)
			settle()
			if n := p.acted(sw.fakeSwitch); n != 1 {
				t.Errorf("acted %d times, want once", n)
			}
			if p.name == "ai" && answered.Load() != 1 {
				t.Errorf("answer observer ran %d times, want once", answered.Load())
			}
		})
	}
}

func TestTheContinuationRunsOnceWhenTheResultComesFirst(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, _ := gateService(t)
			leg := placeAt(t, p, s, sw)
			create(s, leg)

			s.HandleSwitchEvent(okEvent(jobOf(sw, leg), leg))
			settle()
			if p.acted(sw.fakeSwitch) != 0 {
				t.Fatal("the leg was sent on before anybody answered")
			}
			answerOnly(s, leg)
			p.wait(t, sw.fakeSwitch)
			answerOnly(s, leg)
			released(s, leg)
			settle()
			if n := p.acted(sw.fakeSwitch); n != 1 {
				t.Errorf("acted %d times, want once", n)
			}
		})
	}
}

func TestAnAnswerWithoutTheResultSendsNothingButStillTellsTheObserver(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, _ := gateService(t)
			var answered atomic.Int32
			s.SetAnswerObserver(func(uuid.UUID) { answered.Add(1) })
			leg := placeAt(t, p, s, sw)
			create(s, leg)
			answerOnly(s, leg)
			answerOnly(s, leg)
			settle()
			if p.acted(sw.fakeSwitch) != 0 || s.PendingCount() != 1 {
				t.Errorf("acted %d, pending %d; want nothing sent and the entry kept",
					p.acted(sw.fakeSwitch), s.PendingCount())
			}
			want := int32(0)
			if p.name == "ai" {
				want = 1
			}
			if answered.Load() != want {
				t.Errorf("observer ran %d times, want %d", answered.Load(), want)
			}
		})
	}
}

// A failed originate after the channel existed leaves the CDR as the record:
// no row of its own, and nothing is sent onto the dying leg, whichever of the
// answer and the failure comes first.
func TestAFailedResultDropsTheContinuationWhicheverComesFirst(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		for _, answerFirst := range []bool{true, false} {
			name := p.name + "/failure-first"
			if answerFirst {
				name = p.name + "/answer-first"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s, sw, ledger, _ := gateService(t)
				leg := placeAt(t, p, s, sw)
				create(s, leg)
				fail := func() { s.HandleSwitchEvent(errEvent(jobOf(sw, leg), leg, "NORMAL_TEMPORARY_FAILURE")) }
				if answerFirst {
					answerOnly(s, leg)
					fail()
				} else {
					fail()
					answerOnly(s, leg)
				}
				released(s, leg)
				settle()
				if hasPending(s, leg) {
					t.Error("the entry survived a failed originate")
				}
				if p.acted(sw.fakeSwitch) != 0 {
					t.Error("a leg whose originate failed was sent on")
				}
				if ledger.total() != 0 {
					t.Errorf("%d placement-failure rows for a call whose channel existed", ledger.total())
				}
			})
		}
	}
}

func TestAResultThenALateAnswerHangsUpAndSendsNothing(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, clock := gateService(t)
			leg := placeAt(t, p, s, sw)
			create(s, leg)
			s.HandleSwitchEvent(okEvent(jobOf(sw, leg), leg))
			clock.t = clock.t.Add(s.cfg.AnswerWindow + time.Millisecond)
			answerOnly(s, leg)
			h := waitHangups(t, sw.fakeSwitch, 1)
			if h[0] != (hungUp{leg, LateAnswerCause}) {
				t.Errorf("hangup = %+v, want %s on the leg", h[0], LateAnswerCause)
			}
			settle()
			if p.acted(sw.fakeSwitch) != 0 || hasPending(s, leg) {
				t.Errorf("acted %d, armed %v after a late answer", p.acted(sw.fakeSwitch), hasPending(s, leg))
			}
		})
	}
}

func TestAHangupBetweenTheAnswerAndTheResultSendsNothing(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, _ := gateService(t)
			leg := placeAt(t, p, s, sw)
			create(s, leg)
			answerOnly(s, leg)
			hangup(s, leg)
			if hasPending(s, leg) {
				t.Fatal("the entry survived the hangup")
			}
			s.HandleSwitchEvent(okEvent(jobOf(sw, leg), leg))
			settle()
			if p.acted(sw.fakeSwitch) != 0 {
				t.Error("a hung-up leg was sent on")
			}
		})
	}
}

// An answer inside the window is in time whenever the result follows: the
// window no longer applies to it, and the sweep leaves it for the result.
func TestAnAnswerInsideTheWindowKeepsItsPlaceUntilTheResult(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, clock := gateService(t)
			s.jobMargin = time.Minute
			leg := placeAt(t, p, s, sw)
			create(s, leg)
			clock.t = clock.t.Add(s.cfg.AnswerWindow - time.Millisecond)
			answerOnly(s, leg)
			clock.t = clock.t.Add(10 * time.Millisecond) // the window is over
			if n := s.PendingCount(); n != 1 {
				t.Fatalf("pending = %d: the sweep dropped an answered leg waiting for its result", n)
			}
			s.HandleSwitchEvent(okEvent(jobOf(sw, leg), leg))
			p.wait(t, sw.fakeSwitch)
			if h := sw.hangupList(); len(h) != 0 {
				t.Errorf("hangups = %+v: an in-time answer was treated as late", h)
			}
		})
	}
}

// A result that never comes does not keep the entry for ever.
func TestAnAnsweredLegWhoseResultNeverComesIsSweptAfterTheMargin(t *testing.T) {
	t.Parallel()
	s, sw, _, clock := gateService(t)
	s.jobMargin = time.Minute
	leg := placeAt(t, placers()[1], s, sw)
	create(s, leg)
	answerOnly(s, leg)
	clock.t = clock.t.Add(s.cfg.AnswerWindow + 30*time.Second)
	if n := s.PendingCount(); n != 1 {
		t.Fatalf("pending = %d inside the margin, want 1", n)
	}
	clock.t = clock.t.Add(31 * time.Second)
	if n := s.PendingCount(); n != 0 {
		t.Errorf("pending = %d past the margin, want 0", n)
	}
}

// The result dispatched before the bgapi reply is handled (#302) is kept on
// the job and applied by attachJob, once the answer is armed. The answer
// follows it.
func TestAResultKeptBeforeTheReplyReleasesTheLeg(t *testing.T) {
	t.Parallel()
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			s, sw, _, _ := gateService(t)
			sw.onSend = func(jobID string) {
				sw.mu.Lock()
				leg := sw.legs[jobID]
				sw.mu.Unlock()
				s.HandleSwitchEvent(okEvent("", leg)) // not yet named by a reply
			}
			leg := placeAt(t, p, s, sw)
			if !hasPending(s, leg) {
				t.Fatal("the leg is not armed")
			}
			settle()
			if p.acted(sw.fakeSwitch) != 0 {
				t.Fatal("the leg was sent on before anybody answered")
			}
			create(s, leg)
			answerOnly(s, leg)
			p.wait(t, sw.fakeSwitch)
			settle()
			if n := p.acted(sw.fakeSwitch); n != 1 {
				t.Errorf("acted %d times, want once", n)
			}
		})
	}
}

// The unconfirmed placement (#221): the result reaches the service by each
// of the ways the leg can be held.
func TestAnUnconfirmedPlacementIsReleasedByItsResult(t *testing.T) {
	t.Parallel()
	type unconfirmed struct {
		name  string
		place func(s *Service, id uuid.UUID) error
		acted func(st stalled) int
		wait  func(t *testing.T, st stalled)
	}
	kinds := []unconfirmed{
		{"ai",
			func(s *Service, id uuid.UUID) error {
				_, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: id, To: "18688886660", DIDNumber: "95099"})
				return err
			},
			func(st stalled) int { return st.sw.bridgeCount() },
			func(t *testing.T, st stalled) { waitBridges(t, st.sw.fakeSwitch, 1) }},
		{"click-to-dial",
			func(s *Service, id uuid.UUID) error {
				_, err := s.Dial(context.Background(), AgentDialRequest{CallID: id, AgentExtension: "1008", To: "18688886666"})
				return err
			},
			func(st stalled) int { return st.sw.transferCount() },
			func(t *testing.T, st stalled) { waitTransfers(t, st.sw.fakeSwitch, 1) }},
	}
	for _, k := range kinds {
		// How the result reaches the service.
		for _, how := range []string{"applied-by-hold", "after-hold", "after-window-adopted-the-leg"} {
			t.Run(k.name+"/"+how, func(t *testing.T) {
				t.Parallel()
				st, timers := manualService(t)
				s := st.s
				callID := uuid.Must(uuid.NewV7())
				var leg string
				if how == "applied-by-hold" {
					s.beforeHold = func(legID string) { s.HandleSwitchEvent(okEvent("job-late", legID)) }
				}
				if err := k.place(s, callID); !errors.Is(err, ErrPlacementUnconfirmed) {
					t.Fatalf("placement error = %v", err)
				}
				leg = st.sw.originateList()[0].partyID.String()
				st.create(leg, callID)
				if how == "after-window-adopted-the-leg" {
					timers.fire(t) // the leg exists: adopted, the job forgotten
				}

				answerOnly(s, leg)
				if how != "applied-by-hold" {
					settle()
					if k.acted(st) != 0 {
						t.Fatal("the leg was sent on before the originate's result")
					}
					s.HandleSwitchEvent(okEvent("job-late", leg))
				}
				k.wait(t, st)
				settle()
				if k.acted(st) != 1 {
					t.Errorf("acted %d times, want once", k.acted(st))
				}
				if rows := st.ledger.get(callID); len(rows) != 0 {
					t.Errorf("placement-failure rows %+v for a call that exists", rows)
				}
			})
		}
	}
}

// After a restart a ringing leg is re-armed with no tracked job; the result
// of the originate the previous process sent reaches this one too, naming
// only the leg.
func TestARearmedLegIsReleasedByTheResultOfAnotherProcessesOriginate(t *testing.T) {
	for _, kind := range []string{"ai", "click-to-dial"} {
		t.Run(kind, func(t *testing.T) {
			rig := newRecoveryRig(t)
			call, id := uuid.New(), uuid.New()
			vars := aiVars(call, "95099", "en", "18688886666")
			if kind == "click-to-dial" {
				vars = map[string]string{
					"aicc_call_id": call.String(), telephony.VarPlacedBy: telephony.PlacedByDial,
					"aicc_destination": "13800001111", "aicc_extension": "1007",
				}
			}
			rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second, vars: vars})
			if rep := rig.reconcile(t); rep.Rearmed != 1 {
				t.Fatalf("report = %+v, want the leg re-armed", rep)
			}

			answerOnly(rig.svc, id.String())
			settle()
			if len(rig.sw.bridgeCommands()) != 0 {
				t.Fatal("the leg was sent on before the originate's result")
			}
			rig.svc.HandleSwitchEvent(okEvent("job-of-the-old-process", id.String()))
			waitForTransfer(t, rig.sw, id.String())
			rig.svc.HandleSwitchEvent(okEvent("job-of-the-old-process", id.String()))
			settle()
			if n := len(rig.sw.bridgeCommands()); n != 1 {
				t.Errorf("%d transfers, want 1", n)
			}
		})
	}
}

// The answer was seen and the result lost with the link: the reconnect finds
// the leg answered and parked, which no originate holds any more, so the
// continuation runs from there, without a second answer notice, and a result
// that turns up afterwards finds nothing to do.
func TestALostResultIsPickedUpByRecoveryOfTheParkedLeg(t *testing.T) {
	rig := newRecoveryRig(t)
	call, id := uuid.New(), uuid.New()
	var answered atomic.Int32
	rig.svc.SetAnswerObserver(func(uuid.UUID) { answered.Add(1) })
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second,
		vars: aiVars(call, "95099", "en", "18688886666")})
	if rep := rig.reconcile(t); rep.Rearmed != 1 {
		t.Fatalf("report = %+v, want the leg re-armed", rep)
	}
	answerOnly(rig.svc, id.String())
	if answered.Load() != 1 {
		t.Fatalf("observer ran %d times at the answer, want once", answered.Load())
	}

	// The link drops and the result is lost; on the reconnect the leg is
	// answered and parked.
	rig.sw.order = nil
	rig.sw.add(t0, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "park",
		age: 20 * time.Second, vars: aiVars(call, "95099", "en", "18688886666")})
	rep := rig.reconcile(t)
	if rep.Resumed != 1 || rep.Held != 0 {
		t.Errorf("report = %+v, want the leg resumed", rep)
	}
	waitForTransfer(t, rig.sw, id.String())
	if cause, ok := rig.sw.killed(id.String()); ok {
		t.Errorf("the leg was hung up (%s)", cause)
	}
	if answered.Load() != 1 {
		t.Errorf("observer ran %d times, want once", answered.Load())
	}
	if hasPending(rig.svc, id.String()) {
		t.Error("the entry survived the recovery")
	}

	rig.svc.HandleSwitchEvent(okEvent("job-late", id.String()))
	settle()
	if n := len(rig.sw.bridgeCommands()); n != 1 {
		t.Errorf("%d transfers after the late result, want 1", n)
	}
}

// And the other order: the result arrives first and the recovery finds the
// entry gone, a leg whose continuation fired a moment ago and is parked only
// because its command is on the way.
func TestAParkedLegWhoseContinuationJustFiredIsHeld(t *testing.T) {
	rig := newRecoveryRig(t)
	rig.cold = false
	call, id := uuid.New(), uuid.New()
	rig.sw.add(t0, chanSpec{id: id, callState: "ACTIVE", answered: true, application: "park",
		age: 20 * time.Second, vars: aiVars(call, "95099", "en", "18688886666")})
	ran := make(chan struct{}, 2)
	rig.svc.armUntil(id.String(), t0.Add(40*time.Second), continuation{proceed: func() { ran <- struct{}{} }})
	answer(rig.svc, id.String())
	<-ran

	rep := rig.reconcile(t)
	if rep.Held != 1 || rep.Resumed != 0 {
		t.Errorf("report = %+v, want the leg held", rep)
	}
	if cause, ok := rig.sw.killed(id.String()); ok {
		t.Errorf("a leg being sent on was hung up (%s)", cause)
	}
	select {
	case <-ran:
		t.Error("the continuation ran twice")
	default:
	}
}
