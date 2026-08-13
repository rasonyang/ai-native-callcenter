// SPDX-License-Identifier: Apache-2.0

package media

import "fmt"

// Encoding names how samples are represented in a byte stream.
type Encoding uint8

const (
	// EncodingPCM16 is signed 16-bit little-endian, which is what every
	// provider speaks when it is not speaking G.711.
	EncodingPCM16 Encoding = iota
	EncodingMuLaw
	EncodingALaw
)

func (e Encoding) String() string {
	switch e {
	case EncodingMuLaw:
		return "PCMU"
	case EncodingALaw:
		return "PCMA"
	default:
		return "PCM16"
	}
}

// law reports the companding law and whether this encoding is companded.
func (e Encoding) law() (Law, bool) {
	switch e {
	case EncodingMuLaw:
		return LawMu, true
	case EncodingALaw:
		return LawAlaw, true
	default:
		return 0, false
	}
}

// AudioFormat describes a stream well enough to convert it.
type AudioFormat struct {
	Encoding Encoding
	// RateHz is the sample rate. G.711 is 8000 by definition.
	RateHz int
}

// G711Format returns the telephone-side format for a law.
func G711Format(law Law) AudioFormat {
	if law == LawAlaw {
		return AudioFormat{Encoding: EncodingALaw, RateHz: RateTelephone}
	}
	return AudioFormat{Encoding: EncodingMuLaw, RateHz: RateTelephone}
}

// PCM16Format returns linear audio at a rate.
func PCM16Format(rateHz int) AudioFormat {
	return AudioFormat{Encoding: EncodingPCM16, RateHz: rateHz}
}

func (f AudioFormat) String() string { return fmt.Sprintf("%s@%d", f.Encoding, f.RateHz) }

// Converter turns one audio format into another.
//
// It exists because the two provider paths differ: one accepts G.711 straight
// off the wire and needs no conversion at all, the other needs linear audio at
// its own rates in each direction. Both go through this type so the call
// carries no knowledge of which case it is in.
//
// A converter holds filter state, so it belongs to one direction of one call
// and must not be shared. Its buffers are reused, so converting a frame costs
// no allocations.
type Converter struct {
	from, to AudioFormat

	upFactor   int
	downFactor int
	down       *Downsampler

	samples  []int16
	resample []int16
}

// NewConverter prepares a conversion between two formats.
//
// Rates must be integer multiples of one another. Every rate this application
// deals with — 8000, 16000, 24000 — satisfies that, and an arbitrary-ratio
// resampler would be more machinery than the problem needs.
func NewConverter(from, to AudioFormat) (*Converter, error) {
	if from.RateHz <= 0 || to.RateHz <= 0 {
		return nil, fmt.Errorf("media: cannot convert %s to %s: rate must be positive", from, to)
	}

	c := &Converter{
		from:     from,
		to:       to,
		samples:  make([]int16, 0, 4*FrameSamples),
		resample: make([]int16, 0, 4*FrameSamples),
	}

	switch {
	case from.RateHz == to.RateHz:
		// No resampling; both factors stay at one.
	case to.RateHz > from.RateHz:
		if to.RateHz%from.RateHz != 0 {
			return nil, fmt.Errorf("media: cannot convert %s to %s: rates are not integer multiples", from, to)
		}
		c.upFactor = to.RateHz / from.RateHz
	default:
		if from.RateHz%to.RateHz != 0 {
			return nil, fmt.Errorf("media: cannot convert %s to %s: rates are not integer multiples", from, to)
		}
		c.downFactor = from.RateHz / to.RateHz
		// Thirty-three taps is enough for telephone-band speech and cheap
		// enough to run on every frame of every concurrent call.
		c.down = NewDownsampler(c.downFactor, 32)
	}
	return c, nil
}

// From and To report the configured formats.
func (c *Converter) From() AudioFormat { return c.from }
func (c *Converter) To() AudioFormat   { return c.to }

// IsPassthrough reports whether conversion is a copy, which is the case for the
// provider path that accepts G.711 directly.
func (c *Converter) IsPassthrough() bool { return c.from == c.to }

// Convert writes src converted into dst and returns dst.
func (c *Converter) Convert(dst, src []byte) []byte {
	dst = dst[:0]
	if c.IsPassthrough() {
		return append(dst, src...)
	}

	// Companded to companded at the same rate is a table lookup, with no need
	// to visit the linear domain.
	fromLaw, isFromCompanded := c.from.Encoding.law()
	toLaw, isToCompanded := c.to.Encoding.law()
	if isFromCompanded && isToCompanded && c.upFactor == 0 && c.downFactor == 0 {
		return Transcode(dst, src, fromLaw, toLaw)
	}

	if isFromCompanded {
		c.samples = fromLaw.Decode(c.samples, src)
	} else {
		c.samples = BytesToPCM16(c.samples, src)
	}

	switch {
	case c.upFactor > 1:
		c.resample = Upsample(c.resample, c.samples, c.upFactor)
		c.samples, c.resample = c.resample, c.samples
	case c.downFactor > 1:
		c.resample = c.down.Process(c.resample, c.samples)
		c.samples, c.resample = c.resample, c.samples
	}

	if isToCompanded {
		return toLaw.Encode(dst, c.samples)
	}
	return PCM16ToBytes(dst, c.samples)
}

// Reset clears filter state so the converter can serve a new call.
func (c *Converter) Reset() {
	if c.down != nil {
		c.down.Reset()
	}
}
