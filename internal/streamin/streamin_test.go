// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// stallSession accepts audio only when released, so a test can hold the
// recogniser still and watch the queue overflow deliberately.
type stallSession struct {
	mu       sync.Mutex
	received [][]byte
	release  chan struct{}
	commits  int
	events   chan transcribe.Event
}

func newStallSession() *stallSession {
	return &stallSession{release: make(chan struct{}), events: make(chan transcribe.Event)}
}

func (s *stallSession) Start(context.Context, transcribe.Config) error { return nil }
func (s *stallSession) Events() <-chan transcribe.Event                { return s.events }
func (s *stallSession) Close(context.Context) error                    { close(s.events); return nil }

func (s *stallSession) SendAudio(pcm []byte) error {
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, pcm)
	return nil
}

func (s *stallSession) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	return nil
}

func (s *stallSession) commitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits
}

func frame(samples int, amplitude int16) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(amplitude))
	}
	return b
}

// The queue drops the oldest frame rather than blocking its writer — the
// switch is on the other end of that write, and stalling it to keep audio is
// the worse failure. But a drop leaves no other trace, so it is counted: a
// transcript with a hole in it reads the same whether the engine mis-heard the
// words or we never sent them.
func TestOverflowDropsOldestAndCountsIt(t *testing.T) {
	client := newStallSession()
	p := newPump("HUMAN_AGENT", "qwen", client, false, nopLogger{})
	t.Cleanup(func() { close(client.release); p.close(context.Background()) })

	const over = queueDepth + 50
	for i := 0; i < over; i++ {
		p.write(frame(160, 1000))
	}

	// The writer never blocked: reaching here at all is the assertion.
	_, dropped := p.stats()
	if dropped == 0 {
		t.Fatal("the queue overflowed and reported no drops — a lost line would be untraceable")
	}
	if dropped > int64(over) {
		t.Errorf("dropped %d of %d frames, which is more than were written", dropped, over)
	}
	t.Logf("dropped %d of %d frames written while the recogniser stalled", dropped, over)
}

// Where the engine refuses to end an utterance, the pump must. Without this a
// final never arrives at all on that path.
func TestSilenceCommitsWhereWeOwnEndpointing(t *testing.T) {
	client := newStallSession()
	close(client.release) // accept audio immediately
	p := newPump("CUSTOMER", "openai", client, true, nopLogger{})
	t.Cleanup(func() { p.close(context.Background()) })

	for i := 0; i < 5; i++ {
		p.write(frame(160, 8000)) // speech
	}
	for i := 0; i < 5; i++ {
		p.write(frame(160, 5)) // then quiet
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if client.commitCount() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("silence never ended the utterance; on this engine no final would ever arrive")
}

// Where the engine segments for itself, committing would be wrong.
func TestNoCommitWhereTheEngineOwnsEndpointing(t *testing.T) {
	client := newStallSession()
	close(client.release)
	p := newPump("CUSTOMER", "qwen", client, false, nopLogger{})
	t.Cleanup(func() { p.close(context.Background()) })

	for i := 0; i < 5; i++ {
		p.write(frame(160, 8000))
	}
	for i := 0; i < 10; i++ {
		p.write(frame(160, 5))
	}
	time.Sleep(900 * time.Millisecond)

	if n := client.commitCount(); n != 0 {
		t.Errorf("sent %d commits to an engine that segments server-side", n)
	}
}

// Left is the agent's microphone and right is the customer. Measured on a live
// agent leg; asserted here so a future edit cannot quietly transpose them and
// put every word under the wrong name.
func TestStereoSplitPutsTheAgentOnTheLeft(t *testing.T) {
	s := &session{}
	// Two samples: left = 1000, right = -2000.
	stereo := make([]byte, 8)
	l, r := int16(1000), int16(-2000)
	binary.LittleEndian.PutUint16(stereo[0:], uint16(l))
	binary.LittleEndian.PutUint16(stereo[2:], uint16(r))
	binary.LittleEndian.PutUint16(stereo[4:], uint16(l))
	binary.LittleEndian.PutUint16(stereo[6:], uint16(r))

	s.split(stereo) // no pumps attached; the scratch buffers still fill

	left := int16(binary.LittleEndian.Uint16(s.mono[0]))
	right := int16(binary.LittleEndian.Uint16(s.mono[1]))
	if left != 1000 {
		t.Errorf("left = %d, want 1000", left)
	}
	if right != -2000 {
		t.Errorf("right = %d, want -2000", right)
	}
	if speakerFor(0) != "HUMAN_AGENT" {
		t.Errorf("left maps to %s, want HUMAN_AGENT", speakerFor(0))
	}
	if speakerFor(1) != "CUSTOMER" {
		t.Errorf("right maps to %s, want CUSTOMER", speakerFor(1))
	}
}

//
// Tokens. The stream carries call audio in the clear on a LAN, so the token is
// what stops an arbitrary connection from receiving it.
//

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{
		Addr:        "127.0.0.1:0",
		Secret:      []byte("s3cr3t"),
		NewSession:  func() (transcribe.Session, error) { return newStallSession(), nil },
		Transcripts: stubTranscripts{},
		Logger:      nopLogger{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

type stubTranscripts struct{}

func (stubTranscripts) Lookup(uuid.UUID) (*transcript.Actor, bool) { return nil, false }

func TestTokenRoundTripsAndRefusesTampering(t *testing.T) {
	s := testServer(t)
	claim := Claim{
		CallID: uuid.New(), PartyID: uuid.New(), AgentID: uuid.New(),
		Channel: "chan-1", Expires: time.Now().Add(time.Minute),
	}
	token := s.Token(claim)

	got, err := s.verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.CallID != claim.CallID || got.Channel != claim.Channel || got.AgentID != claim.AgentID {
		t.Errorf("claim round-trip = %+v, want %+v", got, claim)
	}

	if _, err := s.verify(token + "x"); err == nil {
		t.Error("a tampered signature verified")
	}
	if _, err := s.verify("garbage"); err == nil {
		t.Error("a malformed token verified")
	}

	// A different secret must not verify: the token is the whole of the
	// authentication on this listener.
	other := testServer(t)
	other.cfg.Secret = []byte("different")
	if _, err := other.verify(token); err == nil {
		t.Error("a token signed with another key verified")
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	s := testServer(t)
	token := s.Token(Claim{CallID: uuid.New(), Channel: "c", Expires: time.Now().Add(-time.Second)})
	if _, err := s.verify(token); err == nil {
		t.Error("an expired token verified")
	}
}

// One token, one connection: a captured URL is worthless the moment it is used.
func TestATokenCannotBeReplayed(t *testing.T) {
	s := testServer(t)
	token := s.Token(Claim{CallID: uuid.New(), Channel: "c", Expires: time.Now().Add(time.Minute)})
	if !s.claimToken(token) {
		t.Fatal("the first use of a token was refused")
	}
	if s.claimToken(token) {
		t.Error("a token was accepted twice")
	}
}

// The metadata frame confirms an identity; it must never be able to change one.
func TestMetadataMustAgreeWithTheToken(t *testing.T) {
	claim := Claim{CallID: uuid.New(), Channel: "chan-1"}
	if err := checkMetadata([]byte(`{"callId":"`+claim.CallID.String()+`","channelId":"chan-1"}`), claim); err != nil {
		t.Errorf("matching metadata was refused: %v", err)
	}
	if err := checkMetadata([]byte(`{"callId":"`+uuid.New().String()+`"}`), claim); err == nil {
		t.Error("metadata naming a different call was accepted")
	}
	if err := checkMetadata([]byte(`{"channelId":"someone-else"}`), claim); err == nil {
		t.Error("metadata naming a different channel was accepted")
	}
	if err := checkMetadata([]byte(`not json`), claim); err == nil {
		t.Error("undecodable metadata was accepted")
	}
}

func TestNewRefusesAnUnusableConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no address", Config{Secret: []byte("x"), Logger: nopLogger{}}},
		{"no secret", Config{Addr: "127.0.0.1:0", Logger: nopLogger{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("an unusable configuration was accepted")
			}
		})
	}
}
