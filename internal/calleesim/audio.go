// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// silenceRMS is the linear level at or below which a frame counts as silence.
// G.711 silence decodes to zero, and a quiet line stays well under this; the
// quietest audible thing a scenario plays is far above it.
const silenceRMS = 50

// frameRMS is the root-mean-square level of one G.711 frame.
func frameRMS(law media.Law, frame []byte) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range law.Decode(nil, frame) {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// audioSource yields 20 ms G.711 frames; nil means nothing to send now.
type audioSource interface {
	next() []byte
}

// toneSource loops a sine.
type toneSource struct {
	pcm []byte
	pos int
}

func newToneSource(law media.Law, frequencyHz int) *toneSource {
	if frequencyHz <= 0 {
		frequencyHz = 440
	}
	// One second is a whole number of periods for any whole frequency, so the
	// loop has no click.
	samples := make([]int16, media.RateTelephone)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*float64(frequencyHz)*float64(i)/media.RateTelephone))
	}
	return &toneSource{pcm: law.Encode(nil, samples)}
}

func (t *toneSource) next() []byte {
	frame := t.pcm[t.pos : t.pos+media.FrameSamples]
	t.pos = (t.pos + media.FrameSamples) % len(t.pcm)
	return frame
}

// burstSource is silence with bursts of a tone at set times.
type burstSource struct {
	tone   *toneSource
	bursts []Burst
}

func newBurstSource(law media.Law, frequencyHz int, bursts []Burst) *burstSource {
	return &burstSource{tone: newToneSource(law, frequencyHz), bursts: bursts}
}

// frameAt is the frame to send when elapsed has passed since media started:
// the tone inside a burst, nil (nothing queued; the session fills with
// silence itself) outside every burst.
func (b *burstSource) frameAt(elapsed time.Duration) []byte {
	for _, burst := range b.bursts {
		if elapsed >= ms(burst.AtMs) && elapsed < ms(burst.AtMs+burst.DurationMs) {
			return b.tone.next()
		}
	}
	return nil
}

// pcmSource plays linear samples once, then goes quiet.
type pcmSource struct {
	law     media.Law
	samples []int16
	pos     int
}

func (p *pcmSource) next() []byte {
	if p.pos >= len(p.samples) {
		return nil
	}
	end := min(p.pos+media.FrameSamples, len(p.samples))
	chunk := make([]int16, media.FrameSamples) // a short last frame is padded with silence
	copy(chunk, p.samples[p.pos:end])
	p.pos = end
	return p.law.Encode(nil, chunk)
}

// readWAV loads an 8 kHz mono 16-bit PCM WAV as samples.
func readWAV(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	var format []byte
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := data[pos+8 : min(pos+8+size, len(data))]
		switch id {
		case "fmt ":
			format = body
		case "data":
			if len(format) < 16 {
				return nil, errors.New("data chunk before fmt chunk")
			}
			tag := binary.LittleEndian.Uint16(format[0:2])
			channels := binary.LittleEndian.Uint16(format[2:4])
			rate := binary.LittleEndian.Uint32(format[4:8])
			bits := binary.LittleEndian.Uint16(format[14:16])
			if tag != 1 || channels != 1 || rate != media.RateTelephone || bits != 16 {
				return nil, fmt.Errorf("want 8 kHz mono 16-bit PCM, got tag %d, %d ch, %d Hz, %d bit",
					tag, channels, rate, bits)
			}
			return media.BytesToPCM16(nil, body[:len(body)&^1]), nil
		}
		pos += 8 + size + size&1
	}
	return nil, errors.New("no data chunk")
}
