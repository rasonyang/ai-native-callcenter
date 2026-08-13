// SPDX-License-Identifier: Apache-2.0

package media

import (
	"math"
	"slices"
	"testing"
)

func TestConverterRejectsRatesThatAreNotIntegerMultiples(t *testing.T) {
	if _, err := NewConverter(PCM16Format(44100), PCM16Format(8000)); err == nil {
		t.Error("accepted a ratio this resampler cannot honour")
	}
	if _, err := NewConverter(PCM16Format(0), PCM16Format(8000)); err == nil {
		t.Error("accepted a zero rate")
	}
}

// The provider path that accepts G.711 must not pay for conversion it does not
// need — that is the whole reason the RTP session keeps audio encoded.
func TestSameFormatIsPassthrough(t *testing.T) {
	c, err := NewConverter(G711Format(LawMu), G711Format(LawMu))
	if err != nil {
		t.Fatalf("new converter: %v", err)
	}
	if !c.IsPassthrough() {
		t.Fatal("identical formats did not report as passthrough")
	}

	src := []byte{0x01, 0xFF, 0x7F}
	got := c.Convert(make([]byte, 0, 3), src)
	for i := range src {
		if got[i] != src[i] {
			t.Errorf("byte %d changed from %#x to %#x", i, src[i], got[i])
		}
	}
}

func TestLawChangeAtTheSameRateSkipsTheLinearDomain(t *testing.T) {
	c, _ := NewConverter(G711Format(LawMu), G711Format(LawAlaw))
	if c.IsPassthrough() {
		t.Fatal("a law change reported as passthrough")
	}

	original := []int16{0, 4000, -4000, 16000}
	muBytes := LawMu.Encode(make([]byte, 0, 4), original)

	got := c.Convert(make([]byte, 0, 4), muBytes)
	want := Transcode(make([]byte, 0, 4), muBytes, LawMu, LawAlaw)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("byte %d = %#x, want %#x", i, got[i], want[i])
		}
	}
}

// The uplink for the provider that needs linear audio: G.711 off the wire at
// 8 kHz becomes PCM16 at 16 kHz.
func TestTelephoneToProviderUplink(t *testing.T) {
	c, err := NewConverter(G711Format(LawMu), PCM16Format(RateProviderIn))
	if err != nil {
		t.Fatalf("new converter: %v", err)
	}

	tone := make([]int16, FrameSamples)
	for i := range tone {
		tone[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/RateTelephone))
	}
	encoded := LawMu.Encode(make([]byte, 0, FrameSamples), tone)

	got := c.Convert(make([]byte, 0, FrameSamples*4), encoded)
	if want := FrameSamples * 2 * 2; len(got) != want {
		t.Fatalf("produced %d bytes, want %d (double the samples, two bytes each)", len(got), want)
	}

	// The audio must survive, not merely have the right length.
	samples := BytesToPCM16(make([]int16, 0, FrameSamples*2), got)
	var energy float64
	for _, s := range samples {
		energy += float64(s) * float64(s)
	}
	if math.Sqrt(energy/float64(len(samples))) < 3000 {
		t.Error("the upconverted tone came out far too quiet")
	}
}

// The downlink for the same provider: PCM16 at 24 kHz becomes G.711 at 8 kHz.
func TestProviderToTelephoneDownlink(t *testing.T) {
	c, err := NewConverter(PCM16Format(RateProviderOut), G711Format(LawAlaw))
	if err != nil {
		t.Fatalf("new converter: %v", err)
	}

	const samples = 480 // 20 ms at 24 kHz
	tone := make([]int16, samples)
	for i := range tone {
		tone[i] = int16(9000 * math.Sin(2*math.Pi*500*float64(i)/RateProviderOut))
	}
	src := PCM16ToBytes(make([]byte, 0, samples*2), tone)

	got := c.Convert(make([]byte, 0, FrameSamples), src)
	if len(got) != samples/3 {
		t.Fatalf("produced %d bytes, want %d", len(got), samples/3)
	}

	decoded := LawAlaw.Decode(make([]int16, 0, len(got)), got)
	var energy float64
	for _, s := range decoded {
		energy += float64(s) * float64(s)
	}
	if math.Sqrt(energy/float64(len(decoded))) < 3000 {
		t.Error("the downconverted tone came out far too quiet")
	}
}

// A stream converted frame by frame must match the same audio converted whole,
// or every frame boundary is an audible click.
func TestConverterIsContinuousAcrossFrames(t *testing.T) {
	const total = 2400 // 100 ms at 24 kHz
	tone := make([]int16, total)
	for i := range tone {
		tone[i] = int16(9000 * math.Sin(2*math.Pi*700*float64(i)/RateProviderOut))
	}
	all := PCM16ToBytes(make([]byte, 0, total*2), tone)

	whole, _ := NewConverter(PCM16Format(RateProviderOut), G711Format(LawMu))
	want := slices.Clone(whole.Convert(make([]byte, 0, total/3), all))

	framed, _ := NewConverter(PCM16Format(RateProviderOut), G711Format(LawMu))
	var got []byte
	for start := 0; start < len(all); start += 480 * 2 {
		end := min(start+480*2, len(all))
		got = append(got, framed.Convert(make([]byte, 0, 160), all[start:end])...)
	}

	if len(got) != len(want) {
		t.Fatalf("framed conversion produced %d bytes, whole produced %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d differs between framed and whole conversion", i)
		}
	}
}

func TestConverterResetClearsFilterState(t *testing.T) {
	loud := make([]int16, 480)
	for i := range loud {
		loud[i] = 20000
	}
	src := PCM16ToBytes(make([]byte, 0, len(loud)*2), loud)

	c, _ := NewConverter(PCM16Format(RateProviderOut), G711Format(LawMu))
	first := slices.Clone(c.Convert(make([]byte, 0, 160), src))

	c.Reset()
	second := c.Convert(make([]byte, 0, 160), src)

	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("byte %d differs after Reset: state from the previous call leaked", i)
		}
	}
}

func BenchmarkConvertUplinkFrame(b *testing.B) {
	c, _ := NewConverter(G711Format(LawMu), PCM16Format(RateProviderIn))
	src := make([]byte, FrameSamples)
	dst := make([]byte, 0, FrameSamples*4)

	b.ReportAllocs()
	for b.Loop() {
		dst = c.Convert(dst, src)
	}
}

func BenchmarkConvertDownlinkFrame(b *testing.B) {
	c, _ := NewConverter(PCM16Format(RateProviderOut), G711Format(LawMu))
	src := make([]byte, 480*2)
	dst := make([]byte, 0, FrameSamples)

	b.ReportAllocs()
	for b.Loop() {
		dst = c.Convert(dst, src)
	}
}

func BenchmarkConvertPassthroughFrame(b *testing.B) {
	c, _ := NewConverter(G711Format(LawMu), G711Format(LawMu))
	src := make([]byte, FrameSamples)
	dst := make([]byte, 0, FrameSamples)

	b.ReportAllocs()
	for b.Loop() {
		dst = c.Convert(dst, src)
	}
}
