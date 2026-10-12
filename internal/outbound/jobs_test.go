// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// jobSwitch issues a distinct Job-UUID per originate, as the switch does, and
// can deliver a result before the originate returns.
type jobSwitch struct {
	*fakeSwitch
	mu     sync.Mutex
	n      int
	legs   map[string]string // job id -> leg id
	onSend func(jobID string)
}

func (j *jobSwitch) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	if _, err := j.fakeSwitch.Originate(partyID, endpoint, vars); err != nil {
		return "", err
	}
	j.mu.Lock()
	j.n++
	id := fmt.Sprintf("job-%04d", j.n)
	j.legs[id] = partyID.String()
	hook := j.onSend
	j.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return id, nil
}

type recordingLedger struct {
	mu   sync.Mutex
	cdrs map[uuid.UUID][]store.CDR
}

func (r *recordingLedger) InsertCDR(_ context.Context, cdr store.CDR) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cdrs[cdr.CallID] = append(r.cdrs[cdr.CallID], cdr)
	return nil
}

func (r *recordingLedger) get(id uuid.UUID) []store.CDR {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.CDR(nil), r.cdrs[id]...)
}

func (r *recordingLedger) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.cdrs {
		n += len(c)
	}
	return n
}

func jobEvent(jobID, body string) telephony.SwitchEvent {
	isOK, cause := telephony.ParseJobResult(body)
	return telephony.SwitchEvent{Kind: telephony.KindBackgroundJob, JobID: jobID, IsJobOK: isOK, JobCause: cause}
}

func jobService(t *testing.T) (*Service, *jobSwitch, *recordingLedger) {
	t.Helper()
	sw := &jobSwitch{fakeSwitch: &fakeSwitch{}, legs: map[string]string{}}
	s := testService(t, sw.fakeSwitch, nil)
	s.sw = sw
	s.limiter = NewLimiter(1_000_000)
	s.cfg.AnswerWindow = 2 * time.Second
	s.jobMargin = time.Millisecond
	ledger := &recordingLedger{cdrs: map[uuid.UUID][]store.CDR{}}
	s.SetFailureLedger(ledger)
	return s, sw, ledger
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type jobCase struct {
	name string
	// body of the result; empty is a real event with nothing in it
	body string
	// hasChannel: the switch created the leg (and hung it up) first
	hasChannel bool
	// before: the result is dispatched before Originate returns
	before bool
	// dup: the result is delivered twice
	dup bool
	// wantCause: the cause the one failed CDR must carry; "" for none
	wantCause string
}

// A realistic mix of 300 originates, seeded so a failure reproduces.
func TestThreeHundredJobResultsFinishEachCallOnceOrNotAtAll(t *testing.T) {
	t.Parallel()
	s, sw, ledger := jobService(t)
	rng := rand.New(rand.NewSource(156))

	pre := []struct{ body, cause string }{
		{"-ERR GATEWAY_DOWN\n", "GATEWAY_DOWN"},
		{"-ERR INVALID_GATEWAY", "INVALID_GATEWAY"},
		{"-ERR NORMAL_TEMPORARY_FAILURE\n", "NORMAL_TEMPORARY_FAILURE"},
		{"-ERR NO_ANSWER\n", "NO_ANSWER"},
		{"-ERR USER_BUSY\n", "USER_BUSY"},
		{"-ERR  GATEWAY_DOWN \n\n", "GATEWAY_DOWN"},
		{"-ERR SOMETHING_THE_SWITCH_INVENTED\n", "SOMETHING_THE_SWITCH_INVENTED"},
		{"-ERR some prose with spaces\n", "some prose with spaces"},
		{"-ERR\n", telephony.UnspecifiedJobCause},
	}

	plan := map[string]jobCase{} // by job id
	var planMu sync.Mutex
	sw.onSend = func(jobID string) {
		planMu.Lock()
		c := plan[jobID]
		planMu.Unlock()
		if c.before {
			s.HandleSwitchEvent(jobEvent(jobID, c.body))
		}
	}

	type placed struct {
		callID uuid.UUID
		jobID  string
		c      jobCase
	}
	var calls []placed
	for i := 0; i < 300; i++ {
		var c jobCase
		switch r := rng.Intn(100); {
		case r < 40: // ringing or answered normally
			c = jobCase{name: "ok", body: "+OK " + uuid.NewString() + "\n", hasChannel: true}
		case r < 55: // busy callee: the channel existed, the job says -ERR afterwards
			c = jobCase{name: "busy-with-channel", body: "-ERR USER_BUSY\n", hasChannel: true}
		case r < 62:
			c = jobCase{name: "no-answer-with-channel", body: "-ERR NO_ANSWER\n", hasChannel: true}
		case r < 85: // never made a channel
			p := pre[rng.Intn(len(pre))]
			c = jobCase{name: "pre-channel", body: p.body, wantCause: p.cause}
			c.before = rng.Intn(5) == 0
			c.dup = rng.Intn(6) == 0
		case r < 90:
			c = jobCase{name: "empty-body"}
		case r < 94:
			c = jobCase{name: "unreadable-body", body: "-USAGE nonsense"}
		default: // a result that outlives its hangup
			c = jobCase{name: "late-after-hangup", body: "-ERR NORMAL_CLEARING\n", hasChannel: true}
		}
		jobID := fmt.Sprintf("job-%04d", i+1)
		planMu.Lock()
		plan[jobID] = c
		planMu.Unlock()

		callID := uuid.Must(uuid.NewV7())
		if _, err := s.DialAI(context.Background(), AIDialRequest{
			CallID: callID, To: fmt.Sprintf("1869999%04d", i), DIDNumber: "95012",
		}); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, placed{callID, jobID, c})
	}

	// Deliver the rest in a shuffled order, the way a busy link interleaves
	// them. A channel's events precede its own job's result, and another
	// client's jobs are mixed in throughout.
	order := rng.Perm(len(calls))
	for _, idx := range order {
		p := calls[idx]
		leg := sw.legs[p.jobID]
		if p.c.hasChannel {
			s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg})
			if p.c.name != "ok" {
				s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelHangup, ChannelID: leg})
			}
		}
		if !p.c.before {
			s.HandleSwitchEvent(jobEvent(p.jobID, p.c.body))
		}
		if p.c.dup {
			s.HandleSwitchEvent(jobEvent(p.jobID, p.c.body))
		}
		s.HandleSwitchEvent(jobEvent("foreign-"+uuid.NewString(), "-ERR GATEWAY_DOWN\n"))
		s.HandleSwitchEvent(jobEvent("foreign-"+uuid.NewString(), "+OK "+uuid.NewString()+"\n"))
	}

	wantFailed := 0
	for _, p := range calls {
		if p.c.wantCause != "" {
			wantFailed++
		}
	}
	eventually(t, "every pre-channel failure to be written", func() bool { return ledger.total() >= wantFailed })
	time.Sleep(50 * time.Millisecond) // nothing more may arrive

	if got := ledger.total(); got != wantFailed {
		t.Fatalf("wrote %d cdrs, want exactly %d (one per pre-channel failure)", got, wantFailed)
	}
	for _, p := range calls {
		got := ledger.get(p.callID)
		if p.c.wantCause == "" {
			if len(got) != 0 {
				t.Errorf("%s: call %s was finished by the job result; the cdr alone does that", p.c.name, p.callID)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%s: call %s finished %d times, want once", p.c.name, p.callID, len(got))
			continue
		}
		cdr := got[0]
		if cdr.Status != store.CDRStatusFailed || cdr.HangupCause != p.c.wantCause {
			t.Errorf("%s: cdr status=%q cause=%q, want FAILED/%q", p.c.name, cdr.Status, cdr.HangupCause, p.c.wantCause)
		}
		if !cdr.AnsweredAt.IsZero() || cdr.CallType != "OUTBOUND" || cdr.DID != "95012" {
			t.Errorf("%s: cdr shape wrong: %+v", p.c.name, cdr)
		}
	}

	// Silent and unreadable jobs are cleared by the backstop; nothing leaks.
	eventually(t, "the job map to drain", func() bool { return s.JobCount() == 0 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.legJobs) != 0 {
		t.Errorf("leg index leaked %d entries", len(s.legJobs))
	}
	for _, p := range calls {
		if p.c.wantCause == "" {
			continue
		}
		if _, armed := s.pending[sw.legs[p.jobID]]; armed {
			t.Errorf("failed call %s is still armed", p.callID)
		}
	}
	if len(s.early) > earlyResultMax {
		t.Errorf("early results exceeded their bound: %d", len(s.early))
	}
}

// A click-to-dial whose agent leg is refused leaves a failed row naming the
// agent's phone and the number that was to be dialled.
func TestAClickToDialThatNeverMadeAChannelIsRecorded(t *testing.T) {
	t.Parallel()
	s, sw, ledger := jobService(t)
	callID, err := s.Dial(context.Background(), AgentDialRequest{
		AgentExtension: "1008", To: "13800001111",
		UserData: map[string]string{"ticket": "T-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var jobID string
	for id := range sw.legs {
		jobID = id
	}
	s.HandleSwitchEvent(jobEvent(jobID, "-ERR USER_NOT_REGISTERED\n"))
	eventually(t, "the cdr", func() bool { return len(ledger.get(callID)) == 1 })
	cdr := ledger.get(callID)[0]
	if cdr.Status != store.CDRStatusFailed || cdr.HangupCause != "USER_NOT_REGISTERED" ||
		cdr.FromNumber != "1008" || cdr.ToNumber != "13800001111" || cdr.CallType != "OUTBOUND" {
		t.Errorf("unexpected cdr: %+v", cdr)
	}
	if cdr.UserData["ticket"] != "T-9" {
		t.Errorf("the request's business data was lost: %v", cdr.UserData)
	}
	if s.Data().CallData(callID) != nil {
		t.Error("call data still held for a call that never existed")
	}
	if s.JobCount() != 0 {
		t.Error("job map not empty")
	}
}

// A job that stays silent is logged by the backstop and forgotten; nothing is
// written on its behalf.
func TestASilentJobIsForgottenWithoutAFinish(t *testing.T) {
	t.Parallel()
	s, _, ledger := jobService(t)
	s.cfg.AnswerWindow = 10 * time.Millisecond
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95012"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the backstop", func() bool { return s.JobCount() == 0 })
	if ledger.total() != 0 {
		t.Error("the backstop must not invent a finish")
	}
}
