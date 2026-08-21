// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

// readTimeout fails a stream that has gone quiet at the socket level. The
// switch sends audio continuously while a call is up, so silence here means a
// half-open connection rather than a quiet caller.
const readTimeout = 30 * time.Second

// session is one tapped leg: one websocket in, two recognition sessions out.
type session struct {
	claim Claim
	conn  *websocket.Conn
	actor *transcript.Actor
	log   Logger
	cfg   Config

	// mu guards pumps. The session's own goroutine builds them and feeds
	// them, but Server.Stop closes sessions from whatever goroutine is
	// shutting the server down — including one still starting up, where the
	// teardown would read the array the startup was midway through writing.
	mu    sync.Mutex
	pumps [2]*pump
	// mono holds the de-interleaved output so the split allocates once per
	// frame size rather than once per frame.
	mono [2][]byte

	once sync.Once
}

// livePumps is the current set, copied under the lock so callers can work with
// it without holding one. Two pointers; the copy is free next to a frame.
func (s *session) livePumps() [2]*pump {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pumps
}

// run reads the tapped stream until it ends, and reports whether it ended
// gracefully — a normal websocket closure, which is what the module sends when
// it stops the stream itself because the channel went away.
func (s *session) run() (graceful bool) {
	defer s.close(context.Background())

	if err := s.startPumps(); err != nil {
		s.log.Error("transcription could not start", "callId", s.claim.CallID, "error", err)
		// A recogniser that accepted the connection and never started its task
		// is its own failure, and it is the one that used to be invisible: the
		// socket is open, nothing errors, and every frame is discarded. It gets
		// its own code so the panel is not told the generic thing.
		reason := "ASR_START_FAILED"
		if errors.Is(err, transcribe.ErrTaskNeverStarted) {
			reason = "ASR_NEVER_STARTED"
		}
		s.actor.State("ERROR", reason, nil)
		return
	}
	s.actor.State("LIVE", "", nil)
	s.log.Info("transcription live",
		"callId", s.claim.CallID, "channelId", s.claim.Channel)

	_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			s.log.Info("tapped stream ended", "callId", s.claim.CallID, "error", err)
			return websocket.IsCloseError(err, websocket.CloseNormalClosure)
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))

		if mt == websocket.TextMessage {
			// The module sends its metadata first. It confirms the identity the
			// token already established; it never grants one.
			if err := checkMetadata(data, s.claim); err != nil {
				s.log.Warn("tapped stream metadata disagrees with its token",
					"callId", s.claim.CallID, "error", err)
				return
			}
			continue
		}
		s.split(data)
	}
}

// startSessionTimeout bounds one recogniser handshake from this side. It sits
// above the client's own start timeout on purpose, so a task that never starts
// is reported as that rather than as a context deadline — the specific failure
// is the one worth telling the agent about.
const startSessionTimeout = 25 * time.Second

// startPumps opens both recognisers, concurrently.
//
// Concurrently because Start now blocks until the service says the task
// exists, and that handshake was measured at ~5s against the Beijing MaaS host
// (2026-08-18). Sequentially that is ten seconds of a live conversation before
// the first frame is recognised; in parallel it is one handshake for both
// speakers. Neither is free, and the alternative — not waiting — is what threw
// away five seconds of audio per call and reported LIVE while doing it.
func (s *session) startPumps() error {
	clients := make([]transcribe.Session, len(s.pumps))
	errs := make([]error, len(s.pumps))

	var wg sync.WaitGroup
	for i := range s.pumps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := s.cfg.NewSession()
			if err != nil {
				errs[i] = err
				return
			}
			clients[i] = client
			// The call's language, not the deployment's: a bilingual queue
			// answers in whichever language the number was dialled in.
			ctx, cancel := context.WithTimeout(context.Background(), startSessionTimeout)
			defer cancel()
			errs[i] = client.Start(ctx, transcribe.Config{Language: s.claim.Language})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			continue
		}
		// One recogniser is no better than none for a two-sided conversation,
		// so whichever did start is closed rather than left running.
		for _, c := range clients {
			if c != nil {
				closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = c.Close(closeCtx)
				cancel()
			}
		}
		return fmt.Errorf("%s: %w", speakerFor(i), err)
	}

	for i, client := range clients {
		speaker := speakerFor(i)
		p := newPump(speaker, s.cfg.Profile.Name, client,
			s.cfg.Profile.OwnsEndpointing, s.log)
		s.mu.Lock()
		s.pumps[i] = p
		s.mu.Unlock()
		go s.consume(speaker, client)
	}
	return nil
}

// split de-interleaves one stereo frame into two mono frames.
//
// Qwen accepts mono only, so a split is required regardless; doing it here
// costs one pass over the buffer and saves a second media bug on the switch.
func (s *session) split(frame []byte) {
	samples := len(frame) / 4 // two channels, two bytes each
	if samples == 0 {
		return
	}
	for c := range s.mono {
		if cap(s.mono[c]) < samples*2 {
			s.mono[c] = make([]byte, samples*2)
		}
		s.mono[c] = s.mono[c][:samples*2]
	}
	for i := 0; i < samples; i++ {
		src := i * 4
		s.mono[0][i*2] = frame[src]
		s.mono[0][i*2+1] = frame[src+1]
		s.mono[1][i*2] = frame[src+2]
		s.mono[1][i*2+1] = frame[src+3]
	}
	// The pump owns its frame once handed over, so each gets its own copy —
	// the scratch buffer is reused on the very next frame.
	for c, p := range s.livePumps() {
		if p == nil {
			continue
		}
		out := make([]byte, len(s.mono[c]))
		copy(out, s.mono[c])
		p.write(out)
	}
}

// consume turns one speaker's recognition events into transcript lines.
func (s *session) consume(speaker string, client transcribe.Session) {
	for ev := range client.Events() {
		switch ev.Type {
		case transcribe.EventPartial, transcribe.EventFinal:
			line := transcript.Line{
				Speaker:     speaker,
				Kind:        store.TranscriptKindText,
				Text:        ev.Text,
				Language:    ev.Language,
				Source:      store.TranscriptSourceASR,
				Provider:    s.cfg.Profile.Name,
				UtteranceID: speaker + ":" + ev.UtteranceID,
				IsFinal:     ev.Type == transcribe.EventFinal,
			}
			// Only the agent's own lines carry an agent id. The customer's
			// come from the same leg but are not that agent's words, and a
			// wrong attribution is worse than none.
			if speaker == store.SpeakerHumanAgent && s.claim.AgentID != uuid.Nil {
				agentID := s.claim.AgentID
				line.AgentID = &agentID
			}
			if s.claim.PartyID != uuid.Nil {
				partyID := s.claim.PartyID
				line.PartyID = &partyID
			}
			s.actor.Post(line)

		case transcribe.EventError:
			s.log.Warn("recognition failed", "callId", s.claim.CallID,
				"speaker", speaker, "error", ev.Err)
			// One side failing is not both: say which, so the panel can be
			// honest rather than merely quiet.
			s.actor.State("DEGRADED", "ASR_SESSION_FAILED", []string{speaker})
		}
	}
}

func (s *session) close(ctx context.Context) {
	s.once.Do(func() { s.closeLocked(ctx) })
}

func (s *session) closeLocked(ctx context.Context) {
	for _, p := range s.livePumps() {
		if p != nil {
			p.close(ctx)
		}
	}
	s.actor.State("STOPPED", "", nil)
	_ = s.conn.Close()
}
