// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

// The assembly, in one process.
//
// Every layer of this feature is unit-tested on its own, and every failure it
// has actually produced was one the pieces could not see: a module that loads
// and does not resample, a reload that reports success and runs the old image,
// an attach that returns +OK to a port with nothing listening, and — the one
// this file exists for — two producers that could each have found their own
// transcript actor and numbered from 1, colliding on nothing and erroring
// nowhere.
//
// That last failure was proven absent by two calls placed by hand. This makes
// it a test, because the next silent break will not have somebody standing
// next to a phone.

// fakeASR is a DashScope-speaking server. Each connection answers with one
// final whose text is fixed per connection, so the two speakers are
// distinguishable at the other end.
type fakeASR struct {
	srv *http.Server
	ln  interface{ Close() error }

	mu       sync.Mutex
	connects int
	texts    []string
}

func startFakeASR(t *testing.T, texts []string) (*fakeASR, string) {
	t.Helper()
	f := &fakeASR{texts: texts}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		f.mu.Lock()
		which := f.connects
		f.connects++
		f.mu.Unlock()

		// run-task, then audio. Answer the handshake, then wait until enough
		// audio has arrived to be worth a sentence.
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})

		frames := 0
		for {
			mt, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			frames++
			if frames == 3 && which < len(texts) {
				_ = c.WriteJSON(dsFinal(1, texts[which]))
			}
		}
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake asr listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f, "ws://" + ln.Addr().String() + "/"
}

// dsFinal is one DashScope result-generated envelope carrying a settled
// sentence. Built here rather than imported: the protocol is what this test
// stands in for, so writing it out is the point.
func dsFinal(sentenceID int, text string) map[string]any {
	return map[string]any{
		"header": map[string]any{"event": "result-generated"},
		"payload": map[string]any{"output": map[string]any{"sentence": map[string]any{
			"begin_time": 0, "end_time": 1000, "text": text,
			"sentence_begin": false, "sentence_end": true, "sentence_id": sentenceID,
		}}},
	}
}

// stereoFrame interleaves one 20 ms frame: left is the agent, right the customer.
func stereoFrame(samplesPerChannel int, left, right int16) []byte {
	b := make([]byte, samplesPerChannel*4)
	for i := 0; i < samplesPerChannel; i++ {
		binary.LittleEndian.PutUint16(b[i*4:], uint16(left))
		binary.LittleEndian.PutUint16(b[i*4+2:], uint16(right))
	}
	return b
}

type capturingStore struct {
	mu    sync.Mutex
	lines []store.TranscriptLine
}

func (c *capturingStore) InsertTranscriptLine(_ context.Context, _ uuid.UUID, l store.TranscriptLine) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
	return nil
}

func (c *capturingStore) all() []store.TranscriptLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.TranscriptLine(nil), c.lines...)
}

type registryLookup struct{ reg *transcript.Registry }

func (r registryLookup) Lookup(id uuid.UUID) (*transcript.Actor, bool) { return r.reg.Lookup(id) }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// One conversation, two producers, one dense sequence.
func TestBothProducersShareOneActorAndOneSequence(t *testing.T) {
	asr, asrURL := startFakeASR(t, []string{"I can see the order", "hello can you hear me"})
	_ = asr

	rows := &capturingStore{}
	hub := events.NewHub(events.NewSequence(&blockReserver{}, "events"))
	reg := transcript.NewRegistry(rows, hub, discardLog())

	callID := uuid.New()
	actor := reg.For(callID, "INBOUND", time.Now().Add(-time.Minute))
	t.Cleanup(func() { reg.Close(callID) })

	profile := transcribe.Profile{Name: "qwen", Endpoint: asrURL, Model: "m", SampleRate: 16000}
	srv, err := New(Config{
		Addr:   "127.0.0.1:0",
		Secret: []byte("e2e"),
		NewSession: func() (transcribe.Session, error) {
			return transcribe.New(profile, "key", discardLog())
		},
		Profile:     profile,
		Transcripts: registryLookup{reg: reg},
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	// --- the bot phase, exactly as aicall posts it ------------------------
	for _, line := range []struct{ speaker, text string }{
		{store.SpeakerBot, "Thanks for calling NovaNet"},
		{store.SpeakerCustomer, "please transfer me"},
	} {
		actor.Post(transcript.Line{
			Speaker: line.speaker, Kind: store.TranscriptKindText, Text: line.text,
			Source: store.TranscriptSourceModel, IsFinal: true,
		})
	}
	waitUntil(t, func() bool { return len(rows.all()) == 2 }, "the bot phase to be written")

	// --- the transfer: the switch taps the agent's leg --------------------
	claim := Claim{CallID: callID, AgentID: uuid.New(), PartyID: uuid.New(),
		Channel: "agent-chan", Language: "en", Expires: time.Now().Add(time.Minute)}
	dial := "ws://" + srv.Addr() + StreamPath + "?t=" + url.QueryEscape(srv.Token(claim))

	conn, _, err := websocket.DefaultDialer.Dial(dial, nil)
	if err != nil {
		t.Fatalf("the tap could not connect: %v", err)
	}
	defer conn.Close()

	meta, _ := json.Marshal(map[string]string{"callId": callID.String(), "channelId": "agent-chan"})
	_ = conn.WriteMessage(websocket.TextMessage, meta)
	for i := 0; i < 6; i++ {
		if err := conn.WriteMessage(websocket.BinaryMessage, stereoFrame(320, 6000, 4000)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}

	waitUntil(t, func() bool { return len(rows.all()) == 4 }, "both ASR lines to be written")

	// --- the assertions ---------------------------------------------------
	lines := rows.all()
	for i, l := range lines {
		if l.Seq != i+1 {
			t.Fatalf("seq went %v — two producers numbered independently, which renders as a "+
				"perfectly plausible transcript", seqsOf(lines))
		}
	}
	if lines[0].Source != store.TranscriptSourceModel || lines[1].Source != store.TranscriptSourceModel {
		t.Errorf("the bot phase is not MODEL-sourced: %v", sourcesOf(lines))
	}
	if lines[2].Source != store.TranscriptSourceASR || lines[3].Source != store.TranscriptSourceASR {
		t.Errorf("the agent phase is not ASR-sourced: %v", sourcesOf(lines))
	}

	// The same person is CUSTOMER on both sides of the seam.
	if lines[1].Speaker != store.SpeakerCustomer {
		t.Errorf("bot-phase caller = %s, want CUSTOMER", lines[1].Speaker)
	}
	speakers := map[string]bool{lines[2].Speaker: true, lines[3].Speaker: true}
	if !speakers[store.SpeakerHumanAgent] || !speakers[store.SpeakerCustomer] {
		t.Errorf("agent phase speakers = %v, want both HUMAN_AGENT and CUSTOMER", speakers)
	}

	// Only the agent's own lines carry an agent id: a wrong attribution is
	// worse than none.
	for _, l := range lines {
		if l.Speaker == store.SpeakerCustomer && l.AgentID != nil {
			t.Errorf("a CUSTOMER line carries an agent id (%s)", *l.AgentID)
		}
	}
}

func seqsOf(lines []store.TranscriptLine) []int {
	out := make([]int, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Seq)
	}
	return out
}

func sourcesOf(lines []store.TranscriptLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Source)
	}
	return out
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// blockReserver hands out sequence blocks without a database.
type blockReserver struct {
	mu sync.Mutex
	n  int64
}

func (b *blockReserver) ReserveSeqBlock(_ context.Context, _ string, size int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n += size
	return b.n, nil
}
