// SPDX-License-Identifier: Apache-2.0

package httpapi

// Redialling a callback (#478). The attempt is written on the callback once a
// call is placed, or once its placement is unconfirmed (202), because the
// phone may still ring. A redial refused before anything is sent leaves the
// callback's last attempt exactly as it was and announces nothing, because no
// attempt happened. Run against a real ledger and the real outbound service
// in front of a counting switch, as createcall_callid_test.go is: "nothing
// was dialled and nothing was written" is the claim under test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// listedNumber is on the do-not-call list of every redial stack. The number
// the callbacks were left from (claimedCallback's) is not.
const listedNumber = "13700137099"

// redialStack is the callId stack with an event stream: what a redial
// announces is part of what it does.
type redialStack struct {
	*dncStack
	sub *events.Subscription
}

func newRedialStack(t *testing.T) *redialStack {
	t.Helper()
	s := newCallIDStack(t)
	hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))
	s.server.hub = hub
	sub, _, _ := hub.Subscribe(events.Subscriber{UserID: v7(t), SeesEveryCall: true}, 0)
	t.Cleanup(sub.Close)
	if _, _, err := s.st.DNC().Add(context.Background(), "+86"+listedNumber, "MANUAL", nil, nil); err != nil {
		t.Fatalf("list %s: %v", listedNumber, err)
	}
	return &redialStack{dncStack: s, sub: sub}
}

// redial places a click-to-dial keeping the callback. It reports a bad answer
// instead of failing the test, so it can run on any goroutine.
func (s *redialStack) redial(ac AuthContext, callbackID uuid.UUID, to, callID string) (createCallAnswer, error) {
	body := fmt.Sprintf(`{"to":%q,"callbackId":%q}`, to, callbackID)
	if callID != "" {
		body = fmt.Sprintf(`{"to":%q,"callbackId":%q,"callId":%q}`, to, callbackID, callID)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.server.CreateCall(w, r)
	a := createCallAnswer{status: w.Code, body: w.Body.Bytes()}
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		return a, fmt.Errorf("answer is not JSON: %v: %s", err, w.Body)
	}
	return a, nil
}

func (s *redialStack) mustRedial(ac AuthContext, callbackID uuid.UUID, to, callID string) createCallAnswer {
	s.t.Helper()
	a, err := s.redial(ac, callbackID, to, callID)
	if err != nil {
		s.t.Fatal(err)
	}
	return a
}

// announced drains what the hub has published so far and returns the
// callbacks CALLBACK_UPDATED carried. The handler publishes before it answers,
// so after a request has returned its events are already queued.
func (s *redialStack) announced() []api.Callback {
	var out []api.Callback
	for {
		select {
		case ev := <-s.sub.C:
			if ev.Type != events.TypeCallbackUpdated {
				continue
			}
			cb, _ := ev.Payload["callback"].(api.Callback)
			out = append(out, cb)
		default:
			return out
		}
	}
}

// callbackRow reads one callback as the list serves it.
func callbackRow(t *testing.T, st *store.Store, id uuid.UUID) store.Callback {
	t.Helper()
	cbs, _, err := st.Ledger().ListCallbacks(context.Background(), "", nil, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cbs {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("callback %s not listed", id)
	return store.Callback{}
}

// attemptedCallback is a claimed callback whose previous attempt, ten minutes
// ago, went unanswered: marked when it was placed and settled by its CDR, the
// way every attempt is.
func attemptedCallback(t *testing.T, st *store.Store) (store.Callback, AuthContext) {
	t.Helper()
	ctx := context.Background()
	cb, ac := claimedCallback(t, st)
	previous := v7(t)
	if _, err := st.Ledger().MarkCallbackAttempt(ctx, cb.ID, previous, ac.ActorUserID); err != nil {
		t.Fatalf("mark the previous attempt: %v", err)
	}
	ended := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	if err := st.Ledger().InsertCDR(ctx, store.CDR{
		CallID: previous, StartedAt: ended.Add(-45 * time.Second), EndedAt: ended,
		CallType: "OUTBOUND", Language: "zh", FromNumber: "1008", ToNumber: "13700137000",
		Status: store.CDRStatusNoAnswer, HangupCause: "NO_ANSWER",
	}); err != nil {
		t.Fatalf("the previous attempt's CDR: %v", err)
	}
	cb = callbackRow(t, st, cb.ID)
	if cb.LastAttemptCallID == nil || *cb.LastAttemptCallID != previous ||
		cb.LastAttemptAt == nil || !cb.LastAttemptAt.Equal(ended) || cb.LastAttemptStatus != store.CDRStatusNoAnswer {
		t.Fatalf("fixture: the previous attempt did not settle: %+v", cb)
	}
	return cb, ac
}

// assertSameAttempt fails unless the callback's last attempt is still want's,
// field for field.
func assertSameAttempt(t *testing.T, got, want store.Callback) {
	t.Helper()
	if !sameID(got.LastAttemptCallID, want.LastAttemptCallID) {
		t.Errorf("lastAttemptCallId = %v, want %v", idOf(got.LastAttemptCallID), idOf(want.LastAttemptCallID))
	}
	if !sameTime(got.LastAttemptAt, want.LastAttemptAt) {
		t.Errorf("lastAttemptAt = %v, want %v", got.LastAttemptAt, want.LastAttemptAt)
	}
	if got.LastAttemptStatus != want.LastAttemptStatus {
		t.Errorf("lastAttemptStatus = %q, want %q", got.LastAttemptStatus, want.LastAttemptStatus)
	}
}

// isAbout reports whether the database stamped at between from and to. The
// database's clock is not this process's (a container's may drift from the
// host's), so the window is widened by a minute each way: enough to tell a
// fresh mark from the attempt ten minutes earlier.
func isAbout(at *time.Time, from, to time.Time) bool {
	return at != nil && at.After(from.Add(-time.Minute)) && at.Before(to.Add(time.Minute))
}

func sameID(a, b *uuid.UUID) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func sameTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

func idOf(id *uuid.UUID) string {
	if id == nil {
		return "<none>"
	}
	return id.String()
}

// echoingOutbound stands in for the dialer: every click-to-dial comes back
// with the callId it was sent and one error (nil: placed). onDial, when set,
// runs while the dial is in progress.
type echoingOutbound struct {
	err    error
	onDial func(outbound.AgentDialRequest)
}

func (o echoingOutbound) Dial(_ context.Context, req outbound.AgentDialRequest) (uuid.UUID, error) {
	if o.onDial != nil {
		o.onDial(req)
	}
	return req.CallID, o.err
}

func (echoingOutbound) DialAI(context.Context, outbound.AIDialRequest) (uuid.UUID, error) {
	return uuid.Nil, errors.New("not an AI call")
}

// A redial the dialer refuses before anything is sent answers its error and
// leaves the callback's last attempt as it was: the previous attempt's call,
// time and outcome, or nothing on a callback never attempted. Nothing is
// announced, because nothing about the callback changed.
func TestARedialRefusedBeforeItRingsLeavesTheLastAttempt(t *testing.T) {
	s := newRedialStack(t)
	dialer := s.server.outbound
	for _, c := range []struct {
		name     string
		to       string
		svc      OutboundService // nil: the real dialer in front of the counting switch
		wantHTTP int
		wantCode ErrorCode
	}{
		{"a number on the do-not-call list", listedNumber, nil, http.StatusUnprocessableEntity, CodeNumberOnDNC},
		{"a number that cannot be dialled", "137-0013-7000", nil, http.StatusBadRequest, CodeValidationFailed},
		{"the switch still being reconciled", "13700137000", erringOutbound{err: outbound.ErrNotReady},
			http.StatusServiceUnavailable, CodeSwitchDown},
		{"the link to the switch down", "13700137000",
			erringOutbound{err: fmt.Errorf("bgapi originate: %w", esl.ErrDown)},
			http.StatusServiceUnavailable, CodeSwitchDown},
	} {
		for _, f := range []struct {
			name    string
			fixture func(*testing.T, *store.Store) (store.Callback, AuthContext)
		}{
			{"attempted ten minutes ago", attemptedCallback},
			{"never attempted", claimedCallback},
		} {
			t.Run(c.name+"/"+f.name, func(t *testing.T) {
				s.server.outbound = dialer
				if c.svc != nil {
					s.server.outbound = c.svc
				}
				t.Cleanup(func() { s.server.outbound = dialer })
				before, ac := f.fixture(t, s.st)
				s.announced()
				originates := s.sw.count()

				a := s.mustRedial(ac, before.ID, c.to, "")
				if a.status != c.wantHTTP || a.Error.Code != string(c.wantCode) {
					t.Fatalf("%d %s, want %d %s", a.status, a.body, c.wantHTTP, c.wantCode)
				}
				after := callbackRow(t, s.st, before.ID)
				assertSameAttempt(t, after, before)
				if after.Status != "CLAIMED" {
					t.Errorf("status = %s, want CLAIMED", after.Status)
				}
				if got := s.announced(); len(got) != 0 {
					t.Errorf("announced %d CALLBACK_UPDATED for a redial that placed nothing: %+v", len(got), got)
				}
				if n := s.sw.count() - originates; n != 0 {
					t.Errorf("originates = %d, want 0", n)
				}
			})
		}
	}
}

// A placement the switch did not confirm may still ring the customer, so the
// callback keeps its note of it and says so (202); the placement-failure CDR
// that settles such a call settles the callback too.
func TestAnUnconfirmedRedialKeepsItsMark(t *testing.T) {
	s := newRedialStack(t)
	before, ac := attemptedCallback(t, s.st)
	s.server.outbound = echoingOutbound{err: fmt.Errorf("%w: %w", outbound.ErrPlacementUnconfirmed,
		fmt.Errorf("%w: bgapi originate", esl.ErrTimeout))}
	s.announced()

	from := time.Now()
	a := s.mustRedial(ac, before.ID, "13700137000", "")
	to := time.Now()
	if a.status != http.StatusAccepted || a.CallID.Version() != 7 {
		t.Fatalf("%d %s, want 202 with a minted callId", a.status, a.body)
	}
	after := callbackRow(t, s.st, before.ID)
	if !sameID(after.LastAttemptCallID, &a.CallID) || after.LastAttemptStatus != "" ||
		!isAbout(after.LastAttemptAt, from, to) {
		t.Fatalf("after an unconfirmed redial: %s at %v (%q), want %s between %v and %v with no outcome",
			idOf(after.LastAttemptCallID), after.LastAttemptAt, after.LastAttemptStatus, a.CallID, from, to)
	}
	got := s.announced()
	if len(got) != 1 || !sameID(got[0].LastAttemptCallID, &a.CallID) {
		t.Fatalf("announced %+v, want one CALLBACK_UPDATED naming %s", got, a.CallID)
	}

	ended := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.st.Ledger().InsertCDR(context.Background(), store.CDR{
		CallID: a.CallID, StartedAt: ended.Add(-time.Minute), EndedAt: ended,
		CallType: "OUTBOUND", FromNumber: "1008", ToNumber: "13700137000",
		Status: store.CDRStatusFailed, HangupCause: outbound.UnconfirmedCause,
		Tech: map[string]any{"isPlacementFailure": true},
	}); err != nil {
		t.Fatal(err)
	}
	settled := callbackRow(t, s.st, before.ID)
	if settled.LastAttemptStatus != store.CDRStatusFailed || !sameTime(settled.LastAttemptAt, &ended) {
		t.Errorf("after its placement-failure CDR: %q at %v, want FAILED at %v",
			settled.LastAttemptStatus, settled.LastAttemptAt, ended)
	}
}

// A redial that places its call notes it on the callback and announces it;
// the call's CDR then settles the attempt.
func TestARedialThatIsPlacedIsMarkedAndItsCDRSettlesIt(t *testing.T) {
	s := newRedialStack(t)
	before, ac := attemptedCallback(t, s.st)
	s.announced()

	from := time.Now()
	a := s.mustRedial(ac, before.ID, "13700137000", "")
	to := time.Now()
	if a.status != http.StatusCreated {
		t.Fatalf("%d %s, want 201", a.status, a.body)
	}
	if n := s.sw.count(); n != 1 {
		t.Errorf("originates = %d, want 1", n)
	}
	after := callbackRow(t, s.st, before.ID)
	if !sameID(after.LastAttemptCallID, &a.CallID) || after.LastAttemptStatus != "" ||
		!isAbout(after.LastAttemptAt, from, to) {
		t.Fatalf("after a placed redial: %s at %v (%q), want %s between %v and %v with no outcome",
			idOf(after.LastAttemptCallID), after.LastAttemptAt, after.LastAttemptStatus, a.CallID, from, to)
	}
	got := s.announced()
	if len(got) != 1 || !sameID(got[0].LastAttemptCallID, &a.CallID) || got[0].LastAttemptStatus != nil {
		t.Fatalf("announced %+v, want one CALLBACK_UPDATED naming %s with no outcome", got, a.CallID)
	}

	ended := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.st.Ledger().InsertCDR(context.Background(), store.CDR{
		CallID: a.CallID, StartedAt: ended.Add(-2 * time.Minute), AnsweredAt: ended.Add(-90 * time.Second),
		EndedAt: ended, CallType: "OUTBOUND", FromNumber: "1008", ToNumber: "13700137000",
		Status: store.CDRStatusAnswered, HangupCause: "NORMAL_CLEARING",
	}); err != nil {
		t.Fatal(err)
	}
	settled := callbackRow(t, s.st, before.ID)
	if !sameID(settled.LastAttemptCallID, &a.CallID) || settled.LastAttemptStatus != store.CDRStatusAnswered ||
		!sameTime(settled.LastAttemptAt, &ended) {
		t.Errorf("after its CDR: %s %q at %v, want %s ANSWERED at %v",
			idOf(settled.LastAttemptCallID), settled.LastAttemptStatus, settled.LastAttemptAt, a.CallID, ended)
	}
	if settled.Status != "CLAIMED" {
		t.Errorf("status = %s, want CLAIMED: ringing somebody is not keeping the promise", settled.Status)
	}
}

// Nobody's promise gets a call recorded against it but its holder's: a
// callback somebody else holds, or nobody does, is refused with 409 before
// anything is dialled or written.
func TestARedialOfACallbackTheCallerDoesNotHoldIsRefused(t *testing.T) {
	s := newRedialStack(t)
	held, _ := attemptedCallback(t, s.st)
	_, stranger := claimedCallback(t, s.st) // another agent, with a claim of their own

	released, holder := attemptedCallback(t, s.st)
	if _, err := s.st.Ledger().ReleaseCallback(context.Background(), released.ID, holder.ActorUserID); err != nil {
		t.Fatal(err)
	}
	released = callbackRow(t, s.st, released.ID)

	for _, c := range []struct {
		name string
		cb   store.Callback
		ac   AuthContext
	}{
		{"somebody else holds it", held, stranger},
		{"nobody holds it", released, holder},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.announced()
			a := s.mustRedial(c.ac, c.cb.ID, "13700137000", "")
			if a.status != http.StatusConflict || a.Error.Code != string(CodeConflict) {
				t.Fatalf("%d %s, want 409 CONFLICT", a.status, a.body)
			}
			assertSameAttempt(t, callbackRow(t, s.st, c.cb.ID), c.cb)
			if got := s.announced(); len(got) != 0 {
				t.Errorf("announced %+v", got)
			}
			if n := s.sw.count(); n != 0 {
				t.Errorf("originates = %d, want 0", n)
			}
		})
	}
}

// The claim is checked before the dial and the mark is written after it, so
// the claim can be lost in between. The call is placed by then: it is
// answered as placed, and the callback, no longer the caller's, is left
// alone.
func TestAClaimLostWhileDiallingLeavesTheCallbackAndKeepsTheCall(t *testing.T) {
	s := newRedialStack(t)
	before, ac := attemptedCallback(t, s.st)
	var releaseErr error
	s.server.outbound = echoingOutbound{onDial: func(outbound.AgentDialRequest) {
		_, releaseErr = s.st.Ledger().ReleaseCallback(context.Background(), before.ID, ac.ActorUserID)
	}}
	s.announced()

	a := s.mustRedial(ac, before.ID, "13700137000", "")
	if releaseErr != nil {
		t.Fatalf("release during the dial: %v", releaseErr)
	}
	if a.status != http.StatusCreated || a.CallID.Version() != 7 {
		t.Fatalf("%d %s, want 201: the call was placed", a.status, a.body)
	}
	after := callbackRow(t, s.st, before.ID)
	assertSameAttempt(t, after, before)
	if after.Status != "OPEN" {
		t.Errorf("status = %s, want OPEN (released)", after.Status)
	}
	if got := s.announced(); len(got) != 0 {
		t.Errorf("announced %+v", got)
	}
}

// A retry with the callId of a call already placed (200 isDuplicate) places
// nothing, so it writes nothing: the attempt it names keeps the outcome its
// CDR gave it, and a newer attempt made since is not replaced by it.
func TestARetryOfAPlacedRedialLeavesTheLastAttempt(t *testing.T) {
	s := newRedialStack(t)
	cb, ac := attemptedCallback(t, s.st)
	first := v7(t)
	if a := s.mustRedial(ac, cb.ID, "13700137000", first.String()); a.status != http.StatusCreated {
		t.Fatalf("first: %d %s", a.status, a.body)
	}
	ended := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.st.Ledger().InsertCDR(context.Background(), store.CDR{
		CallID: first, StartedAt: ended.Add(-time.Minute), EndedAt: ended,
		CallType: "OUTBOUND", FromNumber: "1008", ToNumber: "13700137000",
		Status: store.CDRStatusBusy, HangupCause: "USER_BUSY",
	}); err != nil {
		t.Fatal(err)
	}
	settled := callbackRow(t, s.st, cb.ID)
	if settled.LastAttemptStatus != store.CDRStatusBusy {
		t.Fatalf("fixture: %+v", settled)
	}

	t.Run("the attempt it names", func(t *testing.T) {
		s.announced()
		a := s.mustRedial(ac, cb.ID, "13700137000", first.String())
		if a.status != http.StatusOK || !a.IsDuplicate || a.CallID != first {
			t.Fatalf("%d %s, want 200 isDuplicate for %s", a.status, a.body, first)
		}
		assertSameAttempt(t, callbackRow(t, s.st, cb.ID), settled)
		if got := s.announced(); len(got) != 0 {
			t.Errorf("announced %+v", got)
		}
	})

	t.Run("a newer attempt", func(t *testing.T) {
		newer := s.mustRedial(ac, cb.ID, "13700137000", "")
		if newer.status != http.StatusCreated {
			t.Fatalf("newer: %d %s", newer.status, newer.body)
		}
		latest := callbackRow(t, s.st, cb.ID)
		s.announced()
		a := s.mustRedial(ac, cb.ID, "13700137000", first.String())
		if a.status != http.StatusOK || !a.IsDuplicate {
			t.Fatalf("%d %s, want 200 isDuplicate", a.status, a.body)
		}
		assertSameAttempt(t, callbackRow(t, s.st, cb.ID), latest)
		if got := s.announced(); len(got) != 0 {
			t.Errorf("announced %+v", got)
		}
	})
	if n := s.sw.count(); n != 2 {
		t.Errorf("originates = %d, want 2", n)
	}
}

// Two redials of one callback at once, one to a listed number and one placed:
// whichever order they run in, the mark left is the placed call's, and it is
// the only thing announced.
func TestOfTwoConcurrentRedialsThePlacedOneKeepsTheMark(t *testing.T) {
	s := newRedialStack(t)
	for round := range 5 {
		cb, ac := attemptedCallback(t, s.st)
		s.announced()

		var refused, placed createCallAnswer
		var refusedErr, placedErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			refused, refusedErr = s.redial(ac, cb.ID, listedNumber, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			placed, placedErr = s.redial(ac, cb.ID, "13700137000", "")
		}()
		close(start)
		wg.Wait()
		if refusedErr != nil || placedErr != nil {
			t.Fatalf("round %d: %v / %v", round, refusedErr, placedErr)
		}
		if refused.status != http.StatusUnprocessableEntity || refused.Error.Code != string(CodeNumberOnDNC) {
			t.Fatalf("round %d: the listed redial answered %d %s", round, refused.status, refused.body)
		}
		if placed.status != http.StatusCreated {
			t.Fatalf("round %d: the placed redial answered %d %s", round, placed.status, placed.body)
		}

		after := callbackRow(t, s.st, cb.ID)
		if !sameID(after.LastAttemptCallID, &placed.CallID) || after.LastAttemptStatus != "" {
			t.Fatalf("round %d: the mark is %s (%q), want the placed call %s with no outcome",
				round, idOf(after.LastAttemptCallID), after.LastAttemptStatus, placed.CallID)
		}
		got := s.announced()
		if len(got) != 1 || !sameID(got[0].LastAttemptCallID, &placed.CallID) {
			t.Fatalf("round %d: announced %+v, want one CALLBACK_UPDATED naming %s", round, got, placed.CallID)
		}
	}
	if n := s.sw.count(); n != 5 {
		t.Errorf("originates = %d, want 5", n)
	}
}
