// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
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

	pumps [2]*pump
	// mono holds the de-interleaved output so the split allocates once per
	// frame size rather than once per frame.
	mono [2][]byte

	sawMetadata bool
	once        sync.Once
}

func (s *session) run() {
	defer s.close(context.Background())

	if err := s.startPumps(); err != nil {
		s.log.Error("transcription could not start", "callId", s.claim.CallID, "error", err)
		s.actor.State("ERROR", "ASR_START_FAILED", nil)
		return
	}
	s.actor.State("LIVE", "", nil)

	_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			s.log.Info("tapped stream ended", "callId", s.claim.CallID, "error", err)
			return
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
			s.sawMetadata = true
			continue
		}
		s.split(data)
	}
}

func (s *session) startPumps() error {
	for i := range s.pumps {
		client, err := s.cfg.NewSession()
		if err != nil {
			return err
		}
		cfg := transcribe.Config{}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = client.Start(ctx, cfg)
		cancel()
		if err != nil {
			return err
		}
		speaker := speakerFor(i)
		s.pumps[i] = newPump(speaker, s.cfg.Profile.Name, client,
			s.cfg.Profile.OwnsEndpointing, s.log)
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
	for c, p := range s.pumps {
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
	for _, p := range s.pumps {
		if p != nil {
			p.close(ctx)
		}
	}
	s.actor.State("STOPPED", "", nil)
	_ = s.conn.Close()
}
