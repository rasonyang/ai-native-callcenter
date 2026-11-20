// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
)

// capsSwitch records what the switch was told, and can refuse it.
type capsSwitch struct {
	mu    sync.Mutex
	told  [][2]time.Duration
	err   error
	calls int
}

func (s *capsSwitch) set(botKillAfter, agentCap time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.told = append(s.told, [2]time.Duration{botKillAfter, agentCap})
	return nil
}

func capsFixture(t *testing.T, sw *capsSwitch, botCap, agentCap time.Duration) (*wiringFixture, *bytes.Buffer) {
	t.Helper()
	f := newWiringFixture(t)
	var logs bytes.Buffer
	f.comp.CallCaps = newSwitchCallCaps(sw.set, botCap, agentCap, slog.New(slog.NewTextHandler(&logs, nil)))
	f.comp.connect()
	return f, &logs
}

// The switch is told the deployment's two maximums on the first connect and
// again on every reconnect (#637): a switch that restarted has forgotten
// them, and until it is told its scripts run on their defaults. The bot side
// is the cap plus the grace.
func TestTheSwitchIsGivenTheMaximumCallDurationsOnEveryConnect(t *testing.T) {
	t.Parallel()
	sw := &capsSwitch{}
	f, logs := capsFixture(t, sw, 20*time.Minute, 2*time.Hour)
	if sw.calls != 0 {
		t.Fatalf("told the switch %d times before it connected", sw.calls)
	}
	f.link.connected(t.Context())
	f.link.lost()
	f.link.connected(t.Context())
	want := [2]time.Duration{21 * time.Minute, 2 * time.Hour}
	if len(sw.told) != 2 || sw.told[0] != want || sw.told[1] != want {
		t.Fatalf("told %v, want %v on each of two connects", sw.told, want)
	}
	if out := logs.String(); !strings.Contains(out, "botKillAfterSec=1260") || !strings.Contains(out, "agentMaxSec=7200") ||
		strings.Contains(out, "level=WARN") {
		t.Errorf("log: %s", out)
	}
}

// The defaults are the settings' own: 300 s plus the 60 s grace, and 3600 s,
// the same numbers the Lua scripts fall back to.
func TestTheDefaultMaximumsAreTheScriptsDefaults(t *testing.T) {
	t.Parallel()
	sw := &capsSwitch{}
	f, _ := capsFixture(t, sw, config.DefaultBotMaxCallDuration, config.DefaultAgentMaxCallDuration)
	f.link.connected(t.Context())
	if want := [2]time.Duration{360 * time.Second, 3600 * time.Second}; len(sw.told) != 1 || sw.told[0] != want {
		t.Errorf("told %v, want %v", sw.told, want)
	}
}

// A switch that refuses is a warning, not a stop: the scripts still end calls
// at their defaults, and the next connect tries again.
func TestARefusedMaximumWarnsAndTheNextConnectTriesAgain(t *testing.T) {
	t.Parallel()
	sw := &capsSwitch{err: errors.New("timeout")}
	f, logs := capsFixture(t, sw, 5*time.Minute, time.Hour)
	f.link.connected(t.Context())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "timeout") {
		t.Errorf("want a WARN with the error: %s", logs)
	}
	sw.mu.Lock()
	sw.err = nil
	sw.mu.Unlock()
	f.link.connected(t.Context())
	if sw.calls != 2 || len(sw.told) != 1 {
		t.Errorf("calls %d told %v, want a second try that lands", sw.calls, sw.told)
	}
}

// Registered ahead of everything else on the link, so the scripts read the
// deployment's numbers before the slower reconnect work runs.
func TestTheMaximumsAreTheFirstThingAConnectDoes(t *testing.T) {
	t.Parallel()
	sw := &capsSwitch{}
	f, _ := capsFixture(t, sw, 5*time.Minute, time.Hour)
	if len(f.link.onConnect) == 0 {
		t.Fatal("no connect hooks")
	}
	f.link.onConnect[0](t.Context())
	if sw.calls != 1 {
		t.Errorf("the first connect hook told the switch %d times, want 1", sw.calls)
	}
	without := newWiringFixture(t)
	without.comp.connect()
	if len(f.link.onConnect) != len(without.link.onConnect)+1 {
		t.Errorf("connect hooks %d with, %d without", len(f.link.onConnect), len(without.link.onConnect))
	}
}
