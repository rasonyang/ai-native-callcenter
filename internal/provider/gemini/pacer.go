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
)

// Stats is what the uplink did, for the line logged when a session ends. A call
// with drops in it had five seconds of the caller's words go missing, which is
// the socket having stalled for longer than anything measured.
type Stats struct {
	FramesSent    int64
	FramesDropped int64
	StreamEnds    int64
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

	streamEnds atomic.Int64
}

func newPacer(session *Session, ticks <-chan time.Time, mimeType string) *pacer {
	p := &pacer{
		session: session,
		prefix:  []byte(`{"realtimeInput":{"audio":{"mimeType":"` + mimeType + `","data":"`),
	}
	p.uplink = uplink.New(uplink.Config{
		Ticks:      ticks,
		Stopping:   session.stopping,
		Write:      session.sendFrame,
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
func (p *pacer) push(frame []byte) { p.uplink.Push(frame) }

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

	message := make([]byte, 0,
		len(p.prefix)+base64.StdEncoding.EncodedLen(len(audio))+len(suffix))
	message = append(message, p.prefix...)
	message = base64.StdEncoding.AppendEncode(message, audio)
	return append(message, suffix...)
}
