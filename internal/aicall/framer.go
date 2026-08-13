// SPDX-License-Identifier: Apache-2.0

package aicall

import "github.com/rasonyang/ai-native-callcenter/internal/media"

// framer cuts a model's audio into the fixed frames the telephone leg sends.
//
// A speech model emits audio in whatever chunks suit it — a sentence at a
// time, or a few milliseconds. The wire wants exactly one 20 ms frame every
// 20 ms, so the remainder of each chunk is carried into the next one. Without
// this the leg would send short frames, which peers interpret as a different
// packetisation than the one the SDP promised.
type framer struct {
	law media.Law
	// size is one frame in bytes. G.711 is one byte per sample.
	size int
	// pending holds the tail of the last chunk, less than one frame long.
	pending []byte
}

func newFramer(law media.Law) *framer {
	return &framer{
		law:     law,
		size:    media.FrameSamples,
		pending: make([]byte, 0, media.FrameSamples*2),
	}
}

// push emits every whole frame available and keeps the rest.
func (f *framer) push(audio []byte, emit func(frame []byte)) {
	// The common case is a chunk large enough to frame directly, with nothing
	// carried over; avoid copying through pending when that holds.
	if len(f.pending) == 0 {
		for len(audio) >= f.size {
			emit(audio[:f.size])
			audio = audio[f.size:]
		}
		f.pending = append(f.pending, audio...)
		return
	}

	f.pending = append(f.pending, audio...)
	offset := 0
	for len(f.pending)-offset >= f.size {
		emit(f.pending[offset : offset+f.size])
		offset += f.size
	}
	// Move the tail back to the front of the same buffer. Re-slicing forward
	// instead would walk through the capacity and reallocate on every chunk
	// for the length of the call.
	f.pending = f.pending[:copy(f.pending, f.pending[offset:])]
}

// flush emits the remainder, padded to a whole frame with silence.
//
// It is called when a turn ends: the last few milliseconds of a sentence are
// worth padding rather than dropping, and the padding is inaudible.
func (f *framer) flush(emit func(frame []byte)) {
	if len(f.pending) == 0 {
		return
	}
	frame := make([]byte, f.size)
	copy(frame, f.pending)
	for i := len(f.pending); i < f.size; i++ {
		frame[i] = f.law.Silence()
	}
	f.pending = f.pending[:0]
	emit(frame)
}

// reset drops anything buffered, for a response that was interrupted.
func (f *framer) reset() { f.pending = f.pending[:0] }
