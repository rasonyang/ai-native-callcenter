// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// memFile is an in-memory io.WriteSeeker.
type memFile struct {
	mu  sync.Mutex
	buf []byte
	pos int
}

func (f *memFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if end := f.pos + len(p); end > len(f.buf) {
		f.buf = append(f.buf, make([]byte, end-len(f.buf))...)
	}
	copy(f.buf[f.pos:], p)
	f.pos += len(p)
	return len(p), nil
}

func (f *memFile) Seek(off int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if whence != io.SeekStart {
		return 0, errors.New("only SeekStart")
	}
	f.pos = int(off)
	return off, nil
}

func (f *memFile) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.buf...)
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func openMem(f *memFile) func() (io.WriteSeeker, io.Closer, error) {
	return func() (io.WriteSeeker, io.Closer, error) { return f, nopCloser{}, nil }
}

func TestTheDumpIsAStereoWavWithTheTwoSignalsAligned(t *testing.T) {
	file := &memFile{}
	dumper := NewDumper(openMem(file), func(err error) { t.Error(err) })
	start := time.Unix(1_000_000, 0)
	m := New(Config{Start: start, Law: media.LawMu, Dump: dumper})

	// Slot k: reference is a ramp 1000+k on every sample, uplink 5000+k, so a
	// misaligned or swapped channel shows at once.
	const slots = 300
	law := media.LawMu
	var refPCM, upPCM [frameSamples]int16
	enc := make([]byte, 0, frameSamples)
	for k := 0; k < slots; k++ {
		for i := range refPCM {
			refPCM[i] = int16(1000 + 16*k)
			upPCM[i] = int16(5000 + 16*k)
		}
		enc = law.Encode(enc, refPCM[:])
		m.PushReference(enc, start.Add(time.Duration(k)*20*time.Millisecond))
		enc = law.Encode(enc, upPCM[:])
		m.FeedUplink(enc)
	}
	// What the dump must hold is what G.711 makes of those values.
	wantRef := make([]int16, 0, slots*frameSamples)
	wantUp := make([]int16, 0, slots*frameSamples)
	for k := 0; k < slots; k++ {
		for i := range refPCM {
			refPCM[i] = int16(1000 + 16*k)
			upPCM[i] = int16(5000 + 16*k)
		}
		var b []byte
		var d []int16
		b = law.Encode(b, refPCM[:])
		d = law.Decode(d, b)
		wantRef = append(wantRef, d...)
		b = law.Encode(b[:0], upPCM[:])
		d = law.Decode(d[:0], b)
		wantUp = append(wantUp, d...)
	}
	m.Summary() // closes the dump

	data := file.bytes()
	if len(data) != wavHeaderSize+slots*frameSamples*4 {
		t.Fatalf("file is %d bytes, want %d", len(data), wavHeaderSize+slots*frameSamples*4)
	}
	if string(data[0:4]) != "RIFF" || string(data[8:16]) != "WAVEfmt " || string(data[36:40]) != "data" {
		t.Fatalf("header %q", data[:44])
	}
	le := binary.LittleEndian
	if le.Uint16(data[20:]) != 1 || le.Uint16(data[22:]) != 2 || le.Uint32(data[24:]) != 8000 ||
		le.Uint16(data[34:]) != 16 || le.Uint32(data[28:]) != 32000 || le.Uint16(data[32:]) != 4 {
		t.Errorf("format fields wrong: % x", data[20:36])
	}
	if got := le.Uint32(data[40:]); int(got) != slots*frameSamples*4 {
		t.Errorf("data size %d", got)
	}
	if got := le.Uint32(data[4:]); int(got) != 36+slots*frameSamples*4 {
		t.Errorf("riff size %d", got)
	}
	pcm := data[wavHeaderSize:]
	for n := 0; n < slots*frameSamples; n++ {
		left := int16(le.Uint16(pcm[4*n:]))
		right := int16(le.Uint16(pcm[4*n+2:]))
		if left != wantRef[n] || right != wantUp[n] {
			t.Fatalf("sample %d (slot %d): left %d right %d, want %d %d",
				n, n/frameSamples, left, right, wantRef[n], wantUp[n])
		}
	}
}

func TestAStalledDumpWriterNeverBlocksTheCall(t *testing.T) {
	release := make(chan struct{})
	stalled := &stallFile{release: release}
	dumper := NewDumper(func() (io.WriteSeeker, io.Closer, error) { return stalled, nopCloser{}, nil },
		func(error) {})
	start := time.Unix(1_000_000, 0)
	m := New(Config{Start: start, Law: media.LawMu, Dump: dumper})

	frame := make([]byte, frameSamples)
	r := rand.New(rand.NewSource(1))
	r.Read(frame)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		// 40 s of call, far more than the dump holds.
		for k := 0; k < 2000; k++ {
			m.PushReference(frame, start.Add(time.Duration(k)*20*time.Millisecond))
			m.FeedUplink(frame)
		}
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the call blocked on a stalled dump writer")
	}
	closed := make(chan Summary)
	go func() { closed <- m.Summary() }()
	select {
	case s := <-closed:
		if s.DumpDropped == 0 {
			t.Errorf("a stalled writer dropped nothing: %+v", s)
		}
	case <-time.After(dumpCloseWait + 5*time.Second):
		t.Fatal("ending the call blocked on a stalled dump writer")
	}
	close(release)
}

// stallFile blocks every write until released.
type stallFile struct{ release chan struct{} }

func (f *stallFile) Write(p []byte) (int, error) { <-f.release; return len(p), nil }
func (f *stallFile) Seek(o int64, _ int) (int64, error) {
	return o, nil
}

func TestADumpThatCannotOpenIsReportedOnceAndDropsFrames(t *testing.T) {
	var errs int
	var mu sync.Mutex
	dumper := NewDumper(func() (io.WriteSeeker, io.Closer, error) { return nil, nil, errors.New("disk full") },
		func(error) { mu.Lock(); errs++; mu.Unlock() })
	left, right := make([]int16, frameSamples), make([]int16, frameSamples)
	for i := 0; i < 100; i++ {
		dumper.Write(left, right)
	}
	dumper.Close()
	mu.Lock()
	defer mu.Unlock()
	if errs != 1 {
		t.Errorf("reported %d times", errs)
	}
	if dumper.Dropped() == 0 {
		t.Error("frames written to a dump that cannot open were not counted")
	}
}
