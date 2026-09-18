// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"encoding/base64"
	"sync"
	"time"
)

// The uplink cadence.
//
// This provider reads the upstream stream as a clock — "strictly at real time;
// too fast OR too slow is an error" — and as a keepalive: stop feeding it
// without saying so and the model stops answering altogether. Neither is true
// of the other protocol in this repository, where audio is written straight
// through from whatever drives the telephone leg.
//
// So the cadence is this client's own. Caller audio arrives from the RTP jitter
// buffer in whatever rhythm the network left it in; the pacer turns that back
// into one frame every 20 ms, drops what it cannot place rather than catching
// up in a burst, and declares a hold when the caller's side goes quiet.
const (
	// queueDepth is how much jitter is absorbed: three frames, 60 ms. Deeper
	// would be latency the caller hears, and the frames past it are ones the
	// conversation has already moved beyond.
	queueDepth = 3
	// muteAfterEmptyTicks is the silence that counts as the microphone being
	// off — half a second of nothing arriving from the leg.
	muteAfterEmptyTicks = 25
)

// Stats is what the uplink did, for the line logged when a session ends. A
// call with drops in it sounded clipped to the model, and a call with mutes in
// it had gaps on the telephone leg.
type Stats struct {
	FramesSent    int64
	FramesDropped int64
	Mutes         int64
	Unmutes       int64
}

// pacer is the one goroutine that writes caller audio.
//
// SendAudio hands it a frame and returns; nothing on the call path ever blocks
// on this socket, because the thing feeding it is the media path and a stalled
// write there is audio lost in both directions.
type pacer struct {
	session *Session
	ticks   <-chan time.Time
	stopped chan struct{}

	mu         sync.Mutex
	queue      [][]byte
	isMuted    bool
	emptyTicks int
	writeErr   error
	stats      Stats
}

func newPacer(session *Session, ticks <-chan time.Time) *pacer {
	return &pacer{
		session: session,
		ticks:   ticks,
		stopped: make(chan struct{}),
		queue:   make([][]byte, 0, queueDepth),
	}
}

// push takes one frame of caller audio. A full queue drops its oldest: the
// caller has already moved past it, and the alternative — blocking — would
// stall the media path that produced it.
//
// The queue is shifted in place rather than resliced: this runs fifty times a
// second for the length of every call, and a slice that walks forward through
// its own array reallocates for ever.
func (p *pacer) push(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queue) == queueDepth {
		copy(p.queue, p.queue[1:])
		p.queue[queueDepth-1] = frame
		p.stats.FramesDropped++
		return
	}
	p.queue = append(p.queue, frame)
}

// run paces the uplink until the session begins stopping. Queued frames are
// discarded rather than flushed: they are audio from a call that has ended.
func (p *pacer) run() {
	defer close(p.stopped)
	if p.session.stopTicker != nil {
		defer p.session.stopTicker()
	}

	for {
		select {
		case <-p.session.stopping:
			p.discard()
			return
		case <-p.ticks:
			p.tick()
			// The tests drive the clock themselves and need to know when a tick
			// has been dealt with; there is no such channel on a real call.
			if p.session.paced != nil {
				select {
				case p.session.paced <- struct{}{}:
				case <-p.session.stopping:
					return
				}
			}
		}
	}
}

// tick writes at most one frame. A tick the pacer was late for is a tick
// missed, never two frames: this provider reads a burst as a pacing error, and
// the caller would hear the conversation drift further behind with every one.
func (p *pacer) tick() {
	p.mu.Lock()
	if len(p.queue) == 0 {
		p.emptyTicks++
		isMuteDue := !p.isMuted && p.emptyTicks >= muteAfterEmptyTicks
		p.mu.Unlock()
		if isMuteDue {
			p.muteNow()
		}
		return
	}

	frame := p.queue[0]
	copy(p.queue, p.queue[1:])
	p.queue = p.queue[:len(p.queue)-1]
	p.emptyTicks = 0
	wasMuted := p.isMuted
	p.isMuted = false
	if wasMuted {
		p.stats.Unmutes++
	}
	p.stats.FramesSent++
	p.mu.Unlock()

	if wasMuted {
		p.write(p.session.simpleFrame("input_audio_unmute.commit"))
	}
	p.write(appendFrame(frame))
}

// muteNow declares the hold and forgets what was queued. Whatever is in the
// queue at this point is audio from before the silence; sending it on resume
// would play the caller a moment of their own past.
func (p *pacer) muteNow() {
	p.mu.Lock()
	if p.isMuted {
		p.mu.Unlock()
		return
	}
	p.isMuted = true
	p.queue = p.queue[:0]
	p.stats.Mutes++
	p.mu.Unlock()

	p.write(p.session.simpleFrame("input_audio_mute.commit"))
}

// write records a failure rather than reporting it: the next SendAudio returns
// it, and the read loop reports the connection itself. Reporting from here as
// well would fail the call twice for one broken socket.
func (p *pacer) write(data []byte) {
	if err := p.session.sendFrame(data); err != nil {
		p.mu.Lock()
		p.writeErr = err
		p.mu.Unlock()
	}
}

func (p *pacer) discard() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = p.queue[:0]
}

// err is the last write failure, if there was one.
func (p *pacer) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writeErr
}

func (p *pacer) statistics() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// appendFrame is one frame of caller audio.
//
// The JSON is assembled directly around the base64 rather than marshalled from
// a struct: this runs fifty times a second per call, and marshalling would copy
// every frame twice more for no benefit. Base64's alphabet needs no JSON
// escaping, so this is safe as well as cheap.
func appendFrame(audio []byte) []byte {
	const prefix = `{"type":"input_audio_buffer.append","audio":"`

	message := make([]byte, 0, len(prefix)+base64.StdEncoding.EncodedLen(len(audio))+2)
	message = append(message, prefix...)
	message = base64.StdEncoding.AppendEncode(message, audio)
	return append(message, '"', '}')
}
