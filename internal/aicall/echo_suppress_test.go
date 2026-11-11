// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/echoprobe"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// suppressFormats are the provider paths the suppressor sits in front of: the
// G.711 passthrough, where the provider gets the leg's bytes, and the PCM16
// conversion.
var suppressFormats = []uplinkFormat{
	{"PCMU passthrough", media.LawMu, provider.OpenAIProfile(), true},
	{"PCMA passthrough", media.LawAlaw, provider.OpenAIProfile(), true},
	{"PCMU to PCM16", media.LawMu, provider.QwenProfile(), false},
	{"PCMA to PCM16", media.LawAlaw, provider.QwenProfile(), false},
}

// echoCall is a scripted call: slots of bot audio, an uplink that returns it
// echoDelaySlots later 12 dB down, and from callerFrom on a caller talking
// over it, louder than the echo and with a voice of their own.
type echoCall struct {
	law        media.Law
	bot        [][]byte
	uplink     [][]byte
	callerFrom int
}

const echoCallSlots = 260

// attenuated is a mu-law frame 12 dB quieter.
func attenuated(frame []byte) []byte {
	pcm := media.LawMu.Decode(nil, frame)
	for i := range pcm {
		pcm[i] /= 4
	}
	return media.LawMu.Encode(nil, pcm)
}

func newEchoCall(law media.Law) echoCall {
	c := echoCall{law: law, callerFrom: echoCallSlots - 60}
	c.bot = scriptedBot(echoCallSlots)
	r := rand.New(rand.NewSource(11))
	for k := range echoCallSlots {
		up := silentFrame
		if k >= echoDelaySlots && c.bot[k-echoDelaySlots] != nil {
			up = attenuated(c.bot[k-echoDelaySlots])
		}
		if k >= c.callerFrom {
			up = silentFrame
			if r.Intn(4) > 0 {
				up = randomFrame(r, 0x08)
			}
		}
		c.uplink = append(c.uplink, toLaw(law, up))
		if c.bot[k] != nil {
			c.bot[k] = toLaw(law, c.bot[k])
		}
	}
	return c
}

// toLaw re-encodes a mu-law test frame in law.
func toLaw(law media.Law, frame []byte) []byte {
	if law == media.LawMu {
		return frame
	}
	return media.Transcode(nil, frame, media.LawMu, law)
}

// play runs the call through a pump and returns what the model was fed.
func (c echoCall) play(t *testing.T, f uplinkFormat, isSuppressed, hasTap bool, dumpDir string) ([][]byte, *virtualPump) {
	t.Helper()
	leg := &tapLeg{fakeLeg: newFakeLeg(c.law)}
	var l Leg = leg
	if !hasTap {
		l = struct{ Leg }{leg} // hides SetTxTap: nothing is measured
	}
	v := startVirtualPumpWith(t, l, leg.frames, f.profile, func(s *Session) {
		s.cfg.IsEchoSuppressionEnabled = isSuppressed
		s.cfg.EchoDumpDir = dumpDir
		s.ledgerCallID = uuid.MustParse("01a19507-5177-7dc9-bef3-91e32915b768")
	})
	if hasTap {
		awaitTap(t, leg)
	}
	playScripted(t, v, leg, c.bot, 0, echoCallSlots, func(k int) []byte { return c.uplink[k] })
	fed := v.model.receivedAudio()
	if len(fed) != echoCallSlots {
		t.Fatalf("the model was fed %d slots, want %d", len(fed), echoCallSlots)
	}
	return fed, v
}

// converter is the call's own uplink conversion, for what the provider should
// have been fed. It keeps no state between frames on these paths.
func converter(t *testing.T, f uplinkFormat) func(frame []byte) []byte {
	t.Helper()
	session, _ := newUplinkSession(t, f)
	return func(frame []byte) []byte { return session.uplink.Convert(nil, frame) }
}

func TestWithSuppressionOffTheProviderIsFedExactlyWhatItWasBefore(t *testing.T) {
	for _, f := range suppressFormats {
		t.Run(f.name, func(t *testing.T) {
			c := newEchoCall(f.law)
			off, offPump := c.play(t, f, false, true, "")
			offPump.end()
			// No measurement at all: the bytes the provider got before any of
			// this existed.
			bare, barePump := c.play(t, f, false, false, "")
			barePump.end()
			if !slices.EqualFunc(off, bare, bytes.Equal) {
				t.Fatal("with suppression off the provider was fed different audio")
			}
			convert := converter(t, f)
			for k, frame := range off {
				if want := convert(c.uplink[k]); !bytes.Equal(frame, want) {
					t.Fatalf("slot %d is not the uplink frame as converted", k)
				}
			}
			// The same call with suppression on is a different stream: the
			// scenario does hold an echo the suppressor acts on.
			on, onPump := c.play(t, f, true, true, "")
			onPump.end()
			if slices.EqualFunc(on, off, bytes.Equal) {
				t.Fatal("suppression on changed nothing: the scenario does not exercise it")
			}
		})
	}
}

func TestWithSuppressionOnTheEchoIsSilenceAndTheCallerArrivesUnchanged(t *testing.T) {
	for _, f := range suppressFormats {
		t.Run(f.name, func(t *testing.T) {
			c := newEchoCall(f.law)
			fed, v := c.play(t, f, true, true, "")
			convert := converter(t, f)
			silence := convert(media.SilenceFrame(f.law))
			// Once the echo has been learned (its first second), every slot
			// of echo alone reaches the provider as silence.
			for k := echoDelaySlots + 50; k < c.callerFrom; k++ {
				if !bytes.Equal(fed[k], silence) {
					t.Fatalf("echo slot %d reached the provider", k)
				}
			}
			// The caller talking over it opens the gate within two slots, and
			// from then on arrives exactly as it came.
			opened := -1
			for k := c.callerFrom; k < echoCallSlots; k++ {
				isCaller := !bytes.Equal(c.uplink[k], toLaw(f.law, silentFrame))
				if isCaller && bytes.Equal(fed[k], convert(c.uplink[k])) {
					opened = k
					break
				}
			}
			if opened < 0 || opened > c.callerFrom+3 {
				t.Fatalf("the caller from slot %d first arrived unchanged at slot %d", c.callerFrom, opened)
			}
			for k := opened; k < echoCallSlots; k++ {
				if !bytes.Equal(fed[k], convert(c.uplink[k])) {
					t.Fatalf("caller slot %d was changed after the gate opened at %d", k, opened)
				}
			}
			line := v.end()
			if line["framesSent"] != echoCallSlots || line["framesReceived"] != echoCallSlots || line["framesFilled"] != 0 {
				t.Errorf("the uplink clock's counters moved: %v", line)
			}
			sup := floorLine(t, v, "ai echo suppression")
			for _, want := range []string{`"isEchoSuppressionOn":true`, `"state":"ENGAGED"`, `"isSuppressionFailed":false`} {
				if !strings.Contains(sup, want) {
					t.Errorf("call-end line %q lacks %s", sup, want)
				}
			}
		})
	}
}

// The measurement, its verdicts and the dump judge the uplink as it came, not
// as the provider was fed it.
func TestWithSuppressionOnTheMeasurementAndTheDumpSeeTheRawUplink(t *testing.T) {
	f := suppressFormats[0]
	c := newEchoCall(f.law)
	dir := t.TempDir()
	leg := &tapLeg{fakeLeg: newFakeLeg(c.law)}
	v := startVirtualPumpWith(t, leg, leg.frames, f.profile, func(s *Session) {
		s.cfg.IsEchoSuppressionEnabled = true
		s.cfg.EchoDumpDir = dir
		s.ledgerCallID = uuid.MustParse("01a19507-5177-7dc9-bef3-91e32915b768")
	})
	awaitTap(t, leg)
	playScripted(t, v, leg, c.bot, 0, c.callerFrom, func(k int) []byte { return c.uplink[k] })
	silence := media.SilenceFrame(f.law)
	if fed := v.model.receivedAudio(); !bytes.Equal(fed[c.callerFrom-1], silence) {
		t.Fatal("the echo reached the provider: the test proves nothing")
	}
	// An interruption now is judged on the echo the uplink carried.
	if got := echoVerdictNow(v); got != echoprobe.VerdictEcho {
		t.Fatalf("the measurement says %s over a suppressed echo, want ECHO", got)
	}
	v.end()

	data, err := os.ReadFile(filepath.Join(dir, "01a19507-5177-7dc9-bef3-91e32915b768.wav"))
	if err != nil {
		t.Fatal(err)
	}
	pcm := data[44:]
	raw := make([]int16, 0, media.FrameSamples)
	for k := 0; k < c.callerFrom; k++ {
		raw = f.law.Decode(raw, c.uplink[k])
		for i := range media.FrameSamples {
			at := ((k*media.FrameSamples+i)*2 + 1) * 2
			if got := int16(binary.LittleEndian.Uint16(pcm[at:])); got != raw[i] {
				t.Fatalf("dump slot %d sample %d is %d, the raw uplink %d", k, i, got, raw[i])
			}
		}
	}
	if n := strings.Count(v.logs.String(), `"msg":"ai echo suppression"`); n != 1 {
		t.Errorf("%d \"ai echo suppression\" lines, want 1", n)
	}
}

// Hushed slots send nothing, suppression on or off; fills are silence either
// way; and the clock's accounting is the same.
func TestWithSuppressionOnHushedAndFillSlotsAreAsBefore(t *testing.T) {
	run := func(isSuppressed bool) ([][]byte, map[string]float64) {
		c := newEchoCall(media.LawMu)
		leg := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
		v := startVirtualPumpWith(t, leg, leg.frames, provider.OpenAIProfile(), func(s *Session) {
			s.cfg.IsEchoSuppressionEnabled = isSuppressed
		})
		awaitTap(t, leg)
		playScripted(t, v, leg, c.bot, 0, 100, func(k int) []byte { return c.uplink[k] })
		v.session.hush(true)
		// Nothing is fed while hushed, so the slots are stepped by time
		// rather than by what the model received.
		for k := 100; k < 150; k++ {
			leg.tick(c.bot[k], v.epoch.Add(time.Duration(k)*uplinkSlot))
			v.frames <- append(media.GetBytes(media.FrameSamples), c.uplink[k]...)
			v.step(1)
		}
		v.session.hush(false)
		// Slots with nothing queued are fills.
		for k := 150; k < 160; k++ {
			leg.tick(c.bot[k], v.epoch.Add(time.Duration(k)*uplinkSlot))
			v.step(1)
		}
		fed := v.model.receivedAudio()
		return fed, v.end()
	}
	off, offLine := run(false)
	on, onLine := run(true)
	for _, key := range []string{"framesReceived", "framesSent", "framesFilled", "framesDroppedHushed"} {
		if offLine[key] != onLine[key] {
			t.Errorf("%s is %v with suppression on, %v off", key, onLine[key], offLine[key])
		}
	}
	if len(on) != len(off) || len(on) != 110 {
		t.Fatalf("fed %d slots on, %d off, want 110 (100 before the hush, 10 fills after)", len(on), len(off))
	}
	for k := 100; k < 110; k++ {
		if !bytes.Equal(on[k], media.SilenceFrame(media.LawMu)) || !bytes.Equal(on[k], off[k]) {
			t.Fatalf("fill %d differs with suppression on", k)
		}
	}
}

// BenchmarkUplinkClockWithEchoSuppression is the uplink pump's slot with the
// echo measurement and the suppressor on, the RTP send loop's tap feeding it:
// the whole added per-frame path, on loud bot audio and a loud uplink so that
// the suppressor correlates every slot. echo returns the bot's audio 400 ms
// later (the measurement soon knows the delay); noEcho is an unrelated uplink
// (the wide window, all call). It must stay at 0 allocs/op.
func BenchmarkUplinkClockWithEchoSuppression(b *testing.B) {
	for _, f := range []uplinkFormat{uplinkFormats[0], uplinkFormats[2]} {
		for _, isEcho := range []bool{true, false} {
			name := f.name + "/noEcho"
			if isEcho {
				name = f.name + "/echo"
			}
			b.Run(name, func(b *testing.B) {
				leg := &tapLeg{fakeLeg: newFakeLeg(f.law)}
				session, err := New(leg, nopModel{}, f.profile, Config{IsEchoSuppressionEnabled: true})
				if err != nil {
					b.Fatal(err)
				}
				epoch := time.Unix(1_000_000, 0)
				clock := newUplinkClock(epoch)
				probe := session.startEchoProbe(clock.start)
				converted := make([]byte, 0, media.FrameSamples*8)
				r := rand.New(rand.NewSource(5))
				refs := make([][]byte, 64)
				ups := make([][]byte, 64)
				for i := range refs {
					refs[i] = randomFrame(r, 0x10)
					ups[i] = randomFrame(r, 0x10)
				}
				frame := make([]byte, media.FrameSamples)
				now := epoch
				i := 0
				b.ReportAllocs()
				for b.Loop() {
					leg.tick(refs[i&63], now)
					if isEcho {
						copy(frame, refs[(i-echoDelaySlots)&63])
					} else {
						copy(frame, ups[i&63])
					}
					clock.push(frame)
					now = now.Add(uplinkSlot)
					i++
					// sendDueSlots minus the pool return, as in
					// BenchmarkUplinkClockWithEchoProbe.
					for n := clock.owed(now); n > 0; n-- {
						f := clock.pop()
						probe.FeedUplink(f)
						probe.Suppress(f)
						converted, _ = session.feedSlot(clock, f, converted)
					}
				}
			})
		}
	}
}
