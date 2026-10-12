// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

const emptyListing = `{"row_count":0,"rows":[]}`

func closeFixture(t *testing.T, listing func() string) (*Coordinator, *Registry, *capturingPublisher) {
	t.Helper()
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	t.Cleanup(registry.Shutdown)
	cmd := &fakeCommander{up: true, replyFor: func(cmd string) (string, error) {
		if cmd == "show channels as json" {
			return listing(), nil
		}
		return "", nil
	}}
	return NewCoordinator(registry, NewAdapter(cmd, "aicc.test"), noAgents{}, pub), registry, pub
}

func releasedWith(pub *capturingPublisher, cause string) bool {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	for _, ev := range pub.events {
		if ev.Type == events.TypePartyReleased && ev.Payload["cause"] == cause {
			return true
		}
	}
	return false
}

// A hangup lost during an outage leaves the call open for ever. The listing
// proves the channel is gone, and the ending goes through the ordinary path.
func TestAChannelThatVanishedDuringAnOutageIsClosedThroughTheHangupPath(t *testing.T) {
	t.Parallel()
	c, registry, pub := closeFixture(t, func() string { return emptyListing })
	callID := uuid.New()
	vars := map[string]string{"variable_aicc_call_id": callID.String()}
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "lost-chan", "inbound", vars))
	waitFor(t, func() bool { return registry.Owns("lost-chan") })
	time.Sleep(5 * time.Millisecond)

	if _, err := c.AdoptLiveChannels(t.Context()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return !registry.Owns("lost-chan") })
	if !releasedWith(pub, VanishedCause) {
		t.Errorf("no PARTY_RELEASED carried %s", VanishedCause)
	}
}

// The same on a reconnect, which is where it matters: the registry is warm.
func TestAChannelStillListedIsNotClosed(t *testing.T) {
	t.Parallel()
	id := uuid.New().String()
	c, registry, _ := closeFixture(t, func() string {
		return `{"row_count":1,"rows":[{"uuid":"` + id + `","callstate":"ACTIVE"}]}`
	})
	vars := map[string]string{"variable_aicc_call_id": uuid.NewString()}
	c.Handle(t.Context(), raw("CHANNEL_CREATE", id, "inbound", vars))
	time.Sleep(5 * time.Millisecond)

	if _, err := c.AdoptLiveChannels(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if !registry.Owns(id) {
		t.Error("a channel the switch still lists was closed")
	}
}

// A channel the registry first saw after the snapshot began may simply be too
// new for the listing.
func TestAChannelBornDuringTheSnapshotIsNotClosed(t *testing.T) {
	t.Parallel()
	var c *Coordinator
	var once sync.Once
	var registry *Registry
	c, registry, _ = closeFixture(t, func() string {
		once.Do(func() {
			time.Sleep(2 * time.Millisecond)
			vars := map[string]string{"variable_aicc_call_id": uuid.NewString()}
			c.Handle(t.Context(), raw("CHANNEL_CREATE", "newborn", "inbound", vars))
		})
		return emptyListing
	})
	if _, err := c.AdoptLiveChannels(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if !registry.Owns("newborn") {
		t.Error("a call that started during the reconcile was closed")
	}
}

// A listing that is cut off proves nothing about what is missing from it.
func TestATruncatedListingClosesNothing(t *testing.T) {
	t.Parallel()
	c, registry, _ := closeFixture(t, func() string { return `{"row_count":3,"rows":[{"uuid":"` })
	vars := map[string]string{"variable_aicc_call_id": uuid.NewString()}
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "kept", "inbound", vars))
	time.Sleep(5 * time.Millisecond)

	if _, err := c.AdoptLiveChannels(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if !registry.Owns("kept") {
		t.Error("an incomplete listing closed a call")
	}
}

// A bridge heard twice (replayed, then announced by the switch) attaches the
// tap and addresses the transcript once.
func TestTheSameBridgeTwiceAttachesTheTapOnce(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	taps := newRecordingTapper()
	c.AttachTaps(taps)
	audiences := newRecordingAudiences()
	c.AttachAudiences(audiences)

	ctx := t.Context()
	minted := uuid.New().String()
	callerChan, agentChan := "caller-chan", "agent-chan"
	vars := map[string]string{"variable_aicc_call_id": minted, "variable_aicc_language": "en"}
	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound", map[string]string{
		"variable_dialed_user":            agentExtension,
		"variable_cc_member_session_uuid": callerChan,
	}))
	bridge := raw("CHANNEL_BRIDGE", agentChan, "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": callerChan}))
	c.Handle(ctx, bridge)
	c.Handle(ctx, bridge)

	waitFor(t, func() bool {
		attached, _, _, _ := taps.snapshot()
		return len(attached) > 0
	})
	time.Sleep(20 * time.Millisecond)
	if attached, _, _, _ := taps.snapshot(); len(attached) != 1 {
		t.Errorf("tap attached %d times for one bridge: %v", len(attached), attached)
	}

	// And a real re-bridge after the bridge ended is new.
	c.Handle(ctx, raw("CHANNEL_UNBRIDGE", agentChan, "outbound", vars))
	c.Handle(ctx, bridge)
	waitFor(t, func() bool {
		attached, _, _, _ := taps.snapshot()
		return len(attached) == 2
	})
}

// slowDetach counts how many Handle calls are inside the tap at once.
type slowTapper struct {
	*recordingTapper
	inFlight, peak atomic.Int32
}

func (s *slowTapper) Detach(c string) {
	n := s.inFlight.Add(1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	s.inFlight.Add(-1)
}

// Handle is never entered by two goroutines: the dispatch loop and a reconcile
// replay take turns.
func TestHandleIsNeverRunConcurrently(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})
	taps := &slowTapper{recordingTapper: newRecordingTapper()}
	c.AttachTaps(taps)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := strings.Repeat("x", i+1)
			for range 5 {
				c.Handle(t.Context(), raw("CHANNEL_UNBRIDGE", id, "inbound", nil))
			}
		}()
	}
	wg.Wait()
	if peak := taps.peak.Load(); peak != 1 {
		t.Errorf("%d goroutines were inside Handle at once", peak)
	}
}
