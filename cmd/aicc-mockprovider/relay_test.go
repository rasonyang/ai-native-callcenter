// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const wait = 3 * time.Second

// fakeUpstream stands in for the real provider: it records what the relay
// sends it and speaks when the test tells it to.
type fakeUpstream struct {
	srv    *httptest.Server
	recv   chan string
	conns  chan *websocket.Conn
	header chan http.Header
	query  chan string
	path   chan string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		recv: make(chan string, 64), conns: make(chan *websocket.Conn, 4),
		header: make(chan http.Header, 4), query: make(chan string, 4), path: make(chan string, 4),
	}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.header <- r.Header.Clone()
		f.query <- r.URL.RawQuery
		f.path <- r.URL.Path
		f.conns <- c
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			f.recv <- string(data)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) wsURL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

// session is one client connected through a relay to the fake upstream.
type session struct {
	t    *testing.T
	cli  *websocket.Conn
	up   *websocket.Conn
	fake *fakeUpstream
	in   chan string // frames the client received
}

func connect(t *testing.T, cfg RelayConfig, path string, h http.Header) *session {
	t.Helper()
	fake := newFakeUpstream(t)
	cfg.Upstream = fake.wsURL() + "/api-ws/v1/realtime"
	relay := httptest.NewServer(NewRelay(cfg).Handler())
	t.Cleanup(relay.Close)
	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(relay.URL, "http")+path, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	s := &session{t: t, cli: cli, fake: fake, in: make(chan string, 64)}
	select {
	case s.up = <-fake.conns:
	case <-time.After(wait):
		t.Fatal("relay never dialled upstream")
	}
	go func() {
		for {
			_, data, err := cli.ReadMessage()
			if err != nil {
				close(s.in)
				return
			}
			s.in <- string(data)
		}
	}()
	return s
}

func (s *session) upSend(msg string) {
	s.t.Helper()
	if err := s.up.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) cliSend(msg string) {
	s.t.Helper()
	if err := s.cli.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) clientGets() string {
	s.t.Helper()
	select {
	case m, ok := <-s.in:
		if !ok {
			s.t.Fatal("client socket closed")
		}
		return m
	case <-time.After(wait):
		s.t.Fatal("client received nothing")
	}
	return ""
}

func (s *session) upstreamGets() string {
	s.t.Helper()
	select {
	case m := <-s.fake.recv:
		return m
	case <-time.After(wait):
		s.t.Fatal("upstream received nothing")
	}
	return ""
}

// clientGetsType expects the next client frame to be of the given type.
func (s *session) clientGetsType(typ string) string {
	s.t.Helper()
	m := s.clientGets()
	if typeOf(m) != typ {
		s.t.Fatalf("client got %s, want type %s", m, typ)
	}
	return m
}

func typeOf(m string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(m), &e)
	return e.Type
}

// marker proves that nothing hidden is still in flight: frames of one
// direction keep their order, so when the marker arrives, everything before
// it either arrived first or was swallowed.
func (s *session) expectMarker() {
	s.t.Helper()
	s.upSend(`{"type":"test.marker"}`)
	if got := s.clientGets(); typeOf(got) != "test.marker" {
		s.t.Fatalf("expected the marker next, got %s", got)
	}
}

// vadSpeech is the provider hearing the caller finish a turn, which makes the
// next response.created a VAD turn.
func (s *session) vadSpeech() {
	s.t.Helper()
	s.upSend(`{"type":"input_audio_buffer.speech_stopped"}`)
	s.clientGetsType("input_audio_buffer.speech_stopped")
}

func created(id string) string {
	return `{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`
}
func done(id string, items ...string) string {
	out := []string{}
	for _, i := range items {
		out = append(out, `{"id":"`+i+`"}`)
	}
	return `{"type":"response.done","response":{"id":"` + id + `","status":"completed","output":[` + strings.Join(out, ",") + `]}}`
}
func audio(id string) string {
	return `{"type":"response.audio.delta","response_id":"` + id + `","item_id":"i-` + id + `","delta":"AAAA"}`
}
func transcript(id string) string {
	return `{"type":"response.audio_transcript.delta","response_id":"` + id + `","item_id":"i-` + id + `","delta":"hello"}`
}

const sessionUpdated = `{"type":"session.updated"}`

func faultCfg(f Fault, turn int) RelayConfig {
	return RelayConfig{Fault: f, Ratio: 1, Turns: TurnRange{turn, turn}, Seed: 1,
		Hold: time.Minute, StallTimeout: time.Minute}
}

func TestTheRelayCarriesFramesBothWaysAndTheHandshakeThrough(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret-key")
	h.Set("OpenAI-Beta", "realtime=v1")
	s := connect(t, RelayConfig{}, "/anything/at/all?model=qwen-x&foo=1", h)

	got := <-s.fake.header
	if got.Get("Authorization") != "Bearer secret-key" || got.Get("OpenAI-Beta") != "realtime=v1" {
		t.Fatalf("headers not passed through: %v", got)
	}
	if q := <-s.fake.query; q != "model=qwen-x&foo=1" {
		t.Fatalf("query = %q", q)
	}
	if p := <-s.fake.path; p != "/api-ws/v1/realtime" {
		t.Fatalf("path = %q: the upstream URL decides the path, not the client's", p)
	}

	s.cliSend(`{"type":"session.update","x":1}`)
	if m := s.upstreamGets(); m != `{"type":"session.update","x":1}` {
		t.Fatalf("upstream got %s", m)
	}
	s.upSend(`{"type":"response.audio.delta","delta":"AAAA"}`)
	if m := s.clientGets(); m != `{"type":"response.audio.delta","delta":"AAAA"}` {
		t.Fatalf("client got %s", m)
	}
	if err := s.cli.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if m := s.upstreamGets(); m != "\x01\x02\x03" {
		t.Fatalf("binary frame not relayed: %q", m)
	}

	// The upstream closing ends the client's socket too.
	_ = s.up.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second))
	select {
	case _, ok := <-s.in:
		if ok {
			t.Fatal("unexpected frame")
		}
	case <-time.After(wait):
		t.Fatal("client socket stayed open after the upstream closed")
	}
}

func TestTheDecisionIsAPureFunctionOfSeedAndSession(t *testing.T) {
	turns := TurnRange{2, 4}
	faulted := 0
	for n := 1; n <= 400; n++ {
		f1, t1 := decide(7, n, 0.5, turns)
		f2, t2 := decide(7, n, 0.5, turns)
		if f1 != f2 || t1 != t2 {
			t.Fatalf("session %d decided differently twice", n)
		}
		if t1 < 2 || t1 > 4 {
			t.Fatalf("session %d turn %d outside 2-4", n, t1)
		}
		if f1 {
			faulted++
		}
		if all, _ := decide(7, n, 1, turns); !all {
			t.Fatalf("ratio 1 left session %d clean", n)
		}
		if none, _ := decide(7, n, 0, turns); none {
			t.Fatalf("ratio 0 faulted session %d", n)
		}
	}
	if faulted < 140 || faulted > 260 {
		t.Fatalf("ratio 0.5 faulted %d of 400", faulted)
	}
	differs := false
	for n := 1; n <= 50; n++ {
		a, ta := decide(1, n, 0.5, turns)
		b, tb := decide(2, n, 0.5, turns)
		differs = differs || a != b || ta != tb
	}
	if !differs {
		t.Fatal("two seeds produced the same plan")
	}
	seen := map[int]bool{}
	for n := 1; n <= 200; n++ {
		_, turn := decide(1, n, 1, turns)
		seen[turn] = true
	}
	if len(seen) != 3 {
		t.Fatalf("turns drawn: %v, want all of 2, 3, 4", seen)
	}
}

func TestParsingTheFlags(t *testing.T) {
	if r, err := ParseTurnRange("2-4"); err != nil || r != (TurnRange{2, 4}) {
		t.Fatalf("2-4: %v %v", r, err)
	}
	if r, err := ParseTurnRange("3"); err != nil || r != (TurnRange{3, 3}) {
		t.Fatalf("3: %v %v", r, err)
	}
	for _, bad := range []string{"0-2", "4-2", "x", ""} {
		if _, err := ParseTurnRange(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := ParseFault("nonsense"); err == nil {
		t.Fatal("unknown fault accepted")
	}
}

func TestAnUnfaultedSessionIsUntouchedWhateverTheFaultKind(t *testing.T) {
	cfg := faultCfg(FaultStall, 2)
	cfg.Ratio = 0
	s := connect(t, cfg, "/x", nil)
	for _, m := range []string{sessionUpdated, created("r1"), done("r1"), created("r2"), audio("r2"), done("r2")} {
		s.upSend(m)
		if got := s.clientGets(); got != m {
			t.Fatalf("client got %s, want %s", got, m)
		}
	}
}

func TestStallDropsTheAudioHoldsTheDoneAndCancelsIt(t *testing.T) {
	s := connect(t, faultCfg(FaultStall, 1), "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.upSend(created("r1")) // the greeting is not a VAD turn and passes whole
	s.clientGetsType("response.created")
	s.upSend(audio("r1"))
	s.clientGetsType("response.audio.delta")
	s.upSend(done("r1"))
	s.clientGetsType("response.done")

	s.vadSpeech()
	s.upSend(created("r2"))
	s.clientGetsType("response.created")
	s.upSend(audio("r2"))
	s.upSend(`{"type":"response.output_audio.delta","response_id":"r2","delta":"BBBB"}`)
	s.upSend(transcript("r2"))
	s.clientGetsType("response.audio_transcript.delta") // the audio before it was dropped
	s.upSend(done("r2"))
	s.expectMarker() // the done is held

	s.cliSend(`{"type":"response.cancel"}`)
	if m := s.upstreamGets(); typeOf(m) != "response.cancel" {
		t.Fatalf("the cancel was not forwarded: %s", m)
	}
	var d struct {
		Response struct{ ID, Status string } `json:"response"`
	}
	_ = json.Unmarshal([]byte(s.clientGetsType("response.done")), &d)
	if d.Response.ID != "r2" || d.Response.Status != "cancelled" {
		t.Fatalf("held done = %+v", d.Response)
	}

	// The provider's refusal of the cancel is the relay's business, not the client's.
	s.upSend(`{"type":"error","error":{"message":"Cannot cancel: No active response found"}}`)
	s.expectMarker()
	// Other errors still reach the client.
	s.upSend(`{"type":"error","error":{"message":"something else"}}`)
	s.clientGetsType("error")
}

func TestStallDeliversTheHeldDoneAsIsWhenNoCancelComes(t *testing.T) {
	cfg := faultCfg(FaultStall, 1)
	cfg.StallTimeout = 50 * time.Millisecond
	s := connect(t, cfg, "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.vadSpeech()
	s.upSend(created("r1"))
	s.clientGetsType("response.created")
	s.upSend(audio("r1"))
	s.upSend(done("r1"))
	if got := s.clientGetsType("response.done"); !strings.Contains(got, `"completed"`) {
		t.Fatalf("done = %s, want the status it came with", got)
	}
}

func TestStallHoldRefusesNewResponsesAndReleasesOnBargeIn(t *testing.T) {
	s := connect(t, faultCfg(FaultStallHold, 1), "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.vadSpeech()
	s.upSend(created("r1"))
	s.clientGetsType("response.created")
	s.upSend(audio("r1"))
	s.upSend(done("r1"))
	s.expectMarker()

	// A new response is refused by the relay and never reaches the provider.
	s.cliSend(`{"type":"response.create"}`)
	if got := s.clientGetsType("error"); !strings.Contains(got, "another response is in progress") ||
		!strings.Contains(got, `"code":"invalid_value"`) {
		t.Fatalf("refusal = %s", got)
	}
	// A conversation item still goes through.
	s.cliSend(`{"type":"conversation.item.create","item":{"id":"c1"}}`)
	if m := s.upstreamGets(); typeOf(m) != "conversation.item.create" {
		t.Fatalf("upstream got %s", m)
	}
	// The cancel is forwarded but not honoured.
	s.cliSend(`{"type":"response.cancel"}`)
	if m := s.upstreamGets(); typeOf(m) != "response.cancel" {
		t.Fatalf("upstream got %s", m)
	}
	s.upSend(`{"type":"error","error":{"message":"no active response to cancel"}}`)
	s.expectMarker() // refusal swallowed, done still held

	s.upSend(`{"type":"input_audio_buffer.speech_started"}`)
	s.clientGetsType("input_audio_buffer.speech_started")
	if got := s.clientGetsType("response.done"); !strings.Contains(got, `"cancelled"`) {
		t.Fatalf("done = %s", got)
	}
	// Once released, a new response is the provider's to judge.
	s.cliSend(`{"type":"response.create"}`)
	if m := s.upstreamGets(); typeOf(m) != "response.create" {
		t.Fatalf("upstream got %s", m)
	}
}

func TestStallHoldReleasesAfterTheHoldTime(t *testing.T) {
	cfg := faultCfg(FaultStallHold, 1)
	cfg.Hold = 50 * time.Millisecond
	s := connect(t, cfg, "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.vadSpeech()
	s.upSend(created("r1"))
	s.clientGetsType("response.created")
	s.upSend(done("r1"))
	if got := s.clientGetsType("response.done"); !strings.Contains(got, `"cancelled"`) {
		t.Fatalf("done = %s", got)
	}
}

// playUnanswered drives a session through a greeting, a typed-in response
// (not a VAD turn) and then a VAD turn the relay must swallow. It returns
// after the swallowed response's done.
func playUnanswered(t *testing.T, s *session, errFrame bool) {
	t.Helper()
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.upSend(created("greet"))
	s.clientGetsType("response.created")
	s.upSend(done("greet"))
	s.clientGetsType("response.done")

	// Not a VAD turn: the client asked for it, so it is not eligible.
	s.upSend(`{"type":"input_audio_buffer.speech_stopped"}`)
	s.clientGetsType("input_audio_buffer.speech_stopped")
	s.cliSend(`{"type":"response.create"}`)
	if m := s.upstreamGets(); typeOf(m) != "response.create" {
		t.Fatalf("upstream got %s", m)
	}
	s.upSend(created("manual"))
	s.clientGetsType("response.created")
	s.upSend(done("manual"))
	s.clientGetsType("response.done")

	// The VAD turn.
	s.upSend(`{"type":"input_audio_buffer.speech_started"}`)
	s.clientGetsType("input_audio_buffer.speech_started")
	s.upSend(`{"type":"input_audio_buffer.speech_stopped"}`)
	s.clientGetsType("input_audio_buffer.speech_stopped")
	s.upSend(`{"type":"input_audio_buffer.committed"}`)
	s.clientGetsType("input_audio_buffer.committed")
	s.upSend(created("vad"))
	if errFrame {
		if got := s.clientGetsType("error"); !strings.Contains(got, "no response was created because a manual response is already in progress") {
			t.Fatalf("error = %s", got)
		}
	}
	if m := s.upstreamGets(); typeOf(m) != "response.cancel" {
		t.Fatalf("upstream got %s, want response.cancel", m)
	}
	s.upSend(`{"type":"response.output_item.added","response_id":"vad","item":{"id":"i1"}}`)
	s.upSend(`{"type":"conversation.item.created","item":{"id":"i1"}}`)
	s.upSend(audio("vad"))
	s.upSend(transcript("vad"))
	s.upSend(`{"type":"response.function_call_arguments.done","response_id":"vad","item_id":"i2"}`)
	// The caller's own transcript is not part of the response.
	s.upSend(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"hi"}`)
	s.clientGetsType("conversation.item.input_audio_transcription.completed")
	s.upSend(done("vad", "i1", "i2"))
	s.upSend(`{"type":"error","error":{"message":"Cannot cancel: no active response"}}`)

	deleted := map[string]bool{}
	for range 2 {
		m := s.upstreamGets()
		var d struct {
			Type   string `json:"type"`
			ItemID string `json:"item_id"`
		}
		_ = json.Unmarshal([]byte(m), &d)
		if d.Type != "conversation.item.delete" {
			t.Fatalf("upstream got %s, want conversation.item.delete", m)
		}
		deleted[d.ItemID] = true
	}
	if !deleted["i1"] || !deleted["i2"] {
		t.Fatalf("deleted %v", deleted)
	}
	s.upSend(`{"type":"conversation.item.deleted","item_id":"i1"}`)
	s.upSend(`{"type":"error","error":{"message":"item i2 not found"}}`)
	s.expectMarker()
}

func TestUnansweredHidesTheResponseButNotTheCallersSide(t *testing.T) {
	s := connect(t, faultCfg(FaultUnanswered, 1), "/x", nil)
	playUnanswered(t, s, false)
	// The fault is spent: the next VAD turn is answered normally.
	s.upSend(`{"type":"input_audio_buffer.speech_stopped"}`)
	s.clientGetsType("input_audio_buffer.speech_stopped")
	s.upSend(created("later"))
	s.clientGetsType("response.created")
}

func TestVADRefusedAddsQwensErrorToTheSwallowedResponse(t *testing.T) {
	s := connect(t, faultCfg(FaultVADRefused, 1), "/x", nil)
	playUnanswered(t, s, true)
}

func TestTheChosenVADTurnIsCountedOverVADTurnsOnly(t *testing.T) {
	// Turn 2 of the VAD turns: the first one passes, the second is swallowed.
	s := connect(t, faultCfg(FaultUnanswered, 2), "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	for _, id := range []string{"a", "b"} {
		s.upSend(`{"type":"input_audio_buffer.speech_stopped"}`)
		s.clientGetsType("input_audio_buffer.speech_stopped")
		s.upSend(created(id))
		if id == "a" {
			s.clientGetsType("response.created")
			s.upSend(done(id))
			s.clientGetsType("response.done")
		}
	}
	if m := s.upstreamGets(); typeOf(m) != "response.cancel" {
		t.Fatalf("upstream got %s", m)
	}
	s.expectMarker()
}

func TestOnlyARefusalOfADeleteIsHiddenAndOnlyOnce(t *testing.T) {
	s := connect(t, faultCfg(FaultUnanswered, 1), "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.vadSpeech()
	s.upSend(created("vad"))
	if m := s.upstreamGets(); typeOf(m) != "response.cancel" {
		t.Fatalf("upstream got %s", m)
	}
	s.upSend(`{"type":"response.output_item.added","response_id":"vad","item":{"id":"i1"}}`)
	s.upSend(done("vad", "i1", "i2"))
	for range 2 {
		if m := s.upstreamGets(); typeOf(m) != "conversation.item.delete" {
			t.Fatalf("upstream got %s", m)
		}
	}
	// One delete succeeds, the other is refused: both are the relay's business.
	s.upSend(`{"type":"conversation.item.deleted","item_id":"i1"}`)
	s.upSend(`{"type":"error","error":{"message":"The item with id i2 was Not Found"}}`)
	s.expectMarker()
	// Nothing is outstanding now: whatever the provider complains of next,
	// including the real errors of the faulted turn, reaches the client.
	s.upSend(refusedVADTurn)
	s.clientGetsType("error")
	s.upSend(refusedWhileBusy)
	s.clientGetsType("error")
}

func TestAnErrorIsNotHiddenWhileADeleteIsOutstandingUnlessItRefusesIt(t *testing.T) {
	s := connect(t, faultCfg(FaultUnanswered, 1), "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.vadSpeech()
	s.upSend(created("vad"))
	s.upstreamGets()
	s.upSend(done("vad", "i1"))
	s.upstreamGets()
	s.upSend(refusedVADTurn)
	s.clientGetsType("error")
}

func TestAResponseCreatedWithoutAnIDLeavesTheFaultForTheNextTurn(t *testing.T) {
	s := connect(t, faultCfg(FaultStall, 1), "/x", nil)
	s.upSend(sessionUpdated)
	s.clientGetsType("session.updated")
	s.vadSpeech()
	s.upSend(`{"type":"response.created","response":{}}`)
	s.clientGetsType("response.created")
	s.upSend(audio(""))
	s.clientGetsType("response.audio.delta") // nothing was faulted
	s.upSend(`{"type":"response.done","response":{}}`)
	s.clientGetsType("response.done")

	s.vadSpeech()
	s.upSend(created("r2"))
	s.clientGetsType("response.created")
	s.upSend(audio("r2"))
	s.upSend(done("r2"))
	s.expectMarker() // audio dropped and done held: the next turn took the fault
}
