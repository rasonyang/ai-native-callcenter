// SPDX-License-Identifier: Apache-2.0

// Package pacer is the cadence a provider client writes caller audio at.
//
// Audio reaches a client from the RTP jitter buffer in whatever rhythm the
// network left it in, and what a provider socket wants is neither that rhythm
// nor a blocking write. The thing feeding the uplink is the media path: a
// stalled write there is audio lost in both directions, so the call path hands
// a frame over and returns, and one goroutine decides when each frame goes out
// and which ones do not go out at all.
//
// It carries frames and knows nothing about what is in them: no protocol event,
// no encoding of one, no vendor. A client hands over a function that turns a
// frame into bytes and a function that writes them, and keeps for itself every
// decision about what the frames say — including what, if anything, is written
// when the caller's side falls silent, which on one protocol is a frame the
// engine insists on and on another is nothing at all.
//
// What a client tunes is how deep the queue is and how many frames leave on one
// tick. An engine that reads its uplink as a clock takes one frame per tick and
// calls a burst an error; an engine that takes audio as fast as it is given
// wants a deep queue drained whole after a write stall, which is
// DrainEverything. Both are the same loop.
//
// Goroutines: one. Run is it, started and owned by the client that built the
// pacer, and it ends when the channel that client passed as Stopping is closed.
// Whatever is queued then is discarded rather than flushed — it is audio from a
// call that has ended.
package pacer

import (
	"sync"
	"time"
)

// DrainEverything is a MaxPerTick that writes the whole queue on one tick.
//
// It is for a client whose engine takes audio faster than real time, where the
// queue absorbs a write that stalled rather than pacing anything, and where
// holding the backlog back a frame per tick would only make the stall longer.
const DrainEverything = 0

// Stats is what the uplink did, for the line a client logs when a session ends.
// A call with drops in it sounded clipped to the engine.
//
// Anything a client writes on its own account — a hold, a resume — is that
// client's to count, because only it knows whether such a frame exists.
type Stats struct {
	FramesSent    int64
	FramesDropped int64
}

// Config is everything one uplink needs. Every function in it is called from
// the Run goroutine and from nothing else.
type Config struct {
	// Ticks is the cadence. A ticker rather than a sleeping loop is what makes
	// the period absolute, and a tick the pacer was late for is a tick missed
	// rather than a frame queued.
	Ticks <-chan time.Time
	// Stopping closes when the session has begun ending. It is the only thing
	// that stops Run.
	Stopping <-chan struct{}
	// Write puts one encoded frame on the wire. It is expected to refuse once
	// the session is stopping, and a failure is recorded rather than reported —
	// see WriteError.
	Write func([]byte) error
	// Encode turns one queued frame of caller audio into the bytes that go out.
	// This is where a protocol lives; nothing else here knows one.
	Encode func(frame []byte) []byte
	// Depth is how many frames may wait. It is latency the caller hears, so a
	// client chooses it for its own engine; the frames past it are ones the
	// conversation has already moved beyond.
	Depth int
	// MaxPerTick is how many frames one tick writes. DrainEverything, or any
	// value below it, writes the whole queue.
	MaxPerTick int
	// IdleTicks is how many consecutive empty ticks count as the caller's side
	// having gone quiet. Zero means a client that has nothing to say about it.
	IdleTicks int
	// OnIdle is called once the quiet has lasted IdleTicks, with the queue
	// already emptied and no lock held. What was queued when the quiet began is
	// audio from before it, and writing it on resume would play the caller a
	// moment of their own past.
	OnIdle func()
	// OnResume is called before the first frame written after a quiet, with no
	// lock held: whatever announces the resume has to reach the engine ahead of
	// the audio it announces.
	OnResume func()
	// StopTicker stops whatever produces Ticks, once Run has ended. It may be
	// nil, and is when the ticks are a test's own.
	StopTicker func()
	// Ticked is how a test drives the clock and knows when a tick has been dealt
	// with, which is what makes an assertion exact rather than eventual. There
	// is no such channel on a real call.
	Ticked chan<- struct{}
}

// Pacer is one uplink.
type Pacer struct {
	cfg     Config
	stopped chan struct{}

	mu         sync.Mutex
	queue      [][]byte
	isIdle     bool
	emptyTicks int
	writeErr   error
	stats      Stats

	// batch is the frames one tick is writing, held across ticks so that a
	// cadence running fifty times a second for the length of every call
	// allocates nothing. Only the Run goroutine reads it.
	batch [][]byte
}

// New builds an uplink. Nothing is started; Run is the client's to start and to
// own.
func New(cfg Config) *Pacer {
	if cfg.Depth < 1 {
		cfg.Depth = 1
	}
	return &Pacer{
		cfg:     cfg,
		stopped: make(chan struct{}),
		queue:   make([][]byte, 0, cfg.Depth),
	}
}

// Push takes one frame of caller audio. A full queue drops its oldest: the
// caller has already moved past it, and the alternative — blocking — would
// stall the media path that produced it.
//
// The frame is not copied. Whoever pushes owns that decision, because only they
// know whether the buffer is theirs.
//
// The queue is shifted in place rather than resliced: this runs fifty times a
// second for the length of every call, and a slice that walks forward through
// its own array reallocates for ever.
func (p *Pacer) Push(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queue) == p.cfg.Depth {
		copy(p.queue, p.queue[1:])
		p.queue[p.cfg.Depth-1] = frame
		p.stats.FramesDropped++
		return
	}
	p.queue = append(p.queue, frame)
}

// Run paces the uplink until the session begins stopping. Queued frames are
// discarded rather than flushed: they are audio from a call that has ended.
func (p *Pacer) Run() {
	defer close(p.stopped)
	if p.cfg.StopTicker != nil {
		defer p.cfg.StopTicker()
	}

	for {
		select {
		case <-p.cfg.Stopping:
			p.discard()
			return
		case <-p.cfg.Ticks:
			p.tick()
			if p.cfg.Ticked != nil {
				select {
				case p.cfg.Ticked <- struct{}{}:
				case <-p.cfg.Stopping:
					return
				}
			}
		}
	}
}

// Stopped closes once Run has ended. Waiting on it is a handshake rather than a
// wait: Run exits on the same channel that told it to.
func (p *Pacer) Stopped() <-chan struct{} { return p.stopped }

// Idle declares the quiet now rather than waiting for IdleTicks of it, and is
// what OnIdle is called from either way. Asking twice is nothing: the first
// answer stands until a frame resumes the uplink.
func (p *Pacer) Idle() {
	p.mu.Lock()
	if p.isIdle {
		p.mu.Unlock()
		return
	}
	p.isIdle = true
	p.queue = p.queue[:0]
	p.mu.Unlock()

	if p.cfg.OnIdle != nil {
		p.cfg.OnIdle()
	}
}

// Write puts one frame on the wire that no queue paced, for the frames a client
// writes on its own account from OnIdle or OnResume. A failure is captured the
// same way a paced frame's is, so one broken socket is reported once.
func (p *Pacer) Write(data []byte) {
	if err := p.cfg.Write(data); err != nil {
		p.mu.Lock()
		p.writeErr = err
		p.mu.Unlock()
	}
}

// WriteError is the last write failure, if there was one. A client reports it to
// whoever offers the next frame; the read loop reports the connection itself, so
// reporting from inside the pacer would fail the call twice for one broken
// socket.
func (p *Pacer) WriteError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writeErr
}

// Stats is what the uplink has done so far.
func (p *Pacer) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// tick writes what this tick is due, and nothing more. A tick the pacer was
// late for is a tick missed rather than two frames at once, unless the client
// asked for the whole queue: an engine that reads its uplink as a clock hears a
// burst as the conversation drifting further behind with every one.
func (p *Pacer) tick() {
	p.mu.Lock()
	if len(p.queue) == 0 {
		p.emptyTicks++
		isIdleDue := !p.isIdle && p.cfg.IdleTicks > 0 && p.emptyTicks >= p.cfg.IdleTicks
		p.mu.Unlock()
		if isIdleDue {
			p.Idle()
		}
		return
	}

	count := p.cfg.MaxPerTick
	if count <= DrainEverything || count > len(p.queue) {
		count = len(p.queue)
	}
	p.batch = append(p.batch[:0], p.queue[:count]...)
	copy(p.queue, p.queue[count:])
	p.queue = p.queue[:len(p.queue)-count]
	p.emptyTicks = 0
	wasIdle := p.isIdle
	p.isIdle = false
	p.stats.FramesSent += int64(count)
	p.mu.Unlock()

	if wasIdle && p.cfg.OnResume != nil {
		p.cfg.OnResume()
	}
	for _, frame := range p.batch {
		p.Write(p.cfg.Encode(frame))
	}
}

func (p *Pacer) discard() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = p.queue[:0]
}
