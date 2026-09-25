// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"encoding/base64"
	"sync/atomic"
	"time"

	uplink "github.com/rasonyang/ai-native-callcenter/internal/provider/pacer"
)

// The uplink cadence.
//
// This provider reads the upstream stream as a clock — "strictly at real time;
// too fast OR too slow is an error" — and as a keepalive: stop feeding it
// without saying so and the model stops answering altogether. Neither is true
// of the other protocol in this repository, where audio is written straight
// through from whatever drives the telephone leg.
//
// So the cadence is this client's own, and the shared pacer is the mechanism it
// is spelled with: one frame every 20 ms, three frames of jitter absorbed, the
// oldest dropped rather than caught up in a burst. What belongs to this protocol
// and nothing else is what the frames say — the append, and the two frames that
// declare a hold and end it.
const (
	// queueDepth is how much jitter is absorbed: three frames, 60 ms. Deeper
	// would be latency the caller hears, and the frames past it are ones the
	// conversation has already moved beyond.
	queueDepth = 3
	// framesPerTick is one, because a burst is a pacing error to this engine.
	framesPerTick = 1
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
//
// The cadence itself is shared. What is here is this protocol's half of it: the
// frames, and the count of how often the hold was declared and lifted — which
// only this package can keep, because only this protocol has such a frame.
type pacer struct {
	session *Session
	uplink  *uplink.Pacer

	mutes   atomic.Int64
	unmutes atomic.Int64
}

func newPacer(session *Session, ticks <-chan time.Time) *pacer {
	p := &pacer{session: session}
	p.uplink = uplink.New(uplink.Config{
		Ticks:      ticks,
		Stopping:   session.stopping,
		Write:      session.sendFrame,
		Encode:     appendFrame,
		Depth:      queueDepth,
		MaxPerTick: framesPerTick,
		IdleTicks:  muteAfterEmptyTicks,
		OnIdle:     p.onMute,
		OnResume:   p.onUnmute,
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

// onMute says the microphone is off. Whatever was queued has already been
// forgotten by the time this runs: it is audio from before the silence, and
// sending it on resume would play the caller a moment of their own past.
func (p *pacer) onMute() {
	p.mutes.Add(1)
	p.uplink.Write(p.session.simpleFrame("input_audio_mute.commit"))
}

// onUnmute says it is back on. It goes out ahead of the frame that resumed the
// uplink, on the same tick: this is a frame the provider has to be ready for.
func (p *pacer) onUnmute() {
	p.unmutes.Add(1)
	p.uplink.Write(p.session.simpleFrame("input_audio_unmute.commit"))
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
		Mutes:         p.mutes.Load(),
		Unmutes:       p.unmutes.Load(),
	}
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
