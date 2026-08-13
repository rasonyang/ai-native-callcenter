// SPDX-License-Identifier: Apache-2.0

// Package media holds the audio primitives shared by the SIP leg and the
// speech providers: G.711 companding, sample-rate conversion, and the buffer
// reuse that keeps two hundred concurrent calls off the allocator.
package media

// Law is a G.711 companding law.
type Law uint8

// The two G.711 laws. Both are supported end to end: a leg negotiated as PCMA
// is never silently transcoded to PCMU, because a codec that is only half
// wired is worse than one that is absent.
const (
	LawMu Law = iota
	LawAlaw
)

// String names the law as it appears in SDP.
func (l Law) String() string {
	if l == LawAlaw {
		return "PCMA"
	}
	return "PCMU"
}

// PayloadType is the static RTP payload type for the law.
func (l Law) PayloadType() uint8 {
	if l == LawAlaw {
		return 8
	}
	return 0
}

// Silence is the encoded byte that represents silence in this law. It is not
// zero: a run of 0x00 is a loud buzz in µ-law and a quiet tone in A-law.
func (l Law) Silence() byte {
	if l == LawAlaw {
		return 0xD5
	}
	return 0xFF
}

// Encoding and decoding go through tables built once at startup. The µ-law
// encode table is indexed by the full 16-bit sample space, which costs 64 KiB
// once and removes all branching from the hot path.
var (
	muEncode   [65536]byte
	muDecode   [256]int16
	alawEncode [65536]byte
	alawDecode [256]int16
	muToAlaw   [256]byte
	alawToMu   [256]byte
)

func init() {
	for i := range 256 {
		muDecode[i] = muLawToLinear(byte(i))
		alawDecode[i] = aLawToLinear(byte(i))
	}
	for i := range 65536 {
		sample := int16(i)
		muEncode[i] = linearToMuLaw(sample)
		alawEncode[i] = linearToALaw(sample)
	}
	for i := range 256 {
		muToAlaw[i] = alawEncode[uint16(muDecode[i])]
		alawToMu[i] = muEncode[uint16(alawDecode[i])]
	}
}

// Encode writes PCM samples to dst as G.711 in this law and returns dst.
// dst must have room for len(samples) bytes.
func (l Law) Encode(dst []byte, samples []int16) []byte {
	dst = dst[:0]
	if l == LawAlaw {
		for _, s := range samples {
			dst = append(dst, alawEncode[uint16(s)])
		}
		return dst
	}
	for _, s := range samples {
		dst = append(dst, muEncode[uint16(s)])
	}
	return dst
}

// Decode writes G.711 bytes to dst as PCM samples and returns dst.
func (l Law) Decode(dst []int16, encoded []byte) []int16 {
	dst = dst[:0]
	if l == LawAlaw {
		for _, b := range encoded {
			dst = append(dst, alawDecode[b])
		}
		return dst
	}
	for _, b := range encoded {
		dst = append(dst, muDecode[b])
	}
	return dst
}

// Transcode converts encoded audio between the two laws without going through
// linear PCM in the caller.
func Transcode(dst []byte, encoded []byte, from, to Law) []byte {
	dst = dst[:0]
	if from == to {
		return append(dst, encoded...)
	}
	table := &muToAlaw
	if from == LawAlaw {
		table = &alawToMu
	}
	for _, b := range encoded {
		dst = append(dst, table[b])
	}
	return dst
}

//
// The companding algorithms themselves, from ITU-T G.711.
//

const (
	muBias = 0x84
	muClip = 32635
)

func linearToMuLaw(sample int16) byte {
	sign := (sample >> 8) & 0x80
	if sign != 0 {
		sample = -sample
	}
	if sample > muClip {
		sample = muClip
	}
	sample += muBias

	exponent := muExponent[(sample>>7)&0xFF]
	mantissa := (sample >> (exponent + 3)) & 0x0F
	return byte(^(sign | int16(exponent)<<4 | mantissa))
}

func muLawToLinear(u byte) int16 {
	u = ^u
	sign := int(u & 0x80)
	exponent := int(u>>4) & 0x07
	mantissa := int(u & 0x0F)

	sample := ((mantissa << 3) + muBias) << exponent
	sample -= muBias
	if sign != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

var muExponent = [256]int16{
	0, 0, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3,
	4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
}

func linearToALaw(sample int16) byte {
	sign := ((^sample) >> 8) & 0x80
	if sign == 0 {
		sample = -sample
	}
	if sample > 32635 {
		sample = 32635
	}

	var compressed byte
	if sample >= 256 {
		exponent := int16(alawExponent[(sample>>8)&0x7F])
		mantissa := (sample >> (exponent + 3)) & 0x0F
		compressed = byte((exponent << 4) | mantissa)
	} else {
		compressed = byte(sample >> 4)
	}
	return compressed ^ byte(sign) ^ 0x55
}

func aLawToLinear(a byte) int16 {
	a ^= 0x55
	sign := a & 0x80
	exponent := int((a & 0x70) >> 4)
	mantissa := int(a & 0x0F)

	sample := mantissa << 4
	switch exponent {
	case 0:
		sample += 8
	case 1:
		sample += 0x108
	default:
		sample += 0x108
		sample <<= exponent - 1
	}
	if sign == 0 {
		return int16(-sample)
	}
	return int16(sample)
}

var alawExponent = [128]byte{
	1, 1, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
}
