// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/base64"
	"sync/atomic"
	"time"

	uplink "github.com/rasonyang/ai-native-callcenter/internal/provider/pacer"
)

// The uplink cadence.
//
// This provider is the opposite of the one that reads its uplink as a clock. It
// takes audio as fast as it is given — a burst and a paced stream were measured
// transcribing identically — and its sockets stall: a single write took between
// two and 3.6 seconds, repeatedly, from about twenty seconds into a session.
//
// So the queue here is deep and it is drained whole. Five seconds of it, because
// that is longer than any stall measured, and every frame of it goes out on the
// first tick after the socket comes back. Dropping the caller's words to keep
// the queue shallow would buy nothing: there is no cadence to preserve, and what
// the model would be left with is a sentence with a hole in it.
const (
	// queueDepth is five seconds of caller audio at one frame every 20 ms.
	queueDepth = 250
	// idleAfterEmptyTicks is the second of silence that counts as the caller's
	// side having stopped, which the server is told about so it stops holding
	// audio it will never be given the end of.
	idleAfterEmptyTicks = 50

	// slowWriteThreshold is the write that is worth a line while the call is
	// still happening. One frame is 20 ms of audio, so half a second on the
	// socket is twenty-five frames arriving behind it: past this the caller is
	// being heard late, and a stall that is only counted at the end of the call
	// is a stall nobody could act on.
	slowWriteThreshold = 500 * time.Millisecond
	// slowWriteWarnEvery is how often the slow-write line may be said. A stall
	// is dozens of slow writes in a row and each of them says the same thing;
	// what matters is that the log shows it while it lasts, not that it shows it
	// fifty times.
	slowWriteWarnEvery = time.Second
)

// Stats is what the uplink did, for the line logged when a session ends. A call
// with drops in it had five seconds of the caller's words go missing, which is
// the socket having stalled for longer than anything measured.
//
// The three write figures are the same trouble seen from the other end: how
// often a write was slow, the worst single one, and the deepest the queue got
// behind it. A call whose transcripts read as if the model heard the caller
// seconds late has them; a healthy call has zeroes.
type Stats struct {
	FramesSent    int64
	FramesDropped int64
	StreamEnds    int64

	SlowWrites       int64
	MaxWriteMs       int64
	MaxBacklogFrames int64
}

// pacer is the one goroutine that writes caller audio.
//
// SendAudio hands it a frame and returns; nothing on the call path ever blocks
// on this socket, because the thing feeding it is the media path and a stalled
// write there is audio lost in both directions.
//
// The cadence itself is shared. What is here is this protocol's half of it: the
// frame a piece of audio is wrapped in, and the one frame this client writes on
// its own account.
type pacer struct {
	session *Session
	uplink  *uplink.Pacer
	// prefix is everything in an audio frame up to the base64, built once
	// because the rate in it is the session's and does not change.
	prefix []byte

	// send is the socket write this cadence times, and now is the clock it
	// times it with. They are fields so that a test can make a write take four
	// seconds without waiting four seconds; a call leaves both at their real
	// values.
	send func([]byte) error
	now  func() time.Time

	streamEnds atomic.Int64

	// pushed and encoded are the two ends of the queue, each written by one
	// goroutine and only ever growing. What is waiting is the difference,
	// less what the queue dropped — see backlog, which is the one place that
	// arithmetic is done.
	pushed  atomic.Int64
	encoded atomic.Int64

	slowWrites atomic.Int64
	maxWriteMs atomic.Int64
	maxBacklog atomic.Int64

	// The reporting state, touched only on the pacer's own goroutine.
	//
	// droppedAccounted is the drop total the log has already reported, so that
	// each blocked write names the frames lost behind it and not the running
	// total. lastSlowWarnAt rate-limits the slow-write line.
	droppedAccounted int64
	lastSlowWarnAt   time.Time
}

func newPacer(session *Session, ticks <-chan time.Time, mimeType string) *pacer {
	p := &pacer{
		session: session,
		prefix:  []byte(`{"realtimeInput":{"audio":{"mimeType":"` + mimeType + `","data":"`),
		send:    session.sendFrame,
		now:     time.Now,
	}
	if session.uplinkWrite != nil {
		p.send = session.uplinkWrite
	}
	if session.clock != nil {
		p.now = session.clock
	}
	p.uplink = uplink.New(uplink.Config{
		Ticks:      ticks,
		Stopping:   session.stopping,
		Write:      p.write,
		Encode:     p.appendFrame,
		Depth:      queueDepth,
		MaxPerTick: uplink.DrainEverything,
		IdleTicks:  idleAfterEmptyTicks,
		OnIdle:     p.onStreamEnd,
		StopTicker: session.stopTicker,
		Ticked:     session.paced,
	})
	return p
}

// push takes one frame of caller audio, and drops the oldest if the queue is
// already full.
func (p *pacer) push(frame []byte) {
	p.pushed.Add(1)
	p.uplink.Push(frame)
}

// write puts one frame on the socket and times it.
//
// This is the whole fast path: two readings of the clock and a comparison. A
// write that took no time at all is the normal case and costs nothing more
// than that — everything that reports a stall is behind the branch, where a
// mutex and a log line are affordable because the call is already in trouble.
func (p *pacer) write(data []byte) error {
	started := p.now()
	err := p.send(data)
	elapsed := p.now().Sub(started)

	if elapsed >= slowWriteThreshold {
		p.onSlowWrite(elapsed)
	}
	return err
}

// onSlowWrite is a write that blocked long enough for the caller to be heard
// late, said while it is still happening.
//
// Two lines can come out of here and they say different things. That writes are
// slow is worth knowing once a second for as long as it lasts; that frames were
// LOST behind this write is worth a line of its own every time it happens,
// because those are the caller's words and they are not coming back.
//
// The loss is reported here rather than at any later moment because here is
// where it becomes knowable. Frames are dropped by a full queue *while* the
// write is blocked, and nothing on this goroutine learns that the write was slow
// — or that the queue overflowed behind it — until it returns, by which time the
// socket is already moving again. Two lines, one for the drop and one for the
// recovery, is what this used to say; over 31 live episodes they landed in the
// same millisecond carrying the same count, which is the recovery having happened
// before either of them could be written.
func (p *pacer) onSlowWrite(elapsed time.Duration) {
	blockedMs := elapsed.Milliseconds()
	p.slowWrites.Add(1)
	if blockedMs > p.maxWriteMs.Load() {
		p.maxWriteMs.Store(blockedMs)
	}

	dropped := p.uplink.Stats().FramesDropped
	backlog := p.backlog(dropped)
	if backlog > p.maxBacklog.Load() {
		p.maxBacklog.Store(backlog)
	}

	now := p.now()
	if lost := dropped - p.droppedAccounted; lost > 0 {
		p.droppedAccounted = dropped
		p.lastSlowWarnAt = now
		p.session.log.Warn("the uplink dropped the caller's audio while a write was blocked",
			"blockedMs", blockedMs, "framesDropped", lost, "backlogFrames", backlog)
		return
	}
	if now.Sub(p.lastSlowWarnAt) < slowWriteWarnEvery {
		return
	}
	p.lastSlowWarnAt = now
	p.session.log.Warn("a write to the provider is slow",
		"writeMs", blockedMs, "backlogFrames", backlog)
}

// backlog is how many frames are waiting to go out: everything handed over that
// has not been encoded onto the wire, less what the queue threw away. Each term
// is owned by one goroutine and only grows, so the difference is a number that
// cannot drift even though nothing here holds a lock.
func (p *pacer) backlog(dropped int64) int64 {
	waiting := p.pushed.Load() - p.encoded.Load() - dropped
	if waiting < 0 {
		return 0
	}
	return waiting
}

// run paces the uplink until the session begins stopping.
//
// It is this method that Start runs as a goroutine, rather than the shared loop
// directly: a pacer that outlived its session has to be recognisable as this
// package's in a stack, which is how the leak check finds one.
func (p *pacer) run() { p.uplink.Run() }

// stopped closes when the uplink goroutine has ended.
func (p *pacer) stopped() <-chan struct{} { return p.uplink.Stopped() }

// onStreamEnd tells the server the caller's side has gone quiet, which flushes
// whatever audio it was holding rather than waiting out its own silence timer.
// It is said once per quiet: the stream reopens by itself with the next frame,
// and there is no frame to reopen it with.
func (p *pacer) onStreamEnd() {
	p.streamEnds.Add(1)
	p.uplink.Write([]byte(streamEndFrame))
}

// err is the last write failure, if there was one. It is recorded rather than
// reported: the next SendAudio returns it, and the read loop reports the
// connection itself, so one broken socket does not fail the call twice.
func (p *pacer) err() error { return p.uplink.WriteError() }

func (p *pacer) statistics() Stats {
	paced := p.uplink.Stats()
	return Stats{
		FramesSent:    paced.FramesSent,
		FramesDropped: paced.FramesDropped,
		StreamEnds:    p.streamEnds.Load(),

		SlowWrites:       p.slowWrites.Load(),
		MaxWriteMs:       p.maxWriteMs.Load(),
		MaxBacklogFrames: p.maxBacklog.Load(),
	}
}

// appendFrame is one frame of caller audio.
//
// The JSON is assembled directly around the base64 rather than marshalled from
// a struct: this runs fifty times a second per call, and marshalling would copy
// every frame twice more for no benefit. Base64's alphabet needs no JSON
// escaping, so this is safe as well as cheap.
func (p *pacer) appendFrame(audio []byte) []byte {
	const suffix = `"}}}`

	p.encoded.Add(1)
	message := make([]byte, 0,
		len(p.prefix)+base64.StdEncoding.EncodedLen(len(audio))+len(suffix))
	message = append(message, p.prefix...)
	message = base64.StdEncoding.AppendEncode(message, audio)
	return append(message, suffix...)
}
