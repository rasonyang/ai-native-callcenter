// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"sync/atomic"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

const (
	// reorderWindowFrames is how far ahead a packet may run before the buffer
	// stops waiting for the ones missing behind it. Four frames is 80 ms —
	// enough to fix LAN reordering without adding audible delay.
	reorderWindowFrames = 4
	// maxSilenceGapFrames bounds silence filling. A gap larger than half a
	// second is not packet loss, it is the stream restarting, and filling it
	// would play half a second of nothing before catching up.
	maxSilenceGapFrames = 25
)

// jitterBuffer reorders inbound RTP payloads and fills small gaps with
// codec-correct silence.
//
// It handles reordering only; it adds no delay of its own, because the audio
// is going to a speech model rather than a human ear and every buffered
// millisecond is a millisecond of turn latency.
//
// Payloads passed to push are owned by the buffer, and payloads it returns are
// owned by the caller. Both come from the media pool, so anything the buffer
// discards it returns there itself.
//
// Only the receive goroutine calls push; the counters are atomic so health
// reporting can read them from elsewhere.
type jitterBuffer struct {
	law         media.Law
	initialized bool
	expected    uint16
	future      map[uint16][]byte

	lost    atomic.Int64 // frames that never arrived
	dropped atomic.Int64 // frames that arrived too late to be of use
	filled  atomic.Int64 // silence frames substituted for missing audio
}

func newJitterBuffer(law media.Law) *jitterBuffer {
	return &jitterBuffer{law: law, future: map[uint16][]byte{}}
}

// seqDelta reports how far a runs ahead of b. The int16 conversion is what
// makes the comparison correct across the wrap from 65535 to 0.
func seqDelta(a, b uint16) int { return int(int16(a - b)) }

// push takes one payload and returns whatever is now playable, in order.
func (j *jitterBuffer) push(seq uint16, payload []byte) [][]byte {
	if !j.initialized {
		j.initialized = true
		j.expected = seq + 1
		return [][]byte{payload}
	}

	switch delta := seqDelta(seq, j.expected); {
	case delta < 0:
		// Its slot has already been played; keeping it would play audio out of
		// order, which is worse than the gap it would fill.
		j.dropped.Add(1)
		media.PutBytes(payload)
		return nil

	case delta == 0:
		j.expected++
		return append([][]byte{payload}, j.drain()...)

	default:
		j.future[seq] = payload
		if j.maxLead() < reorderWindowFrames {
			return nil
		}
		return j.giveUpWaiting()
	}
}

// drain releases the run of buffered frames that is now contiguous.
func (j *jitterBuffer) drain() [][]byte {
	var out [][]byte
	for {
		payload, ok := j.future[j.expected]
		if !ok {
			return out
		}
		delete(j.future, j.expected)
		j.expected++
		out = append(out, payload)
	}
}

func (j *jitterBuffer) maxLead() int {
	lead := 0
	for seq := range j.future {
		lead = max(lead, seqDelta(seq, j.expected))
	}
	return lead
}

// giveUpWaiting stops waiting for frames that are not coming: a small gap is
// filled with silence, a large one is treated as a restart and skipped.
func (j *jitterBuffer) giveUpWaiting() [][]byte {
	oldest, gap := j.oldestBuffered()
	j.lost.Add(int64(gap))

	var out [][]byte
	if gap < maxSilenceGapFrames {
		for range gap {
			out = append(out, j.silenceFrame())
		}
		j.filled.Add(int64(gap))
	}

	j.expected = oldest
	return append(out, j.drain()...)
}

func (j *jitterBuffer) oldestBuffered() (seq uint16, gap int) {
	first := true
	for s := range j.future {
		if first || seqDelta(s, seq) < 0 {
			seq, first = s, false
		}
	}
	return seq, seqDelta(seq, j.expected)
}

func (j *jitterBuffer) silenceFrame() []byte {
	frame := media.GetBytes(media.FrameSamples)
	return append(frame, media.SilenceFrame(j.law)...)
}

// reset returns everything still buffered and prepares for a new stream.
func (j *jitterBuffer) reset() {
	for seq, payload := range j.future {
		media.PutBytes(payload)
		delete(j.future, seq)
	}
	j.initialized = false
}

// stats reports frames lost, dropped as too late, and filled with silence.
func (j *jitterBuffer) stats() (lost, dropped, filled int64) {
	return j.lost.Load(), j.dropped.Load(), j.filled.Load()
}
