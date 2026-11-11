// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"math/rand"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// frames makes a ring of loud noise frames, so every iteration has bot speech
// in it and the estimate schedule fires as it would on a call.
func benchFrames() [][]byte {
	r := rand.New(rand.NewSource(1))
	out := make([][]byte, 64)
	for i := range out {
		out[i] = make([]byte, frameSamples)
		for j := range out[i] {
			out[i][j] = byte(r.Intn(256))
		}
	}
	return out
}

// BenchmarkPushReference is the RTP send loop's cost per 20 ms tick.
func BenchmarkPushReference(b *testing.B) {
	m := New(Config{Start: time.Unix(1, 0), Law: media.LawMu})
	frames := benchFrames()
	at := time.Unix(1, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.PushReference(frames[i&63], at)
		m.txMu.Lock() // keep the queue from filling; the drain is measured below
		m.txCount = 0
		m.txMu.Unlock()
	}
}

// BenchmarkFeedUplink is the uplink pump's cost per slot: draining the
// reference, decoding, the envelopes, the dump-free path, and the estimates
// that fall due on a call of constant bot speech.
func BenchmarkFeedUplink(b *testing.B) {
	start := time.Unix(1, 0)
	m := New(Config{Start: start, Law: media.LawMu})
	frames := benchFrames()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.PushReference(frames[i&63], start.Add(time.Duration(i)*20*time.Millisecond))
		m.FeedUplink(frames[(i+7)&63])
	}
}

// BenchmarkEstimate is one cross-correlation, the heaviest thing the monitor
// does and one that runs about every five seconds of bot speech.
func BenchmarkEstimate(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	var up, ref [envBins]float32
	for i := range up {
		ref[i] = float32(500 + r.Intn(2000))
		up[i] = ref[i] * 0.3
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		correlate(&up, &ref, 900)
	}
}

// BenchmarkJudge is the barge-in path's question: the copies under the lock,
// the waveform correlation (two 16384-point transforms) and the decision. It
// runs once per speech interruption, after the flush, never per frame.
func BenchmarkJudge(b *testing.B) {
	r := rand.New(rand.NewSource(3))
	start := time.Unix(1, 0)
	m := New(Config{Start: start, Law: media.LawMu})
	law := media.LawMu
	ref := make([]int16, frameSamples)
	up := make([]int16, frameSamples)
	enc := make([]byte, 0, frameSamples)
	history := make([][]int16, 0, 200)
	for i := 0; i < 200; i++ {
		for j := range ref {
			ref[j] = int16(r.NormFloat64() * 3000)
		}
		history = append(history, append([]int16(nil), ref...))
		m.PushReference(law.Encode(enc, ref), start.Add(time.Duration(i)*20*time.Millisecond))
		// The uplink is the reference 400 ms (20 slots) later, at -10 dB.
		for j := range up {
			up[j] = 0
			if i >= 20 {
				up[j] = history[i-20][j] / 3
			}
		}
		m.FeedUplink(law.Encode(enc, up))
	}
	if j := m.Judge(); j.Verdict != VerdictEcho || j.LagMs != 400 {
		b.Fatalf("%+v", j)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Judge()
	}
}

// benchSuppress is the uplink pump's cost per slot with suppression on: the
// measurement (as BenchmarkFeedUplink) plus the suppressor's decision and
// Suppress, on loud bot audio and a loud uplink, so that the waveform
// correlation runs on every slot unless the case skips it. hint, when set,
// runs before each slot.
func benchSuppress(b *testing.B, hasBot bool, hint func(m *Monitor)) {
	start := time.Unix(1, 0)
	m := New(Config{Start: start, Law: media.LawMu, Suppress: true})
	frames := benchFrames()
	frame := make([]byte, frameSamples)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var ref []byte
		if hasBot {
			ref = frames[i&63]
		}
		m.PushReference(ref, start.Add(time.Duration(i)*20*time.Millisecond))
		if hint != nil {
			hint(m)
		}
		copy(frame, frames[(i+7)&63])
		m.FeedUplink(frame)
		m.Suppress(frame)
	}
	b.StopTimer()
	if hasBot && m.sup.correlated < int64(b.N)/2 {
		b.Fatalf("%d of %d slots correlated: the case does not measure the correlation", m.sup.correlated, b.N)
	}
}

// BenchmarkSuppressWithDelay: the measurement knows the delay, so one
// transform pair covers the ±80 ms window.
func BenchmarkSuppressWithDelay(b *testing.B) {
	benchSuppress(b, true, func(m *Monitor) { m.track.isFound, m.track.lagBins = true, 40 })
}

// BenchmarkSuppressWithoutDelay: no delay known, the 20-700 ms window.
func BenchmarkSuppressWithoutDelay(b *testing.B) {
	benchSuppress(b, true, func(m *Monitor) { m.track.isFound, m.track.hasLast = false, false })
}

// BenchmarkSuppressNoBotAudio: nothing the bot sent is in reach; the slot
// passes without a correlation.
func BenchmarkSuppressNoBotAudio(b *testing.B) {
	benchSuppress(b, false, nil)
}
