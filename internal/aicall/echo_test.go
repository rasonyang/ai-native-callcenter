// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/echoprobe"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// tapLeg is a fake leg that, like the RTP session, lets the echo measurement
// watch what it sends.
type tapLeg struct {
	*fakeLeg
	tapMu sync.Mutex
	tap   voice.TxTap
}

func (l *tapLeg) SetTxTap(tap voice.TxTap) {
	l.tapMu.Lock()
	l.tap = tap
	l.tapMu.Unlock()
}

// tick is one send tick of the pacer: frame is what went on the wire, nil for
// padding.
func (l *tapLeg) tick(frame []byte, at time.Time) {
	l.tapMu.Lock()
	tap := l.tap
	l.tapMu.Unlock()
	if tap != nil {
		tap(frame, at)
	}
}

// stepSlot advances the pump one slot and returns once the model has been fed
// it. virtualPump.step returns when the pump has taken its second tick, not
// when it has finished that tick, so a test that feeds the next frame at once
// would let the leftover tick find the slot empty.
func stepSlot(t *testing.T, v *virtualPump, fedAfter int) {
	t.Helper()
	v.at += uplinkSlot
	v.offset.Store(int64(v.at))
	v.wake()
	for deadline := time.Now().Add(10 * time.Second); len(v.model.receivedAudio()) < fedAfter; {
		if time.Now().After(deadline) {
			t.Fatalf("the model was not fed slot %d", fedAfter)
		}
		runtime.Gosched()
	}
}

// runEchoScenario plays 200 slots (4 s) of caller audio through the pump. The
// bot's audio goes to the wire for slots 0-59 and 100-199, and a flush empties
// the queue for slots 60-99 (the pacer then sends padding). It returns what
// the model was fed and what the leg was given.
func runEchoScenario(t *testing.T, leg *tapLeg, dir string) (fed, sent [][]byte, v *virtualPump) {
	t.Helper()
	v = startVirtualPumpWith(t, leg, leg.frames, provider.OpenAIProfile(), func(s *Session) {
		s.cfg.EchoDumpDir = dir
		s.ledgerCallID = uuid.MustParse("01a19507-5177-7dc9-bef3-91e32915b768")
	})
	awaitTap(t, leg)
	for k := 0; k < 200; k++ {
		var ref []byte
		if k < 60 || k >= 100 {
			ref = callerFrame(media.LawMu, byte(0x10+k%64))
			// The session's own queueing is a different path from the tap:
			// the leg is also given the frame to send, as it would be.
			leg.Send(ref)
		}
		leg.tick(ref, v.epoch.Add(time.Duration(k)*uplinkSlot))
		v.feed(byte(0x80 + k%100))
		stepSlot(t, v, k+1)
	}
	fed = v.model.receivedAudio()
	sent = leg.sentFrames()
	v.end()
	return fed, sent, v
}

func TestMeasuringTheEchoLeavesBothAudioPathsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	measured := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
	fedMeasured, sentMeasured, _ := runEchoScenario(t, measured, dir)

	// The same call on a leg that offers no tap: nothing is measured.
	plain := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
	plainLeg := struct{ Leg }{plain} // hides SetTxTap
	v := startVirtualPump(t, plainLeg, plain.frames, provider.OpenAIProfile())
	for k := 0; k < 200; k++ {
		if k < 60 || k >= 100 {
			plain.Send(callerFrame(media.LawMu, byte(0x10+k%64)))
		}
		v.feed(byte(0x80 + k%100))
		stepSlot(t, v, k+1)
	}
	fedPlain := v.model.receivedAudio()
	sentPlain := plain.sentFrames()
	v.end()

	if len(fedMeasured) != 200 || !slices.EqualFunc(fedMeasured, fedPlain, slices.Equal[[]byte]) {
		t.Fatalf("the provider was fed different audio with the measurement on (%d vs %d frames)",
			len(fedMeasured), len(fedPlain))
	}
	if !slices.EqualFunc(sentMeasured, sentPlain, slices.Equal[[]byte]) {
		t.Fatal("the leg was given different audio with the measurement on")
	}
	for k, frame := range fedMeasured {
		if frame[0] != byte(0x80+k%100) {
			t.Fatalf("frame %d changed: %x", k, frame[:4])
		}
	}
}

func TestAFlushedTxQueueShowsAsSilenceInTheReference(t *testing.T) {
	dir := t.TempDir()
	leg := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
	_, _, v := runEchoScenario(t, leg, dir)

	data, err := os.ReadFile(filepath.Join(dir, "01a19507-5177-7dc9-bef3-91e32915b768.wav"))
	if err != nil {
		t.Fatal(err)
	}
	pcm := data[44:]
	if len(pcm) != 200*media.FrameSamples*4 {
		t.Fatalf("dump holds %d bytes, want 200 frames of stereo", len(pcm))
	}
	sample := func(slot, i, channel int) int16 {
		return int16(binary.LittleEndian.Uint16(pcm[((slot*media.FrameSamples+i)*2+channel)*2:]))
	}
	for slot := 0; slot < 200; slot++ {
		wantSilent := slot >= 60 && slot < 100
		for _, i := range []int{0, 79, 80, 159} {
			left, right := sample(slot, i, 0), sample(slot, i, 1)
			if wantSilent && left != 0 {
				t.Fatalf("slot %d: the reference holds %d after the flush, want silence", slot, left)
			}
			if !wantSilent && left == 0 {
				t.Fatalf("slot %d: the reference is silent while the bot was on the wire", slot)
			}
			// The caller's uplink is there on its own channel throughout.
			if right == 0 {
				t.Fatalf("slot %d: the uplink channel is silent", slot)
			}
		}
	}
	// And the call-end line reports it, once.
	if n := strings.Count(v.logs.String(), `"msg":"ai echo measurement"`); n != 1 {
		t.Errorf("%d \"ai echo measurement\" lines, want 1", n)
	}
}

// echoDelaySlots is the scripted far end's delay: 400 ms.
const echoDelaySlots = 20

// scriptedBot is the bot's audio for each slot of a scripted call, one G.711
// frame per slot (nil for a slot without bot audio): noise-like bursts of
// varying level between short silences, so that both its waveform and its
// envelope can be aligned.
func scriptedBot(slots int) [][]byte {
	r := rand.New(rand.NewSource(7))
	out := make([][]byte, slots)
	for k := 0; k < slots; {
		for n := 3 + r.Intn(8); n > 0 && k < slots; n, k = n-1, k+1 {
			out[k] = randomFrame(r, 0x10+r.Intn(0x30))
		}
		k += 2 + r.Intn(5)
	}
	return out
}

// randomFrame is a frame of G.711 bytes of either sign whose magnitude code
// is at least floor (lower codes are louder).
func randomFrame(r *rand.Rand, floor int) []byte {
	f := make([]byte, media.FrameSamples)
	for i := range f {
		f[i] = byte(floor+r.Intn(0x10)) | byte(r.Intn(2))<<7
	}
	return f
}

// silentFrame is G.711 silence.
var silentFrame = bytes.Repeat([]byte{0xFF}, media.FrameSamples)

// echoOf is the uplink of a far end that returns the bot's audio
// echoDelaySlots later, as loud as it was sent.
func echoOf(bot [][]byte) func(k int) []byte {
	return func(k int) []byte {
		if k < echoDelaySlots || bot[k-echoDelaySlots] == nil {
			return silentFrame
		}
		return bot[k-echoDelaySlots]
	}
}

// awaitTap waits for the measurement to attach to the leg: the pump reads its
// clock before it does.
func awaitTap(t *testing.T, leg *tapLeg) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		leg.tapMu.Lock()
		isTapped := leg.tap != nil
		leg.tapMu.Unlock()
		if isTapped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the measurement never attached to the leg")
		}
		time.Sleep(time.Millisecond)
	}
}

// playScripted runs slots [from, to) of a scripted call on a pump that has run
// from slots before: the bot's audio goes to the wire through the leg's tap,
// and the uplink carries what uplink returns for the slot.
func playScripted(t *testing.T, v *virtualPump, leg *tapLeg, bot [][]byte, from, to int, uplink func(k int) []byte) {
	t.Helper()
	for k := from; k < to; k++ {
		leg.tick(bot[k], v.epoch.Add(time.Duration(k)*uplinkSlot))
		frame := append(media.GetBytes(media.FrameSamples), uplink(k)...)
		select {
		case v.frames <- frame:
		case <-time.After(10 * time.Second):
			t.Fatal("the leg's queue stayed full")
		}
		stepSlot(t, v, k+1)
	}
}

// echoVerdictNow is what the measurement would say about an interruption now.
func echoVerdictNow(v *virtualPump) echoprobe.Verdict {
	return v.session.echo.Load().Judge().Verdict
}

// floorLine is the one log line msg was logged with.
func floorLine(t *testing.T, v *virtualPump, msg string) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(v.logs.String(), "\n") {
		if strings.Contains(l, `"msg":"`+msg+`"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d %q lines, want 1:\n%s", len(lines), msg, v.logs.String())
	}
	return lines[0]
}

func TestASpeechInterruptionReportsWhetherItIsEchoLikelyAndStillCutsTheBot(t *testing.T) {
	const slots = 160
	bot := scriptedBot(slots)
	echo := echoOf(bot)
	// A caller who starts talking over the echo, louder than it and with a
	// voice of their own.
	r := rand.New(rand.NewSource(11))
	caller := make([][]byte, slots)
	for k := range caller {
		caller[k] = silentFrame
		if r.Intn(4) > 0 {
			caller[k] = randomFrame(r, 0x08)
		}
	}
	for _, tt := range []struct {
		name string
		// play runs the call up to the interruption.
		play func(t *testing.T, v *virtualPump, leg *tapLeg)
		want echoprobe.Verdict
		// attr is the line's isEchoLikely, "" for none.
		attr string
	}{
		{"before the bot has said anything", func(t *testing.T, v *virtualPump, leg *tapLeg) {
			v.step(10)
		}, echoprobe.VerdictUnknown, ""},
		{"the bot's own echo", func(t *testing.T, v *virtualPump, leg *tapLeg) {
			playScripted(t, v, leg, bot, 0, slots, echo)
		}, echoprobe.VerdictEcho, `"isEchoLikely":true`},
		{"a caller over the echo", func(t *testing.T, v *virtualPump, leg *tapLeg) {
			playScripted(t, v, leg, bot, 0, slots-30, echo)
			playScripted(t, v, leg, bot, slots-30, slots, func(k int) []byte { return caller[k] })
		}, echoprobe.VerdictNotEcho, `"isEchoLikely":false`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			leg := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
			v := startVirtualPumpWith(t, leg, leg.frames, provider.OpenAIProfile(), nil)
			awaitTap(t, leg)
			tt.play(t, v, leg)
			if got := echoVerdictNow(v); got != tt.want {
				t.Fatalf("the measurement says %s before the interruption, want %s", got, tt.want)
			}

			v.session.mu.Lock()
			v.session.isBotSpeaking = true
			v.session.mu.Unlock()
			v.session.bargeIn(provider.InterruptReasonSpeech)

			// The interruption happened exactly as before (the provider was
			// told), and the line carries the verdict only when there is one:
			// with no bot audio to compare with, there is nothing to judge, and
			// that is not "not echo".
			if got := len(v.model.recordedInterrupts()); got != 1 {
				t.Fatalf("%d interrupts sent to the provider, want 1", got)
			}
			line := floorLine(t, v, "the caller took the floor back")
			switch {
			case tt.attr == "" && strings.Contains(line, `"isEchoLikely"`):
				t.Errorf("interruption line %q carries isEchoLikely without a verdict", line)
			case tt.attr != "" && !strings.Contains(line, tt.attr):
				t.Errorf("interruption line %q lacks %s", line, tt.attr)
			}
			// The peak it was judged from rides along whenever there was one.
			if hasPeak := strings.Contains(line, `"echoCorrelation":`); hasPeak != (tt.attr != "") {
				t.Errorf("interruption line %q: echoCorrelation present = %v", line, hasPeak)
			}
			if tt.want == echoprobe.VerdictEcho && !strings.Contains(line, `"echoLagMs":400`) {
				t.Errorf("interruption line %q does not name the 400 ms delay", line)
			}
		})
	}
}

// An engine with turn detection of its own cuts the bot by itself and says so
// afterwards; the session never barged in. Its SPEECH cut carries the verdict
// as the session's own does, and is otherwise the cut it always was. A cut for
// any other reason was not speech, and carries none.
func TestTheProvidersOwnSpeechCutReportsWhetherItIsEchoLikelyAndStillCutsTheBot(t *testing.T) {
	const slots = 160
	bot := scriptedBot(slots)
	for _, tt := range []struct {
		reason       provider.InterruptReason
		hasVerdict   bool
		isCallersCut bool // a BARGE_IN is published: SYSTEM is a line pre-empting a turn
	}{
		{provider.InterruptReasonSpeech, true, true},
		{provider.InterruptReasonDTMF, false, true},
		{provider.InterruptReasonSystem, false, false},
	} {
		t.Run(string(tt.reason), func(t *testing.T) {
			leg := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
			v := startVirtualPumpWith(t, leg, leg.frames, provider.OpenAIProfile(), nil)
			awaitTap(t, leg)
			// The uplink is the bot's own echo, so a verdict is known.
			playScripted(t, v, leg, bot, 0, slots, echoOf(bot))
			if got := echoVerdictNow(v); got != echoprobe.VerdictEcho {
				t.Fatalf("the measurement says %s before the cut, want ECHO", got)
			}

			// Eight frames go out and three are still queued when the engine
			// cuts, so five of them (100 ms) were heard.
			leg.holdFrames(3)
			v.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
			v.model.events <- provider.Event{
				Type:  provider.EventTypeAudioDelta,
				Audio: make([]byte, media.FrameSamples*8),
			}
			v.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: tt.reason}

			// The cut happened exactly as before: published as the provider's,
			// the turn ended cut short, the queue flushed, the provider told
			// what was heard.
			if tt.isCallersCut {
				cut := awaitBridgeEvent(t, v.session, EventTypeBargeIn)
				if !cut.IsByProvider || cut.Text != string(tt.reason) {
					t.Errorf("cut = %+v, want the provider's %s cut", cut, tt.reason)
				}
			}
			if done := awaitBridgeEvent(t, v.session, EventTypeTurnDone); !done.IsInterrupted {
				t.Error("want the turn reported as cut short")
			}
			if leg.clears() == 0 {
				t.Error("playback continued after the provider abandoned the turn")
			}
			if _, isSpeaking := v.session.audioProgress(); isSpeaking {
				t.Error("the turn still has audio queued after the cut")
			}
			if got := v.model.recordedInterrupts(); len(got) != 1 ||
				got[0] != (interrupt{tt.reason, 5 * frameDurationMs}) {
				t.Fatalf("interrupts = %+v, want one %s reporting %d ms heard",
					got, tt.reason, 5*frameDurationMs)
			}

			if strings.Contains(v.logs.String(), `"msg":"the caller took the floor back"`) {
				t.Fatalf("the session barged in itself:\n%s", v.logs.String())
			}
			line := floorLine(t, v, "the provider took the floor back")
			if !strings.Contains(line, `"reason":"`+string(tt.reason)+`"`) {
				t.Errorf("interruption line %q does not name %s", line, tt.reason)
			}
			switch {
			case tt.hasVerdict && !strings.Contains(line, `"isEchoLikely":true`):
				t.Errorf("interruption line %q lacks isEchoLikely", line)
			case !tt.hasVerdict && strings.Contains(line, `"isEchoLikely"`):
				t.Errorf("a %s cut carries isEchoLikely: %q", tt.reason, line)
			}
		})
	}
}

// BenchmarkUplinkClockWithEchoProbe is the uplink pump's slot with the echo
// measurement attached and the RTP send loop's tap feeding it: the whole added
// per-frame path. It must stay at 0 allocs/op.
func BenchmarkUplinkClockWithEchoProbe(b *testing.B) {
	leg := &tapLeg{fakeLeg: newFakeLeg(media.LawMu)}
	session, err := New(leg, nopModel{}, provider.OpenAIProfile(), Config{})
	if err != nil {
		b.Fatal(err)
	}
	epoch := time.Unix(1_000_000, 0)
	clock := newUplinkClock(epoch)
	probe := session.startEchoProbe(clock.start)
	converted := make([]byte, 0, media.FrameSamples*8)
	ref := make([]byte, media.FrameSamples)
	frame := make([]byte, media.FrameSamples)
	for i := range ref {
		ref[i] = byte(i * 7)
		frame[i] = byte(i * 5)
	}
	now := epoch
	b.ReportAllocs()
	for b.Loop() {
		leg.tick(ref, now)
		clock.push(frame)
		now = now.Add(uplinkSlot)
		// sendDueSlots minus the pool return, whose boxing of the slice
		// header is the existing benchmarks' business, not the probe's.
		for n := clock.owed(now); n > 0; n-- {
			f := clock.pop()
			probe.FeedUplink(f)
			converted, _ = session.feedSlot(clock, f, converted)
		}
	}
}
