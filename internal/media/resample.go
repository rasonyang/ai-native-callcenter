// SPDX-License-Identifier: Apache-2.0

package media

import "math"

// Sample rates this application converts between.
const (
	// RateTelephone is the rate on the wire: G.711 is always 8 kHz.
	RateTelephone = 8000
	// RateProviderIn is what the Chinese provider expects to be fed.
	RateProviderIn = 16000
	// RateProviderOut is what it speaks back.
	RateProviderOut = 24000
)

// FrameSamples is one 20 ms frame at 8 kHz, the packetisation used on the RTP
// leg throughout.
const FrameSamples = RateTelephone / 50

// Upsample raises the rate by an integer factor using linear interpolation.
//
// Linear interpolation leaves images above the original Nyquist frequency, but
// the destination here is a speech model fed from an 8 kHz telephone channel:
// there is nothing above 4 kHz to preserve, and the artefacts sit where the
// channel had no content anyway. The downward direction is the one that needs
// a real filter.
func Upsample(dst, src []int16, factor int) []int16 {
	dst = dst[:0]
	if factor <= 1 || len(src) == 0 {
		return append(dst, src...)
	}

	for i := range len(src) - 1 {
		current, next := float64(src[i]), float64(src[i+1])
		step := (next - current) / float64(factor)
		for f := range factor {
			dst = append(dst, clamp16(current+step*float64(f)))
		}
	}
	// The final sample has no successor to interpolate towards; holding it is
	// inaudible over 125 microseconds and avoids inventing a value.
	last := src[len(src)-1]
	for range factor {
		dst = append(dst, last)
	}
	return dst
}

// Downsampler reduces the rate by an integer factor.
//
// A windowed-sinc low-pass runs before decimation. Plain averaging, or taking
// every nth sample, folds everything above the new Nyquist frequency back into
// the audible band, which is heard as the muffled, metallic speech that the
// reference implementation had to fix in production.
//
// The filter keeps its tail between calls, so a stream converted frame by
// frame sounds identical to the same audio converted in one piece.
type Downsampler struct {
	factor int
	taps   []float64
	// history holds the samples the filter still needs from previous frames.
	history []float64
	// next is where the next output sample is taken from, expressed as an
	// index into the coming window. Carrying it explicitly keeps the
	// decimation phase correct when chunks are not a whole number of output
	// samples long, which is the normal case for provider audio.
	next int
	// window is reused across calls so the hot path does not allocate.
	window []float64
}

// NewDownsampler builds a downsampler for the given integer factor. taps
// controls filter length; 32 is enough for telephone-band speech.
func NewDownsampler(factor, taps int) *Downsampler {
	if factor < 1 {
		factor = 1
	}
	if taps < 8 {
		taps = 8
	}
	if taps%2 == 0 {
		taps++ // an odd length keeps the filter symmetric about its centre
	}

	d := &Downsampler{
		factor:  factor,
		taps:    make([]float64, taps),
		history: make([]float64, taps-1),
		next:    taps - 1, // the first output needs a full window behind it
		window:  make([]float64, 0, taps+4*FrameSamples),
	}

	// Windowed sinc, cut off just below the new Nyquist frequency, with a
	// Hamming window to keep the stopband down without ringing.
	cutoff := 0.5 / float64(factor)
	centre := float64(taps-1) / 2
	var sum float64
	for i := range taps {
		x := float64(i) - centre
		var h float64
		if x == 0 {
			h = 2 * cutoff
		} else {
			h = math.Sin(2*math.Pi*cutoff*x) / (math.Pi * x)
		}
		// Hamming.
		h *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(taps-1))
		d.taps[i] = h
		sum += h
	}
	// Normalise to unity gain so the conversion neither quietens nor clips.
	for i := range d.taps {
		d.taps[i] /= sum
	}
	return d
}

// Process filters and decimates src into dst, carrying state across calls.
func (d *Downsampler) Process(dst, src []int16) []int16 {
	dst = dst[:0]
	if d.factor == 1 {
		return append(dst, src...)
	}

	taps := len(d.taps)
	// Work over the retained tail followed by this frame, so the filter sees a
	// continuous stream.
	window := append(d.window[:0], d.history...)
	for _, s := range src {
		window = append(window, float64(s))
	}
	d.window = window

	i := d.next
	for ; i < len(window); i += d.factor {
		var acc float64
		for j, tap := range d.taps {
			acc += tap * window[i-j]
		}
		dst = append(dst, clamp16(acc))
	}

	// Keep the last taps-1 samples, then re-base the phase onto them so the
	// next call resumes exactly where this one stopped.
	keep := min(taps-1, len(window))
	consumed := len(window) - keep
	d.history = append(d.history[:0], window[consumed:]...)
	d.next = i - consumed
	return dst
}

// Reset clears filter state, for reuse on a new call.
func (d *Downsampler) Reset() {
	d.history = d.history[:len(d.taps)-1]
	clear(d.history)
	d.next = len(d.taps) - 1
}

func clamp16(v float64) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(math.Round(v))
}

// BytesToPCM16 reinterprets little-endian bytes as samples, which is how every
// provider sends and expects linear audio.
func BytesToPCM16(dst []int16, src []byte) []int16 {
	dst = dst[:0]
	for i := 0; i+1 < len(src); i += 2 {
		dst = append(dst, int16(src[i])|int16(src[i+1])<<8)
	}
	return dst
}

// PCM16ToBytes writes samples as little-endian bytes.
func PCM16ToBytes(dst []byte, src []int16) []byte {
	dst = dst[:0]
	for _, s := range src {
		dst = append(dst, byte(s), byte(s>>8))
	}
	return dst
}
