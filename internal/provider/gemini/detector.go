// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/binary"
	"math"
	"sync"
)

// Listening for the caller, in the one window where nobody else is.
//
// This protocol reports nothing about the caller until it has recognised what
// they said, which on a real call is seconds later: no speech-started, no
// speech-stopped, only a transcript that may arrive before or after the answer
// to it. Two things upstairs need to know sooner than that. The call's dead-air
// timer gives up on a silent caller after eight seconds and has no way to tell
// silence from someone talking; and the turn-latency measurement has no start
// point without the moment the caller stopped.
//
// So this listens — and ONLY where listening cannot do any harm. It is armed
// when the server has said the turn is over, or when a turn produced nothing to
// play, and disarmed the instant the model produces anything. While the model is
// speaking, or while what it said is still reaching the caller's ear, barge-in
// belongs to the server's own detector and to nothing else: this one never sends
// a frame, never interrupts a turn and never takes part in deciding whose turn
// it is. It reports two events and changes nothing.
//
// The thresholds below are unmeasured defaults. They are the one part of this
// client that a live call is expected to move, and they are named and gathered
// here so that moving them is a one-line change rather than an archaeology
// exercise.
const (
	// speechFloorRMS is the quietest frame that counts as speech whatever the
	// line sounds like, in 16-bit sample units. A telephone leg carrying speech
	// sits well above this; comfort noise and room tone sit well below it.
	speechFloorRMS = 700
	// noiseFactor is how far above the line's own noise a frame has to be when
	// the line is noisier than the floor.
	noiseFactor = 4
	// noiseAdapt is how quickly the noise estimate follows a quiet frame. Slow,
	// because the estimate must not climb to meet somebody talking.
	noiseAdapt = 0.05
	// framesToStart is how much sustained energy counts as speech rather than a
	// click or a moment of line noise: three 20 ms frames.
	framesToStart = 3
	// defaultSilenceMs is the pause that ends the caller's turn when the flow
	// names none. It matches the turn detection default.
	defaultSilenceMs = 500
	// frameMs is the packetisation of the telephone leg, which is what decides
	// how many quiet frames a pause is.
	frameMs = 20
)

// speechVerdict is what one frame changed, which is almost always nothing.
type speechVerdict uint8

const (
	speechUnchanged speechVerdict = iota
	speechBegan
	speechEnded
)

// speechDetector is the energy detector and its window.
//
// It has no goroutine. observe runs on the call's own goroutine inside
// SendAudio, arm and disarm on the read loop's, and the mutex is what makes
// those two safe together.
type speechDetector struct {
	mu sync.Mutex

	isArmed    bool
	isSpeaking bool

	loudFrames  int
	quietFrames int
	// quietFramesToEnd is the pause that ends the caller's turn, in frames.
	quietFramesToEnd int
	// noiseFloor is the running estimate of what this line sounds like when
	// nobody is talking.
	noiseFloor float64
}

func newSpeechDetector(silenceMs int) *speechDetector {
	if silenceMs <= 0 {
		silenceMs = defaultSilenceMs
	}
	frames := silenceMs / frameMs
	if frames < 1 {
		frames = 1
	}
	return &speechDetector{quietFramesToEnd: frames}
}

// arm starts listening. Whatever the detector thought before is forgotten: the
// window it is opening is a new one.
func (d *speechDetector) arm() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.isArmed = true
	d.isSpeaking = false
	d.loudFrames = 0
	d.quietFrames = 0
}

// disarm stops listening, silently. A caller who was still talking when the
// model answered them is not reported as having stopped — they were answered,
// which is a different thing, and the events that matter about the turn now
// starting are the model's.
func (d *speechDetector) disarm() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.isArmed = false
	d.isSpeaking = false
	d.loudFrames = 0
	d.quietFrames = 0
}

// observe takes one frame of 16-bit little-endian caller audio and says what it
// changed.
//
// It allocates nothing: this runs fifty times a second for every concurrent
// call, and a detector that allocated per frame would be a second media path's
// worth of garbage.
func (d *speechDetector) observe(frame []byte) speechVerdict {
	if d == nil || len(frame) < 2 {
		return speechUnchanged
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.isArmed {
		return speechUnchanged
	}

	level := frameLevel(frame)
	threshold := float64(speechFloorRMS)
	if scaled := noiseFactor * d.noiseFloor; scaled > threshold {
		threshold = scaled
	}

	if level < threshold {
		// Only a quiet frame teaches the detector what quiet sounds like here.
		d.noiseFloor += (level - d.noiseFloor) * noiseAdapt
		d.loudFrames = 0
		if !d.isSpeaking {
			return speechUnchanged
		}
		d.quietFrames++
		if d.quietFrames < d.quietFramesToEnd {
			return speechUnchanged
		}
		d.isSpeaking = false
		d.quietFrames = 0
		return speechEnded
	}

	d.quietFrames = 0
	if d.isSpeaking {
		return speechUnchanged
	}
	d.loudFrames++
	if d.loudFrames < framesToStart {
		return speechUnchanged
	}
	d.isSpeaking = true
	d.loudFrames = 0
	return speechBegan
}

// frameLevel is the root mean square of one frame, in sample units.
func frameLevel(frame []byte) float64 {
	samples := len(frame) / 2
	if samples == 0 {
		return 0
	}
	var energy float64
	for i := range samples {
		sample := float64(int16(binary.LittleEndian.Uint16(frame[2*i:])))
		energy += sample * sample
	}
	return math.Sqrt(energy / float64(samples))
}
