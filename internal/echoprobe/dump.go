// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"bufio"
	"encoding/binary"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// dumpChunks is the frames the dump may hold while its writer is busy:
	// 10 s. A writer further behind than that loses frames, counted.
	dumpChunks = 500
	// dumpCloseWait bounds how long ending a call waits for the writer.
	dumpCloseWait = 2 * time.Second

	wavHeaderSize = 44
	dumpRateHz    = 8000
	dumpChannels  = 2
)

// dumpChunk is one 20 ms stereo frame, interleaved left (what the bot sent),
// right (the uplink as clocked).
type dumpChunk [frameSamples * dumpChannels]int16

// Dumper writes a call's two signals as one stereo 8 kHz PCM16 WAV. It is a
// development aid: the file holds the call's audio.
//
// Write is called on the uplink goroutine and never blocks and never
// allocates: frames travel in a fixed set of chunks that a writer goroutine
// returns once written. When the writer falls behind (a slow disk) the frame
// is dropped and counted.
type Dumper struct {
	free    chan *dumpChunk
	work    chan *dumpChunk
	done    chan struct{}
	dropped atomic.Int64
	isDone  bool
}

// NewDumper starts the writer over w. open is called on the writer goroutine,
// so a slow filesystem never delays the call; if it fails, or a write does,
// onError is told once and the dump stops (frames are then dropped and
// counted).
func NewDumper(open func() (io.WriteSeeker, io.Closer, error), onError func(error)) *Dumper {
	d := &Dumper{
		free: make(chan *dumpChunk, dumpChunks),
		work: make(chan *dumpChunk, dumpChunks),
		done: make(chan struct{}),
	}
	for range dumpChunks {
		d.free <- new(dumpChunk)
	}
	go d.run(open, onError)
	return d
}

// Write queues one frame. left and right are frameSamples samples each.
func (d *Dumper) Write(left, right []int16) {
	if d.isDone {
		return
	}
	select {
	case c := <-d.free:
		for i := range frameSamples {
			c[2*i] = left[i]
			c[2*i+1] = right[i]
		}
		d.work <- c // never blocks: work holds at most dumpChunks chunks
	default:
		d.dropped.Add(1)
	}
}

// Dropped is the frames lost to a writer that fell behind.
func (d *Dumper) Dropped() int64 { return d.dropped.Load() }

// Close stops accepting frames, lets the writer finish what is queued and
// waits for it, but not longer than dumpCloseWait: a stalled disk must not
// hold a call's teardown. The writer goroutine ends by itself afterwards.
func (d *Dumper) Close() {
	if d.isDone {
		return
	}
	d.isDone = true
	close(d.work)
	select {
	case <-d.done:
	case <-time.After(dumpCloseWait):
	}
}

func (d *Dumper) run(open func() (io.WriteSeeker, io.Closer, error), onError func(error)) {
	defer close(d.done)
	ws, closer, err := open()
	if err != nil {
		onError(err)
		d.drain()
		return
	}
	defer closer.Close()

	w := bufio.NewWriterSize(ws, 64<<10)
	var frames int64
	if _, err = w.Write(wavHeader(0)); err != nil {
		onError(err)
		d.drain()
		return
	}
	var buf [frameSamples * dumpChannels * 2]byte
	for c := range d.work {
		for i, v := range c {
			binary.LittleEndian.PutUint16(buf[2*i:], uint16(v))
		}
		_, err = w.Write(buf[:])
		d.free <- c
		if err != nil {
			onError(err)
			d.drain()
			return
		}
		frames++
	}
	if err = w.Flush(); err != nil {
		onError(err)
		return
	}
	// The sizes are only known now.
	if _, err = ws.Seek(0, io.SeekStart); err == nil {
		_, err = ws.Write(wavHeader(frames * frameSamples * dumpChannels * 2))
	}
	if err != nil {
		onError(err)
	}
}

// drain discards what is queued after a failure, so Write keeps counting
// drops rather than filling a queue nobody reads.
func (d *Dumper) drain() {
	for c := range d.work {
		d.free <- c
		d.dropped.Add(1)
	}
}

// wavHeader is the 44-byte header of a PCM16 stereo 8 kHz file with dataBytes
// of samples.
func wavHeader(dataBytes int64) []byte {
	h := make([]byte, wavHeaderSize)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataBytes))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], dumpChannels)
	binary.LittleEndian.PutUint32(h[24:], dumpRateHz)
	binary.LittleEndian.PutUint32(h[28:], dumpRateHz*dumpChannels*2)
	binary.LittleEndian.PutUint16(h[32:], dumpChannels*2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataBytes))
	return h
}

// LogErrorOnce returns an onError for NewDumper that logs the first error.
func LogErrorOnce(log *slog.Logger) func(error) {
	var once atomic.Bool
	return func(err error) {
		if once.CompareAndSwap(false, true) {
			log.Error("echo dump stopped for this call", "err", err)
		}
	}
}
