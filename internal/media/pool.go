// SPDX-License-Identifier: Apache-2.0

package media

import "sync"

// Buffer reuse exists for one reason: at two hundred concurrent calls, every
// per-frame allocation happens fifty times a second per call in each
// direction. Reusing buffers keeps that traffic off the allocator and the
// collector, which is what the capacity budget assumes.

var bytePool = sync.Pool{New: func() any { b := make([]byte, 0, 8192); return &b }}

// GetBytes borrows a byte buffer with at least the given capacity.
func GetBytes(capacity int) []byte {
	p := bytePool.Get().(*[]byte)
	b := *p
	if cap(b) < capacity {
		b = make([]byte, 0, capacity)
	}
	return b[:0]
}

// PutBytes returns a byte buffer.
func PutBytes(b []byte) {
	if cap(b) == 0 {
		return
	}
	b = b[:0]
	bytePool.Put(&b)
}

// SilenceFrame returns one encoded 20 ms frame of silence in the given law.
//
// It is built once per law rather than encoded on demand: an idle call sends
// fifty of these a second, and there is no reason to compand zeroes over and
// over. The returned slice is shared and must not be modified.
func SilenceFrame(law Law) []byte {
	if law == LawAlaw {
		return alawSilenceFrame
	}
	return muSilenceFrame
}

var (
	muSilenceFrame   = makeSilenceFrame(LawMu)
	alawSilenceFrame = makeSilenceFrame(LawAlaw)
)

func makeSilenceFrame(law Law) []byte {
	frame := make([]byte, FrameSamples)
	for i := range frame {
		frame[i] = law.Silence()
	}
	return frame
}
