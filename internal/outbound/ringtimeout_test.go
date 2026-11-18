// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// fakeClock is the service's clock: tests move it instead of waiting.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func clocked(s *Service) *fakeClock {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	s.now = c.now
	return c
}

func hangup(s *Service, channelID string) {
	s.HandleSwitchEvent(telephony.SwitchEvent{
		Kind: telephony.KindChannelHangup, ChannelID: channelID,
	})
}

func waitHangups(t *testing.T, sw *fakeSwitch, want int) []hungUp {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h := sw.hangupList(); len(h) >= want {
			return h
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hangup count never reached %d", want)
	return nil
}

// settle gives a wrongly started goroutine time to show itself.
func settle() { time.Sleep(30 * time.Millisecond) }

type placer struct {
	name  string
	place func(t *testing.T, s *Service, sw *fakeSwitch) string // returns the leg id
	// acted counts the on-time continuations that ran.
	acted func(sw *fakeSwitch) int
	wait  func(t *testing.T, sw *fakeSwitch)
}

func placers() []placer {
	return []placer{
		{
			name: "ai",
			place: func(t *testing.T, s *Service, sw *fakeSwitch) string {
				if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"}); err != nil {
					t.Fatal(err)
				}
				return sw.lastOriginate().partyID.String()
			},
			acted: func(sw *fakeSwitch) int { return sw.bridgeCount() },
			wait:  func(t *testing.T, sw *fakeSwitch) { waitBridges(t, sw, 1) },
		},
		{
			name: "click-to-dial",
			place: func(t *testing.T, s *Service, sw *fakeSwitch) string {
				if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1001", To: "13912345678"}); err != nil {
					t.Fatal(err)
				}
				return sw.lastOriginate().partyID.String()
			},
			acted: func(sw *fakeSwitch) int { return sw.transferCount() },
			wait:  func(t *testing.T, sw *fakeSwitch) { waitTransfers(t, sw, 1) },
		},
	}
}

// An answer on or before the window bridges; one after it is hung up with the
// late-answer cause and its pending entry is gone. Both paths, every instant
// the issue names.
func TestAnswerAroundTheWindowBridgesOrHangsUp(t *testing.T) {
	window := config.OutboundAnswerWindow
	cases := []struct {
		after time.Duration
		late  bool
	}{
		{1 * time.Second, false},
		{30 * time.Second, false},
		{window - time.Millisecond, false},
		{window, false},
		{window + time.Millisecond, true},
		{59 * time.Second, false},
		{61 * time.Second, true},
		{120 * time.Second, true},
	}
	for _, p := range placers() {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.after.String(), func(t *testing.T) {
				sw := &fakeSwitch{}
				s := testService(t, sw, nil)
				clock := clocked(s)
				leg := p.place(t, s, sw)
				clock.t = clock.t.Add(tc.after)
				answer(s, leg)
				if tc.late {
					h := waitHangups(t, sw, 1)
					if h[0] != (hungUp{leg, LateAnswerCause}) {
						t.Errorf("hangup = %+v, want leg %s with %s", h[0], leg, LateAnswerCause)
					}
					settle()
					if p.acted(sw) != 0 {
						t.Error("a late answer was still bridged or transferred")
					}
				} else {
					p.wait(t, sw)
					settle()
					if h := sw.hangupList(); len(h) != 0 {
						t.Errorf("an on-time answer was hung up: %+v", h)
					}
				}
				if s.PendingCount() != 0 {
					t.Error("the pending entry survived the answer")
				}
			})
		}
	}
}

func TestAnswerForAnUnknownChannelDoesNothing(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	answer(s, uuid.NewString())
	settle()
	if len(sw.hangupList()) != 0 || sw.bridgeCount() != 0 || sw.transferCount() != 0 {
		t.Error("an answer for a channel nobody armed caused a command")
	}
}

// A second answer event for the same channel must not bridge twice, nor hang
// up a leg that was already taken on time.
func TestDuplicateAnswerActsOnce(t *testing.T) {
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			sw := &fakeSwitch{}
			s := testService(t, sw, nil)
			clock := clocked(s)
			leg := p.place(t, s, sw)
			answer(s, leg)
			p.wait(t, sw)
			clock.t = clock.t.Add(2 * config.OutboundAnswerWindow)
			answer(s, leg)
			settle()
			if p.acted(sw) != 1 {
				t.Errorf("continuation ran %d times, want 1", p.acted(sw))
			}
			if len(sw.hangupList()) != 0 {
				t.Error("the duplicate answer hung up a bridged leg")
			}
		})
	}
}

// An answer event after the leg's hangup is stale: nothing to bridge, and no
// command at a channel that no longer exists.
func TestAnswerAfterHangupDoesNothing(t *testing.T) {
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			sw := &fakeSwitch{}
			s := testService(t, sw, nil)
			clock := clocked(s)
			leg := p.place(t, s, sw)
			hangup(s, leg)
			clock.t = clock.t.Add(2 * config.OutboundAnswerWindow)
			answer(s, leg)
			settle()
			if p.acted(sw) != 0 || len(sw.hangupList()) != 0 {
				t.Error("an answer after the hangup caused a command")
			}
		})
	}
}

func TestOriginateCarriesATimeoutBelowTheWindow(t *testing.T) {
	for _, p := range placers() {
		t.Run(p.name, func(t *testing.T) {
			sw := &fakeSwitch{}
			s := testService(t, sw, nil)
			p.place(t, s, sw)
			got := sw.lastOriginate().vars["originate_timeout"]
			secs, err := strconv.Atoi(got)
			if err != nil {
				t.Fatalf("originate_timeout = %q, want whole seconds", got)
			}
			if d := time.Duration(secs) * time.Second; d != config.DefaultOutboundRingTimeout || d >= config.OutboundAnswerWindow {
				t.Errorf("originate_timeout = %s, want the default %s below the %s window",
					d, config.DefaultOutboundRingTimeout, config.OutboundAnswerWindow)
			}
		})
	}
}

// A campaign's per-attempt value rides the same parameter; one that would
// outlast the window is refused before anything is dialled.
func TestPerAttemptRingTimeout(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	if _, err := s.DialAI(context.Background(), AIDialRequest{
		To: "13912345678", DIDNumber: "95012", RingTimeout: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := sw.lastOriginate().vars["originate_timeout"]; got != "20" {
		t.Errorf("originate_timeout = %q, want 20", got)
	}
	for _, bad := range []time.Duration{config.OutboundAnswerWindow, 2 * config.OutboundAnswerWindow, -time.Second} {
		_, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012", RingTimeout: bad})
		if err != ErrBadRingTimeout {
			t.Errorf("DialAI ring %s: err = %v, want ErrBadRingTimeout", bad, err)
		}
		_, err = s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1001", To: "13912345678", RingTimeout: bad})
		if err != ErrBadRingTimeout {
			t.Errorf("Dial ring %s: err = %v, want ErrBadRingTimeout", bad, err)
		}
	}
	if n := len(sw.originates); n != 1 {
		t.Errorf("%d originates, want only the valid one", n)
	}
}

// placementLog keeps the attributes of each record by message.
type placementLog struct {
	mu      sync.Mutex
	records map[string][]map[string]any
}

func (l *placementLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *placementLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *placementLog) WithGroup(string) slog.Handler            { return l }
func (l *placementLog) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.records == nil {
		l.records = map[string][]map[string]any{}
	}
	l.records[r.Message] = append(l.records[r.Message], attrs)
	return nil
}

func (l *placementLog) of(msg string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.records[msg]
}

// The line that records a placement says which ring timeout the leg was sent
// with: the originate_timeout itself, the campaign's per-attempt value or the
// service's default.
func TestThePlacementLogsTheRingTimeoutItSent(t *testing.T) {
	tests := []struct {
		name  string
		msg   string
		place func(*Service) error
	}{
		{"a campaign attempt's own value", "ai outbound placed", func(s *Service) error {
			_, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012", RingTimeout: 20 * time.Second})
			return err
		}},
		{"a single AI call takes the default", "ai outbound placed", func(s *Service) error {
			_, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"})
			return err
		}},
		{"click-to-dial", "click-to-dial placed", func(s *Service) error {
			_, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1001", To: "13912345678"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sw := &fakeSwitch{}
			s := testService(t, sw, nil)
			log := &placementLog{}
			s.log = slog.New(log)
			if err := tt.place(s); err != nil {
				t.Fatal(err)
			}
			sent, err := strconv.Atoi(sw.lastOriginate().vars["originate_timeout"])
			if err != nil {
				t.Fatal(err)
			}
			records := log.of(tt.msg)
			if len(records) != 1 {
				t.Fatalf("%d %q records, want 1", len(records), tt.msg)
			}
			if got, ok := records[0]["ringTimeoutSec"].(int64); !ok || int(got) != sent {
				t.Errorf("ringTimeoutSec = %v, want the originate_timeout sent, %d", records[0]["ringTimeoutSec"], sent)
			}
		})
	}
}

func TestStartupRejectsARingTimeoutAtOrAboveTheWindow(t *testing.T) {
	for _, d := range []time.Duration{config.OutboundAnswerWindow, config.OutboundAnswerWindow + time.Second, 0, time.Second} {
		if err := config.ValidateOutboundRingTimeout(d); err == nil {
			t.Errorf("%s was accepted", d)
		}
	}
	for _, d := range []time.Duration{config.MinOutboundRingTimeout, config.DefaultOutboundRingTimeout, config.OutboundAnswerWindow - time.Second} {
		if err := config.ValidateOutboundRingTimeout(d); err != nil {
			t.Errorf("%s refused: %v", d, err)
		}
	}
	if !strings.Contains(config.ValidateOutboundRingTimeout(config.OutboundAnswerWindow).Error(), "AICC_OUTBOUND_RING_TIMEOUT") {
		t.Error("the error does not name the setting")
	}
}
