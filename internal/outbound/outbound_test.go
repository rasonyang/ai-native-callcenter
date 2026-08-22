// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

//
// Limiter.
//

// The bucket must pace above-rate bursts to the configured rate instead of
// letting them slam the switch's sessions-per-second cliff.
func TestLimiterPacesToTheConfiguredRate(t *testing.T) {
	limiter := NewLimiter(100)

	// Virtual time: the limiter sleeps by advancing a fake clock, so the
	// test asserts pacing math without waiting real seconds.
	var mu sync.Mutex
	now := time.Unix(0, 0)
	limiter.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	limiter.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
		return nil
	}
	limiter.lastFill = now
	limiter.tokens = limiter.burst

	for i := 0; i < 250; i++ {
		if err := limiter.Take(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	elapsed := now.Sub(time.Unix(0, 0))
	mu.Unlock()
	// 250 takes at 100/s with 100 burst: the first 100 are free, the next
	// 150 cost 1.5 virtual seconds.
	if elapsed < 1400*time.Millisecond || elapsed > 1700*time.Millisecond {
		t.Errorf("250 originates took %v of virtual time, want ≈1.5s", elapsed)
	}
}

func TestLimiterDoesNotDelayUnderTheRate(t *testing.T) {
	limiter := NewLimiter(100)
	slept := false
	limiter.sleep = func(context.Context, time.Duration) error {
		slept = true
		return nil
	}
	for i := 0; i < 100; i++ {
		if err := limiter.Take(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if slept {
		t.Error("a burst within the rate was delayed")
	}
}

func TestLimiterHonoursContextCancel(t *testing.T) {
	limiter := NewLimiter(1)
	_ = limiter.Take(context.Background()) // drain the single token

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Take(ctx); err == nil {
		t.Error("a cancelled context still took a token")
	}
}

//
// Service.
//

type originated struct {
	partyID  uuid.UUID
	endpoint string
	vars     map[string]string
}

type bridged struct {
	channelID string
	endpoint  string
	vars      map[string]string
}

type transferred struct {
	channelID string
	extension string
	context   string
}

type fakeSwitch struct {
	mu         sync.Mutex
	originates []originated
	bridges    []bridged
	transfers  []transferred
}

func (f *fakeSwitch) TransferToExtension(channelID, extension, context string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transfers = append(f.transfers, transferred{channelID, extension, context})
	return nil
}

func (f *fakeSwitch) transferCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.transfers)
}

func (f *fakeSwitch) lastTransfer() transferred {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transfers[len(f.transfers)-1]
}

func (f *fakeSwitch) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.originates = append(f.originates, originated{partyID, endpoint, vars})
	return "job-1", nil
}

func (f *fakeSwitch) BridgeToEndpoint(channelID string, _ uuid.UUID, endpoint string, vars map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridges = append(f.bridges, bridged{channelID, endpoint, vars})
	return nil
}

func (f *fakeSwitch) Endpoint(ext string) string { return "user/" + ext + "@test" }

func (f *fakeSwitch) lastOriginate() originated {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.originates[len(f.originates)-1]
}

func (f *fakeSwitch) bridgeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bridges)
}

func (f *fakeSwitch) lastBridge() bridged {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bridges[len(f.bridges)-1]
}

type fakeDIDs []catalog.DID

func (f fakeDIDs) DIDs(context.Context) ([]catalog.DID, error) { return f, nil }

func testService(t *testing.T, sw *fakeSwitch, history map[uuid.UUID]bool) *Service {
	t.Helper()
	flowID := uuid.New()
	dids := fakeDIDs{{Number: "95012", Language: "zh", FlowID: &flowID, IsEnabled: true}}
	return New(Config{}, sw, dids,
		func(_ context.Context, id uuid.UUID) (bool, error) { return history[id], nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func answer(s *Service, channelID string) {
	s.HandleSwitchEvent(telephony.SwitchEvent{
		Kind: telephony.KindChannelAnswer, ChannelID: channelID,
	})
}

func waitTransfers(t *testing.T, sw *fakeSwitch, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sw.transferCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("transfer count never reached %d", want)
}

func waitBridges(t *testing.T, sw *fakeSwitch, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sw.bridgeCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("bridge count never reached %d", want)
}

// Click-to-dial rings the agent first and bridges out only on their answer,
// with the minted call id riding every leg.
func TestDialIsAgentFirst(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)

	callID, err := s.Dial(context.Background(), "1001", "13912345678")
	if err != nil {
		t.Fatal(err)
	}

	first := sw.lastOriginate()
	if first.endpoint != "user/1001@test" {
		t.Errorf("first leg went to %q, want the agent", first.endpoint)
	}
	if first.vars["aicc_call_id"] != callID.String() {
		t.Error("the agent leg does not carry the minted call id")
	}
	if first.vars["sip_auto_answer"] != "true" {
		t.Error("the agent clicked; their leg should auto-answer")
	}
	// The next leg inherits this one's codec, and a G.711-only phone rejects
	// an inherited opus offer outright (found live).
	if pin := first.vars["absolute_codec_string"]; pin != "PCMU" {
		t.Errorf("agent leg codec pin = %q, want PCMU", pin)
	}
	if sw.transferCount() != 0 {
		t.Fatal("the customer was dialed before the agent answered")
	}

	answer(s, first.partyID.String())
	waitTransfers(t, sw, 1)
	move := sw.lastTransfer()
	if move.channelID != first.partyID.String() {
		t.Error("the transfer did not ride the agent's channel")
	}
	if move.extension != "13912345678" {
		t.Errorf("transferred to %q, want the destination itself", move.extension)
	}
	// The dialplan owns routing: an extension stays internal, a carrier
	// number leaves through its gateway. Naming an endpoint here would be a
	// second copy of that decision. The dialplan in question is aicc's own —
	// the stock one defines none of these extensions (design 01 §7 D6a).
	if move.context != "aicc" {
		t.Errorf("context = %q, want aicc's own dialplan", move.context)
	}
	if sw.bridgeCount() != 0 {
		t.Error("click-to-dial built a bridge instead of using the dialplan")
	}
}

// A declined agent leg must not dial the customer at all.
func TestDialDoesNothingWhenTheAgentDeclines(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)

	if _, err := s.Dial(context.Background(), "1001", "13912345678"); err != nil {
		t.Fatal(err)
	}
	leg := sw.lastOriginate().partyID.String()
	s.HandleSwitchEvent(telephony.SwitchEvent{
		Kind: telephony.KindChannelHangup, ChannelID: leg,
	})
	answer(s, leg) // a late answer event for the same channel
	time.Sleep(20 * time.Millisecond)

	if sw.transferCount() != 0 {
		t.Error("a declined dial still rang the customer")
	}
	if s.PendingCount() != 0 {
		t.Error("the declined leg leaked a pending entry")
	}
}

// AI outbound: originate to the customer; on answer, bridge into the bot
// gateway with the same correlation headers an inbound call carries — plus
// the direction, which the recorder must not guess.
func TestDialAIBridgesTheAnsweredCustomerToTheBot(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)

	callID, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"})
	if err != nil {
		t.Fatal(err)
	}

	first := sw.lastOriginate()
	if !strings.Contains(first.endpoint, "13912345678") {
		t.Errorf("originate went to %q, want the customer", first.endpoint)
	}
	if first.vars["origination_caller_id_number"] != "95012" {
		t.Error("the customer should see the DID calling")
	}

	answer(s, first.partyID.String())
	waitBridges(t, sw, 1)
	bridge := sw.lastBridge()
	if !strings.Contains(bridge.endpoint, "aicc_bot/95012") {
		t.Errorf("bridge endpoint %q is not the bot gateway entry", bridge.endpoint)
	}
	for header, want := range map[string]string{
		"sip_h_X-AICC-Call-ID":    callID.String(),
		"sip_h_X-AICC-DID":        "95012",
		"sip_h_X-AICC-Language":   "zh",
		"sip_h_X-AICC-Channel-ID": first.partyID.String(),
		"sip_h_X-AICC-Call-Type":  "OUTBOUND",
		"aicc_did":                "95012",
	} {
		if bridge.vars[header] != want {
			t.Errorf("%s = %q, want %q", header, bridge.vars[header], want)
		}
	}
}

// A retry with the same call id never redials a finished or running call.
func TestDialAIIsIdempotentAgainstTheLedger(t *testing.T) {
	callID := uuid.New()
	sw := &fakeSwitch{}
	s := testService(t, sw, map[uuid.UUID]bool{callID: true})

	if _, err := s.DialAI(context.Background(), AIDialRequest{
		CallID: callID, To: "13912345678", DIDNumber: "95012",
	}); err != ErrAlreadyPlaced {
		t.Fatalf("err = %v, want ErrAlreadyPlaced", err)
	}
	if len(sw.originates) != 0 {
		t.Error("a finished call was redialed")
	}
}

// A pinned loopback dies with DESTINATION_OUT_OF_ORDER before routing (found
// live): the G.711 pin may only ride legs that leave through sofia.
func TestCodecPinNeverRidesLoopbackLegs(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil) // default endpoint format is loopback

	_, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"})
	if err != nil {
		t.Fatal(err)
	}
	first := sw.lastOriginate()
	if _, pinned := first.vars["absolute_codec_string"]; pinned {
		t.Error("a loopback customer leg was codec-pinned")
	}

	answer(s, first.partyID.String())
	waitBridges(t, sw, 1)
	pin := sw.lastBridge().vars["absolute_codec_string"]
	if pin == "" {
		t.Error("the sofia bot leg lost its codec pin")
	}
	if strings.Contains(pin, ",") {
		t.Errorf("pin %q contains a comma — a list separator inside a var block cancels the originate", pin)
	}
}

func TestDialAIRejectsUnknownAndFlowlessNumbers(t *testing.T) {
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "99999"}); err != ErrUnknownDID {
		t.Errorf("unknown DID: err = %v", err)
	}
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "not-a-number", DIDNumber: "95012"}); err != ErrBadNumber {
		t.Errorf("bad number: err = %v", err)
	}
}

// The ledger must not call a walk down the hall an outbound call: to the
// switch every originated leg is outbound, so click-to-dial stamps what it
// knows while the destination is still in hand (found live: an extension-to-
// extension dial recorded as OUTBOUND).
func TestDialStampsInternalVersusOutbound(t *testing.T) {
	for _, tc := range []struct{ destination, want string }{
		{"1007", "INTERNAL"},
		{"18688886669", "OUTBOUND"},
	} {
		sw := &fakeSwitch{}
		s := testService(t, sw, nil)
		if _, err := s.Dial(context.Background(), "1008", tc.destination); err != nil {
			t.Fatal(err)
		}
		if got := sw.lastOriginate().vars["aicc_call_type"]; got != tc.want {
			t.Errorf("dialling %s stamped %q, want %q", tc.destination, got, tc.want)
		}
	}
}
