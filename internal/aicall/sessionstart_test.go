// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
)

// startLog is the order things happened in at a call's start: the meter's
// count (COUNTED for an inbound call's, PACED for a campaign call's) and the
// session being made (SESSION).
type startLog struct {
	mu        sync.Mutex
	events    []string
	maxWaits  []time.Duration
	isBlocked bool
	waiting   chan struct{}
}

func (l *startLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *startLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func (l *startLog) SessionStarted() { l.add("COUNTED") }

// AwaitStart makes the start after 1.5 s of waiting, or, blocked, waits for
// the call to end.
func (l *startLog) AwaitStart(ctx context.Context, maxWait time.Duration) (ratemeter.Start, error) {
	l.mu.Lock()
	l.events = append(l.events, "PACED")
	l.maxWaits = append(l.maxWaits, maxWait)
	isBlocked := l.isBlocked
	l.mu.Unlock()
	if isBlocked {
		close(l.waiting)
		<-ctx.Done()
		return ratemeter.Start{Waited: 700 * time.Millisecond}, ctx.Err()
	}
	return ratemeter.Start{Waited: 1500 * time.Millisecond}, nil
}

// A call's start is counted on the meter before its session is made, once:
// an inbound or internal call's at once, a campaign call's through the start
// gate, which waits at most ratemeter.StartWaitBound (#644). The session here
// cannot be made ("no api key"), which ends each call right after the start.
func TestOnlyACampaignCallsStartWaitsForTheSessionWindow(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	placed := "X-Aicc-Placed-To-Customer: true\r\nX-Aicc-Call-Type: OUTBOUND\r\n"
	for _, tc := range []struct {
		name, headers string
		want          []string
		wantWaitLine  bool
	}{
		{name: "inbound", want: []string{"COUNTED", "SESSION"}},
		{name: "internal", headers: "X-Aicc-Call-Type: INTERNAL\r\n", want: []string{"COUNTED", "SESSION"}},
		{name: "placed to a customer, not by a campaign", headers: placed, want: []string{"COUNTED", "SESSION"}},
		{name: "a campaign call", headers: placed + "X-Aicc-Campaign-Id: c-1\r\nX-Aicc-Attempt-Id: a-1\r\n",
			want: []string{"PACED", "SESSION"}, wantWaitLine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts := &startLog{}
			rig := newStartRig(t, startExit{sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
				starts.add("SESSION")
				return nil, errors.New("no api key")
			}}, spec)
			rig.o.cfg.SessionStarts = starts
			if err := rig.o.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(rig.o.Stop)

			callID := uuid.Must(uuid.NewV7())
			sendInviteWith(t, rig.o.uas.LocalPort(), "sip-"+callID.String(),
				"X-Aicc-Did: "+didWithQueue+"\r\nX-Aicc-Channel-Id: "+startFailCallerChannel+"\r\n"+
					"X-AICC-Call-ID: "+callID.String()+"\r\n"+tc.headers)
			awaitLogLine(t, rig.logs, "ai call could not run")

			if got := starts.got(); !slices.Equal(got, tc.want) {
				t.Errorf("start = %v, want %v", got, tc.want)
			}
			if tc.wantWaitLine {
				starts.mu.Lock()
				bounds := slices.Clone(starts.maxWaits)
				starts.mu.Unlock()
				if !slices.Equal(bounds, []time.Duration{ratemeter.StartWaitBound}) {
					t.Errorf("waited with bounds %v, want %s", bounds, ratemeter.StartWaitBound)
				}
				awaitLogLine(t, rig.logs, "provider session start waited for the session window")
				for _, l := range parseLogLines(t, rig.logs) {
					if l.msg() == "provider session start waited for the session window" {
						if ms, _ := l.attrs["waitedMs"].(float64); ms != 1500 {
							t.Errorf("waitedMs = %v, want 1500: %s", l.attrs["waitedMs"], l.raw)
						}
					}
				}
			}
		})
	}
}

// A campaign call that ends while its start waits leaves the gate: no session
// is made, and nothing is rescued, because there is nobody left to rescue.
func TestACallThatEndsWhileItsStartWaitsMakesNoSession(t *testing.T) {
	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatal(err)
	}
	starts := &startLog{isBlocked: true, waiting: make(chan struct{})}
	rig := newStartRig(t, startExit{sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
		starts.add("SESSION")
		return newFakeModel(), nil
	}}, spec)
	rig.o.cfg.SessionStarts = starts
	if err := rig.o.Start(); err != nil {
		t.Fatal(err)
	}

	callID := uuid.Must(uuid.NewV7())
	sendInviteWith(t, rig.o.uas.LocalPort(), "sip-"+callID.String(),
		"X-Aicc-Did: "+didWithQueue+"\r\nX-Aicc-Channel-Id: "+startFailCallerChannel+"\r\n"+
			"X-AICC-Call-ID: "+callID.String()+"\r\n"+
			"X-Aicc-Placed-To-Customer: true\r\nX-Aicc-Call-Type: OUTBOUND\r\n"+
			"X-Aicc-Campaign-Id: c-1\r\nX-Aicc-Attempt-Id: a-1\r\n")
	select {
	case <-starts.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the campaign call's start never reached the gate")
	}
	// The call ends: its context is cancelled, as the hangup's would be.
	rig.o.Stop()
	awaitLogLine(t, rig.logs, "the call ended while its session start waited for the provider's session window")
	awaitLogLine(t, rig.logs, "the conversation never started; the human path writes this call's row")

	if got := starts.got(); !slices.Equal(got, []string{"PACED"}) {
		t.Errorf("start = %v, want the wait alone, no session", got)
	}
	for _, l := range parseLogLines(t, rig.logs) {
		if l.msg() == "ai call could not run" {
			t.Errorf("a call that ended while it waited was taken for a failure: %s", l.raw)
		}
	}
	if got := rig.nonVariableOps(); slices.ContainsFunc(got, func(op string) bool { return strings.HasPrefix(op, "stamp") }) {
		t.Errorf("a call that ended while it waited was rescued: %v", got)
	}
}
