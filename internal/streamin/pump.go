// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
)

// queueDepth bounds one speaker's backlog. Twenty-millisecond frames, so this
// is two seconds of audio: enough to ride out a recogniser's hiccup, short
// enough that what survives is still worth transcribing.
const queueDepth = 100

// silenceRMS is the level below which a frame counts as silence, and
// silenceRun is how much of it ends an utterance. Both matter only where the
// engine refuses to end one itself.
const (
	silenceRMS = 500
	silenceRun = 600 * time.Millisecond
)

// committer is the part of a client that can be told an utterance has ended.
// Only the OpenAI-dialect client implements it, because only that model
// refuses to decide for itself.
type committer interface{ Commit() error }

// pump feeds one speaker's audio to one recognition session.
//
// It never blocks its writer. The switch is on the other end of that write,
// and a recogniser that stalls must cost us audio rather than cost the call
// its media path — the same rule the RTP send queue already follows.
type pump struct {
	speaker  string
	provider string
	session  transcribe.Session
	log      Logger

	frames chan []byte
	done   chan struct{}
	once   sync.Once

	sent    atomic.Int64
	dropped atomic.Int64

	// ownsEndpointing drives the silence detector. Where the engine segments
	// for itself, this stays false and no commit is ever sent.
	ownsEndpointing bool
	quietSince      time.Time
	isSpeaking      bool
}

func newPump(speaker, provider string, s transcribe.Session, ownsEndpointing bool, log Logger) *pump {
	p := &pump{
		speaker:         speaker,
		provider:        provider,
		session:         s,
		log:             log,
		frames:          make(chan []byte, queueDepth),
		done:            make(chan struct{}),
		ownsEndpointing: ownsEndpointing,
	}
	go p.run()
	return p
}

// write hands one frame to the pump, dropping the oldest if the recogniser has
// fallen behind.
//
// Every drop is counted. A discarded frame leaves no other trace, and a
// transcript with a hole in it reads the same whether the engine mis-heard the
// words or we never sent them; the counter is what separates those afterwards.
func (p *pump) write(frame []byte) {
	select {
	case <-p.done:
		return
	default:
	}
	for {
		select {
		case p.frames <- frame:
			return
		default:
		}
		// Full. Discard the oldest and try again — newer audio is worth more
		// than older audio to a live transcript.
		select {
		case <-p.frames:
			p.dropped.Add(1)
		default:
			// Drained by the reader in between; the next send will fit.
		}
	}
}

func (p *pump) run() {
	// The silence check runs on its own tick rather than on frame arrival, so
	// an utterance still ends when the audio stops entirely.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case frame := <-p.frames:
			if err := p.session.SendAudio(frame); err != nil {
				p.log.Warn("transcribe: audio rejected",
					"speaker", p.speaker, "provider", p.provider, "error", err)
				continue
			}
			p.sent.Add(1)
			p.observe(frame)
		case <-ticker.C:
			p.checkSilence()
		}
	}
}

// observe tracks whether this speaker is talking, for the engines that will
// not tell us.
func (p *pump) observe(frame []byte) {
	if !p.ownsEndpointing {
		return
	}
	if meanSquare(frame) >= silenceRMS*silenceRMS {
		p.isSpeaking = true
		p.quietSince = time.Time{}
		return
	}
	if p.isSpeaking && p.quietSince.IsZero() {
		p.quietSince = time.Now()
	}
}

// checkSilence closes an utterance where the engine refuses to. Without it a
// final never arrives at all on that path — six seconds of trailing silence
// produced nothing until a commit was sent.
func (p *pump) checkSilence() {
	if !p.ownsEndpointing || !p.isSpeaking || p.quietSince.IsZero() {
		return
	}
	if time.Since(p.quietSince) < silenceRun {
		return
	}
	p.isSpeaking = false
	p.quietSince = time.Time{}
	if c, ok := p.session.(committer); ok {
		if err := c.Commit(); err != nil {
			p.log.Warn("transcribe: commit failed",
				"speaker", p.speaker, "provider", p.provider, "error", err)
		}
	}
}

// meanSquare is the frame's mean squared amplitude — RMS without the square
// root, because the only question asked of it is a comparison against a
// threshold, and squaring the threshold instead answers it exactly.
// PCM16 little-endian, mono. Runs fifty times a second per speaker.
func meanSquare(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(frame[i*2:])))
		sum += v * v
	}
	return sum / float64(n)
}

// close stops the pump and publishes its frame accounting.
func (p *pump) close(ctx context.Context) {
	p.once.Do(func() {
		close(p.done)
		sent, dropped := p.sent.Load(), p.dropped.Load()
		obs.RecordTranscribeAudio(p.provider, p.speaker, sent, dropped)
		if dropped > 0 {
			// Said out loud as well as counted: a run that lost audio should
			// be visible to whoever reads the log of that call, not only to
			// whoever later scrapes a counter.
			p.log.Warn("transcribe: audio was dropped for this speaker",
				"speaker", p.speaker, "provider", p.provider,
				"sent", sent, "dropped", dropped)
		}
		_ = p.session.Close(ctx)
	})
}

// stats reports what this pump has handled, for tests and for the session's
// own summary.
func (p *pump) stats() (sent, dropped int64) {
	return p.sent.Load(), p.dropped.Load()
}
