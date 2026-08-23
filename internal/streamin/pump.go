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

// frameInterval is how much audio one frame carries, and therefore how long a
// send may take before the pump is falling behind by definition.
const frameInterval = 20 * time.Millisecond

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

	// How the audio was lost, not merely how much. A hole in a transcript
	// reads the same whichever way it happened, and the three ways want
	// opposite fixes: the recogniser stalling for whole seconds at a time, the
	// recogniser being steadily a little too slow for 50 frames a second, or
	// the switch delivering in bursts this queue is too shallow to absorb.
	//
	// sendSlow counts sends that took as long as the frame they carried —
	// past that the pump is losing ground by definition. sendMaxMs is the
	// worst single send. dropRuns counts how many separate times dropping
	// started, so two stalls and two hundred scattered losses stop looking
	// alike.
	sendSlow  atomic.Int64
	sendMaxMs atomic.Int64
	dropRuns  atomic.Int64
	// isDropping belongs to the writer's goroutine alone: write is called from
	// the one goroutine that splits the switch's stream.
	isDropping bool

	// ownsEndpointing drives the silence detector. Where the engine segments
	// for itself, this stays false and no commit is ever sent.
	ownsEndpointing bool

	// speech is guarded because it is written where audio arrives and read
	// where the utterance is ended, and those are deliberately different
	// goroutines.
	speech     sync.Mutex
	quietSince time.Time
	isSpeaking bool
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
	if ownsEndpointing {
		// Only where the engine refuses to end an utterance. Elsewhere there is
		// nothing for this to do and a ticker that never fires anything is just
		// a goroutine to explain later.
		go p.watchSilence()
	}
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
	// Observed here rather than after the send, because the boundary belongs to
	// the audio and not to the recogniser's throughput. A session that is
	// rejecting or merely slow would otherwise stop the level tracking dead,
	// and on an engine that will not end an utterance itself that means no
	// commit is ever sent and no final ever arrives.
	p.observe(frame)

	// A run of dropping ends when a frame goes in without having to evict one,
	// not merely when the retry after an eviction fits: the retry always fits,
	// so counting that as recovery would make one stall look like a hundred
	// separate ones.
	for first := true; ; first = false {
		select {
		case p.frames <- frame:
			if first {
				p.isDropping = false
			}
			return
		default:
		}
		// Full. Discard the oldest and try again — newer audio is worth more
		// than older audio to a live transcript.
		select {
		case <-p.frames:
			p.dropped.Add(1)
			if !p.isDropping {
				p.isDropping = true
				p.dropRuns.Add(1)
			}
		default:
			// Drained by the reader in between; the next send will fit.
		}
	}
}

func (p *pump) run() {
	for {
		select {
		case <-p.done:
			return
		case frame := <-p.frames:
			startedAt := time.Now()
			err := p.session.SendAudio(frame)
			// Timed on the way out rather than sampled: what this is for is the
			// rare send, and an average would hide exactly the stall that
			// empties two seconds of queue in one go.
			p.recordSend(time.Since(startedAt))
			if err != nil {
				p.log.Warn("transcribe: audio rejected",
					"speaker", p.speaker, "provider", p.provider, "error", err)
				continue
			}
			p.sent.Add(1)
		}
	}
}

// recordSend accounts for how long the recogniser took to accept one frame.
// Called from the send loop alone, so the max is a plain load and store.
func (p *pump) recordSend(took time.Duration) {
	if took >= frameInterval {
		p.sendSlow.Add(1)
	}
	if ms := took.Milliseconds(); ms > p.sendMaxMs.Load() {
		p.sendMaxMs.Store(ms)
	}
}

// watchSilence ends an utterance on its own goroutine.
//
// Not in the send loop's select: SendAudio blocks for as long as its write
// deadline allows, and a check sharing that select would be delayed by exactly
// the recogniser trouble it most needs to survive.
func (p *pump) watchSilence() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
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
	loud := meanSquare(frame) >= silenceRMS*silenceRMS

	p.speech.Lock()
	defer p.speech.Unlock()
	if loud {
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
	if !p.ownsEndpointing {
		return
	}
	p.speech.Lock()
	quiet := p.isSpeaking && !p.quietSince.IsZero() && time.Since(p.quietSince) >= silenceRun
	if quiet {
		p.isSpeaking = false
		p.quietSince = time.Time{}
	}
	p.speech.Unlock()
	if !quiet {
		return
	}
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
				"sent", sent, "dropped", dropped,
				// Which of the three shapes this was. A couple of runs with a
				// large maxSendMs is the recogniser stalling; drops spread
				// over many runs with no slow sends is the switch arriving in
				// bursts; slow sends in proportion to the audio is a
				// recogniser that is simply not keeping up.
				"dropRuns", p.dropRuns.Load(),
				"slowSends", p.sendSlow.Load(),
				"maxSendMs", p.sendMaxMs.Load())
		}
		_ = p.session.Close(ctx)
	})
}

// stats reports what this pump has handled, for tests and for the session's
// own summary.
func (p *pump) stats() (sent, dropped int64) {
	return p.sent.Load(), p.dropped.Load()
}
