// SPDX-License-Identifier: Apache-2.0

package media

import (
	"math"
	"slices"
	"testing"
)

func TestLawIdentity(t *testing.T) {
	tests := []struct {
		law     Law
		name    string
		payload uint8
		silence byte
	}{
		{LawMu, "PCMU", 0, 0xFF},
		{LawAlaw, "PCMA", 8, 0xD5},
	}
	for _, tt := range tests {
		if got := tt.law.String(); got != tt.name {
			t.Errorf("String() = %q, want %q", got, tt.name)
		}
		if got := tt.law.PayloadType(); got != tt.payload {
			t.Errorf("%s PayloadType() = %d, want %d", tt.name, got, tt.payload)
		}
		// Silence is not zero in either law: a run of 0x00 is audible noise.
		if got := tt.law.Silence(); got != tt.silence {
			t.Errorf("%s Silence() = %#x, want %#x", tt.name, got, tt.silence)
		}
	}
}

// G.711 is lossy by design; what matters is that the error stays within the
// companding step size rather than the value surviving exactly.
func TestCompandingRoundTrip(t *testing.T) {
	for _, law := range []Law{LawMu, LawAlaw} {
		t.Run(law.String(), func(t *testing.T) {
			var worst float64
			for sample := -32000; sample <= 32000; sample += 37 {
				in := []int16{int16(sample)}
				encoded := law.Encode(make([]byte, 0, 1), in)
				decoded := law.Decode(make([]int16, 0, 1), encoded)

				diff := math.Abs(float64(decoded[0]) - float64(sample))
				// The step grows with amplitude, so the error is judged
				// relative to the sample plus the smallest step.
				tolerance := math.Abs(float64(sample))*0.10 + 40
				if diff > tolerance {
					t.Fatalf("sample %d decoded to %d, error %.0f exceeds %.0f",
						sample, decoded[0], diff, tolerance)
				}
				// A ratio is only meaningful once the sample is larger than the
				// smallest quantization step; below that the step itself is the
				// whole error and the absolute bound above is the real check.
				if abs := math.Abs(float64(sample)); abs >= 256 {
					worst = math.Max(worst, diff/abs)
				}
			}
			if worst > 0.10 {
				t.Errorf("worst relative error %.3f is higher than companding should give", worst)
			}
		})
	}
}

func TestSilenceEncodesToTheSilenceByte(t *testing.T) {
	for _, law := range []Law{LawMu, LawAlaw} {
		encoded := law.Encode(make([]byte, 0, 4), []int16{0, 0, 0, 0})
		for i, b := range encoded {
			if b != law.Silence() {
				t.Errorf("%s: silent sample %d encoded to %#x, want %#x",
					law, i, b, law.Silence())
			}
		}
	}
}

func TestTranscodeBetweenLaws(t *testing.T) {
	original := []int16{0, 1000, -1000, 8000, -8000, 20000}

	muBytes := LawMu.Encode(make([]byte, 0, len(original)), original)
	asAlaw := Transcode(make([]byte, 0, len(original)), muBytes, LawMu, LawAlaw)
	backToMu := Transcode(make([]byte, 0, len(original)), asAlaw, LawAlaw, LawMu)

	decodedOriginal := LawMu.Decode(make([]int16, 0, len(original)), muBytes)
	decodedRoundTrip := LawMu.Decode(make([]int16, 0, len(original)), backToMu)

	for i := range decodedOriginal {
		diff := math.Abs(float64(decodedOriginal[i]) - float64(decodedRoundTrip[i]))
		tolerance := math.Abs(float64(decodedOriginal[i]))*0.20 + 80
		if diff > tolerance {
			t.Errorf("sample %d survived a law round trip as %d from %d (error %.0f)",
				i, decodedRoundTrip[i], decodedOriginal[i], diff)
		}
	}

	// Same law in and out must be a copy, not a conversion.
	same := Transcode(make([]byte, 0, len(muBytes)), muBytes, LawMu, LawMu)
	for i := range muBytes {
		if same[i] != muBytes[i] {
			t.Fatalf("transcoding a law to itself altered byte %d", i)
		}
	}
}

func TestUpsampleLengthAndEndpoints(t *testing.T) {
	src := []int16{0, 100, 200}
	got := Upsample(make([]int16, 0, 6), src, 2)

	if len(got) != len(src)*2 {
		t.Fatalf("length = %d, want %d", len(got), len(src)*2)
	}
	if got[0] != 0 {
		t.Errorf("first sample = %d, want the original 0", got[0])
	}
	// Interpolated points sit between their neighbours.
	if got[1] <= got[0] || got[1] >= got[2] {
		t.Errorf("interpolated sample %d does not lie between %d and %d", got[1], got[0], got[2])
	}

	if same := Upsample(make([]int16, 0, 3), src, 1); len(same) != len(src) {
		t.Errorf("a factor of one must pass the signal through unchanged")
	}
}

// A tone above the new Nyquist frequency must be attenuated, not folded back
// into the audible band. This is the failure that plain decimation produces
// and that a listener hears as metallic speech.
func TestDownsamplerRejectsAliasing(t *testing.T) {
	const (
		inRate = 24000
		factor = 3 // 24 kHz to 8 kHz, so the new Nyquist is 4 kHz
		n      = 2400
	)

	measure := func(freq float64) float64 {
		src := make([]int16, n)
		for i := range src {
			src[i] = int16(8000 * math.Sin(2*math.Pi*freq*float64(i)/inRate))
		}
		out := NewDownsampler(factor, 32).Process(make([]int16, 0, n/factor), src)

		// Ignore the filter's start-up region, then measure energy.
		var sum float64
		start := 40
		for _, s := range out[start:] {
			sum += float64(s) * float64(s)
		}
		return math.Sqrt(sum / float64(len(out)-start))
	}

	passband := measure(800)  // well inside the telephone band
	stopband := measure(9000) // far above the new Nyquist frequency

	if passband < 3000 {
		t.Fatalf("a tone inside the band came out at %.0f, the filter is eating the signal", passband)
	}
	if stopband > passband/10 {
		t.Errorf("a tone above the new Nyquist came out at %.0f against %.0f in band: "+
			"it is aliasing rather than being filtered", stopband, passband)
	}
}

// Converting a stream frame by frame must give the same audio as converting it
// in one piece, or every frame boundary becomes an audible click.
func TestDownsamplerIsContinuousAcrossFrames(t *testing.T) {
	const total = 1440
	src := make([]int16, total)
	for i := range src {
		src[i] = int16(6000 * math.Sin(2*math.Pi*440*float64(i)/24000))
	}

	whole := NewDownsampler(3, 32).Process(make([]int16, 0, total/3), src)

	framed := make([]int16, 0, total/3)
	streaming := NewDownsampler(3, 32)
	for start := 0; start < total; start += 480 {
		chunk := streaming.Process(make([]int16, 0, 160), src[start:start+480])
		framed = append(framed, chunk...)
	}

	if len(framed) != len(whole) {
		t.Fatalf("framed conversion produced %d samples, whole produced %d", len(framed), len(whole))
	}
	for i := range whole {
		if diff := math.Abs(float64(whole[i]) - float64(framed[i])); diff > 1 {
			t.Fatalf("sample %d differs by %.0f between framed and whole conversion: "+
				"the filter is not carrying state across frames", i, diff)
		}
	}
}

// Provider audio does not arrive in chunks that divide evenly by the
// conversion factor. The decimation phase must survive that, or the output
// drifts by a sample at every ragged boundary.
func TestDownsamplerHandlesRaggedChunks(t *testing.T) {
	const total = 3000
	src := make([]int16, total)
	for i := range src {
		src[i] = int16(6000 * math.Sin(2*math.Pi*600*float64(i)/24000))
	}

	whole := NewDownsampler(3, 32).Process(make([]int16, 0, total/3), src)

	// Chunk sizes that are deliberately not multiples of three.
	ragged := make([]int16, 0, total/3)
	streaming := NewDownsampler(3, 32)
	sizes := []int{7, 100, 1, 253, 40, 11}
	for start, n := 0, 0; start < total; n++ {
		size := min(sizes[n%len(sizes)], total-start)
		ragged = append(ragged, streaming.Process(make([]int16, 0, size), src[start:start+size])...)
		start += size
	}

	if len(ragged) != len(whole) {
		t.Fatalf("ragged chunking produced %d samples, whole produced %d", len(ragged), len(whole))
	}
	for i := range whole {
		if diff := math.Abs(float64(whole[i]) - float64(ragged[i])); diff > 1 {
			t.Fatalf("sample %d differs by %.0f: the decimation phase drifted "+
				"on a chunk that was not a multiple of the factor", i, diff)
		}
	}
}

func TestDownsamplerResetClearsState(t *testing.T) {
	loud := make([]int16, 480)
	for i := range loud {
		loud[i] = 20000
	}

	d := NewDownsampler(3, 32)
	first := slices.Clone(d.Process(make([]int16, 0, 160), loud))

	d.Reset()
	second := d.Process(make([]int16, 0, 160), loud)

	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("after Reset, sample %d is %d rather than the first run's %d: "+
				"state from the previous call leaked into the new one", i, second[i], first[i])
		}
	}
}

func TestByteConversionRoundTrip(t *testing.T) {
	samples := []int16{0, 1, -1, 32767, -32768, 1234}
	bytes := PCM16ToBytes(make([]byte, 0, len(samples)*2), samples)
	if len(bytes) != len(samples)*2 {
		t.Fatalf("encoded %d bytes for %d samples", len(bytes), len(samples))
	}
	back := BytesToPCM16(make([]int16, 0, len(samples)), bytes)
	for i := range samples {
		if back[i] != samples[i] {
			t.Errorf("sample %d survived as %d, want %d", i, back[i], samples[i])
		}
	}

	// A trailing odd byte is incomplete and must be dropped rather than
	// producing a bogus sample.
	if got := BytesToPCM16(make([]int16, 0, 2), []byte{1, 2, 3}); len(got) != 1 {
		t.Errorf("an odd trailing byte produced %d samples, want 1", len(got))
	}
}

func TestSilenceFrameIsPrebuilt(t *testing.T) {
	for _, law := range []Law{LawMu, LawAlaw} {
		frame := SilenceFrame(law)
		if len(frame) != FrameSamples {
			t.Fatalf("%s silence frame is %d bytes, want %d", law, len(frame), FrameSamples)
		}
		for _, b := range frame {
			if b != law.Silence() {
				t.Fatalf("%s silence frame contains %#x", law, b)
			}
		}
		// The same backing array is handed out every time: an idle call must
		// not rebuild this fifty times a second.
		if &SilenceFrame(law)[0] != &frame[0] {
			t.Errorf("%s silence frame was rebuilt rather than shared", law)
		}
	}
}

func TestBufferPoolHandsBackUsableBuffers(t *testing.T) {
	s := GetSamples(160)
	if len(s) != 0 || cap(s) < 160 {
		t.Fatalf("borrowed sample buffer has len %d cap %d", len(s), cap(s))
	}
	s = append(s, 1, 2, 3)
	PutSamples(s)

	b := GetBytes(160)
	if len(b) != 0 || cap(b) < 160 {
		t.Fatalf("borrowed byte buffer has len %d cap %d", len(b), cap(b))
	}
	PutBytes(b)

	// A buffer smaller than asked for is replaced rather than returned short.
	if big := GetSamples(100000); cap(big) < 100000 {
		t.Errorf("asked for 100000 samples, got capacity %d", cap(big))
	}
}

//
// The hot path runs fifty times a second per call in each direction. These
// benchmarks exist to keep it allocation free.
//

func BenchmarkEncodeFrame(b *testing.B) {
	samples := make([]int16, FrameSamples)
	for i := range samples {
		samples[i] = int16(i * 100)
	}
	dst := make([]byte, 0, FrameSamples)

	b.ReportAllocs()
	for b.Loop() {
		dst = LawMu.Encode(dst, samples)
	}
}

func BenchmarkDecodeFrame(b *testing.B) {
	encoded := make([]byte, FrameSamples)
	for i := range encoded {
		encoded[i] = byte(i)
	}
	dst := make([]int16, 0, FrameSamples)

	b.ReportAllocs()
	for b.Loop() {
		dst = LawMu.Decode(dst, encoded)
	}
}

func BenchmarkUpsample8kTo16k(b *testing.B) {
	src := make([]int16, FrameSamples)
	dst := make([]int16, 0, FrameSamples*2)

	b.ReportAllocs()
	for b.Loop() {
		dst = Upsample(dst, src, 2)
	}
}

func BenchmarkDownsample24kTo8k(b *testing.B) {
	src := make([]int16, FrameSamples*3)
	dst := make([]int16, 0, FrameSamples)
	d := NewDownsampler(3, 32)

	b.ReportAllocs()
	for b.Loop() {
		dst = d.Process(dst, src)
	}
}
