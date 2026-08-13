// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// collect gathers emitted frames, copying because the framer reuses its buffer.
func collect(out *[][]byte) func([]byte) {
	return func(frame []byte) {
		*out = append(*out, append([]byte(nil), frame...))
	}
}

func TestFramerEmitsWholeFramesOnly(t *testing.T) {
	f := newFramer(media.LawMu)
	var frames [][]byte

	// A chunk of two and a half frames yields two, with the rest held back.
	f.push(make([]byte, media.FrameSamples*2+80), collect(&frames))
	if len(frames) != 2 {
		t.Fatalf("emitted %d frames, want 2", len(frames))
	}
	for i, frame := range frames {
		if len(frame) != media.FrameSamples {
			t.Errorf("frame %d is %d bytes, want %d", i, len(frame), media.FrameSamples)
		}
	}

	// The carried remainder combines with the next chunk.
	f.push(make([]byte, 80), collect(&frames))
	if len(frames) != 3 {
		t.Fatalf("emitted %d frames after the remainder completed, want 3", len(frames))
	}
}

// Model audio does not arrive on frame boundaries, so the stream has to be
// reassembled across chunks without losing or duplicating a byte.
func TestFramerPreservesTheStreamAcrossRaggedChunks(t *testing.T) {
	const total = media.FrameSamples * 10
	source := make([]byte, total)
	for i := range source {
		source[i] = byte(i % 251)
	}

	f := newFramer(media.LawMu)
	var frames [][]byte

	for start, n := 0, 0; start < total; n++ {
		size := min([]int{7, 300, 1, 161, 44}[n%5], total-start)
		f.push(source[start:start+size], collect(&frames))
		start += size
	}

	var reassembled []byte
	for _, frame := range frames {
		reassembled = append(reassembled, frame...)
	}
	if len(reassembled) != total {
		t.Fatalf("reassembled %d bytes from %d", len(reassembled), total)
	}
	for i := range source {
		if reassembled[i] != source[i] {
			t.Fatalf("byte %d is %d, want %d", i, reassembled[i], source[i])
		}
	}
}

func TestFramerPadsTheTailOfATurn(t *testing.T) {
	f := newFramer(media.LawAlaw)
	var frames [][]byte

	f.push(make([]byte, 40), collect(&frames))
	if len(frames) != 0 {
		t.Fatal("a partial frame was sent before the turn ended")
	}

	f.flush(collect(&frames))
	if len(frames) != 1 {
		t.Fatalf("flush emitted %d frames, want the tail padded into one", len(frames))
	}
	// The padding must be silence in the leg's own law, not zero bytes.
	frame := frames[0]
	if len(frame) != media.FrameSamples {
		t.Fatalf("padded frame is %d bytes", len(frame))
	}
	for i := 40; i < len(frame); i++ {
		if frame[i] != media.LawAlaw.Silence() {
			t.Fatalf("padding byte %d is %#x, want %#x", i, frame[i], media.LawAlaw.Silence())
		}
	}

	// Nothing is left to flush twice.
	frames = nil
	f.flush(collect(&frames))
	if len(frames) != 0 {
		t.Error("flush emitted a frame with nothing pending")
	}
}

func TestFramerResetDropsAnInterruptedTail(t *testing.T) {
	f := newFramer(media.LawMu)
	var frames [][]byte

	f.push(make([]byte, 100), collect(&frames))
	f.reset()
	f.flush(collect(&frames))

	if len(frames) != 0 {
		t.Error("audio from an interrupted turn survived the reset")
	}
}

func BenchmarkFramerSteadyState(b *testing.B) {
	f := newFramer(media.LawMu)
	// A chunk that never lines up with the frame size, which is the normal case.
	chunk := make([]byte, 470)
	emit := func([]byte) {}

	b.ReportAllocs()
	for b.Loop() {
		f.push(chunk, emit)
	}
}
