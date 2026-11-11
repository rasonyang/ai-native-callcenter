// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"bytes"
	"log/slog"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// The suppressor's tests run on synthetic speech: voiced syllables on a
// gliding pitch, never recorded calls.

// voice is one synthetic talker.
type voice struct {
	f0      float64 // Hz, the middle of the glide
	levelDb float64 // RMS of a voiced stretch, dBFS
	seed    int64
}

var (
	botVoice    = voice{f0: 190, levelDb: -20, seed: 1}
	callerVoice = voice{f0: 120, levelDb: -20, seed: 2}
)

// speak renders n samples of v: syllables of 100 to 300 ms separated by gaps
// of gapMin to gapMax ms. A gap of zero makes continuous speech.
func (v voice) speak(n int, gapMin, gapMax int) []float64 {
	r := rand.New(rand.NewSource(v.seed))
	out := make([]float64, n)
	amp := math.Pow(10, v.levelDb/20) * 32768 * math.Sqrt2 / 1.25
	phase := 0.0
	for at := 0; at < n; {
		syl := (100 + r.Intn(200)) * 8
		f0 := v.f0 * (0.85 + 0.3*r.Float64())
		glide := (r.Float64() - 0.5) * 0.4 * f0
		for i := 0; i < syl && at+i < n; i++ {
			x := float64(i) / float64(syl)
			f := f0 + glide*x
			phase += 2 * math.Pi * f / 8000
			// A rough vowel: falling harmonics; the syllable starts within
			// 15 ms, as a consonant releases into a vowel, and fades out.
			var s float64
			for h := 1; h <= 12; h++ {
				s += math.Sin(float64(h)*phase) / float64(h)
			}
			env := min(1, float64(i)/120) * 0.5 * (1 + math.Cos(math.Pi*x))
			out[at+i] = amp * s * (0.4 + 0.6*env)
			if i < 120 {
				out[at+i] = amp * s * float64(i) / 120
			}
		}
		at += syl
		if gapMax > 0 {
			at += (gapMin + r.Intn(gapMax-gapMin+1)) * 8
		}
	}
	return out
}

// phrased cuts x into phrases of phraseMs separated by pauses of pauseMs.
func phrased(x []float64, phraseMs, pauseMs int) []float64 {
	period := (phraseMs + pauseMs) * 8
	for i := range x {
		if i%period >= phraseMs*8 {
			x[i] = 0
		}
	}
	return x
}

// returnPath returns x delayed by delayMs, attenuated by lossDb and lightly
// low-passed, as a loudspeaker and a microphone return it.
func returnPath(x []float64, delayMs int, lossDb float64) []float64 {
	g := math.Pow(10, -lossDb/20)
	d := delayMs * 8
	out := make([]float64, len(x))
	for i := range out {
		var s float64
		for k, c := range [...]float64{0.55, 0.3, 0.15} {
			if j := i - d - k; j >= 0 {
				s += c * x[j]
			}
		}
		out[i] = g * s
	}
	return out
}

// lineNoise is a -70 dBFS noise floor.
func lineNoise(n int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	sd := math.Pow(10, -70.0/20) * 32768
	for i := range out {
		out[i] = r.NormFloat64() * sd
	}
	return out
}

func mix(parts ...[]float64) []float64 {
	out := make([]float64, len(parts[0]))
	for _, p := range parts {
		for i := range out {
			out[i] += p[i]
		}
	}
	return out
}

func toPCM(x []float64) []int16 {
	out := make([]int16, len(x))
	for i, v := range x {
		out[i] = int16(max(-32768, min(32767, math.Round(v))))
	}
	return out
}

// suppressRun is a call played through a suppressing Monitor slot by slot, as
// the uplink pump plays it: the reference pushed for the slot, the slot fed to
// the measurement, then Suppress.
type suppressRun struct {
	m *Monitor
	// in and out are the uplink frames before and after Suppress, as G.711;
	// open is the gate after each slot.
	in, out [][]byte
	open    []bool
	changed []bool
}

// runSuppressed plays bot (the reference, nil for none) and up (the uplink)
// through a fresh Monitor; before, when set, runs before each slot k.
func runSuppressed(t testing.TB, law media.Law, bot, up []int16, adjust func(m *Monitor), before func(m *Monitor, k int)) *suppressRun {
	t.Helper()
	start := time.Unix(1_000_000, 0)
	m := New(Config{Start: start, Law: law, Suppress: true, Logger: slog.New(slog.DiscardHandler)})
	if adjust != nil {
		adjust(m)
	}
	r := &suppressRun{m: m}
	slots := len(up) / frameSamples
	ref := make([]byte, 0, frameSamples)
	for k := 0; k < slots; k++ {
		if before != nil {
			before(m, k)
		}
		var refFrame []byte
		if bot != nil {
			refFrame = law.Encode(ref, bot[k*frameSamples:(k+1)*frameSamples])
			if isSilent(bot[k*frameSamples : (k+1)*frameSamples]) {
				refFrame = nil // the pacer pads: no bot audio in the slot
			}
		}
		m.PushReference(refFrame, start.Add(time.Duration(k)*20*time.Millisecond))
		in := law.Encode(make([]byte, 0, frameSamples), up[k*frameSamples:(k+1)*frameSamples])
		frame := bytes.Clone(in)
		m.FeedUplink(frame)
		changed := m.Suppress(frame)
		r.in, r.out = append(r.in, in), append(r.out, frame)
		r.open = append(r.open, m.sup.isOpen)
		r.changed = append(r.changed, changed)
	}
	return r
}

func isSilent(x []int16) bool {
	for _, v := range x {
		if v != 0 {
			return false
		}
	}
	return true
}

// frameDb is the level of one G.711 frame, decoded.
func frameDb(law media.Law, frame []byte) float64 {
	return levelDb(law.Decode(make([]int16, 0, frameSamples), frame))
}

// speechStarts counts where a plain energy detector (four consecutive slots
// above -40 dBFS) would start an utterance in slots [from, to) of frames: a
// stand-in for the provider's turn detection.
func speechStarts(law media.Law, frames [][]byte, from, to int) (starts []int) {
	run, isIn, quiet := 0, false, 0
	for k := from; k < to; k++ {
		if frameDb(law, frames[k]) > -40 {
			run++
			quiet = 0
		} else {
			run = 0
			quiet++
		}
		if !isIn && run >= 4 {
			isIn = true
			starts = append(starts, k)
		}
		if isIn && quiet >= 25 {
			isIn = false
		}
	}
	return starts
}

func TestASteadyEchoIsSuppressedAtEveryReturnLoss(t *testing.T) {
	const seconds = 20
	n := seconds * 8000
	bot := phrased(botVoice.speak(n, 40, 250), 2500, 800)
	for _, delayMs := range []int{60, 400} {
		for _, lossDb := range []float64{0, 6, 12, 20, 30} {
			up := toPCM(mix(returnPath(bot, delayMs, lossDb), lineNoise(n, 3)))
			r := runSuppressed(t, media.LawMu, toPCM(bot), up, nil, nil)
			// Unprocessed, the echo starts utterances throughout.
			if raw := speechStarts(media.LawMu, r.in, 0, len(r.in)); lossDb <= 12 && len(raw) < 5 {
				t.Fatalf("delay %d ms, loss %.0f dB: the scenario's echo starts only %d utterances", delayMs, lossDb, len(raw))
			}
			before := speechStarts(media.LawMu, r.out, 0, 100)
			after := speechStarts(media.LawMu, r.out, 100, len(r.out))
			// What happens while the gain is being learned (the first 2 s) is
			// reported; after it, the echo must start nothing.
			t.Logf("delay %d ms, loss %2.0f dB: %d utterance starts in the first 2 s, %d after; learned gain %.1f dB",
				delayMs, lossDb, len(before), len(after), r.m.sup.est)
			if len(after) != 0 {
				t.Errorf("delay %d ms, loss %.0f dB: the suppressed echo still starts utterances at slots %v",
					delayMs, lossDb, after)
			}
		}
	}
}

// A far end whose own canceller has not converged at the start of each bot
// line leaks it at full level for half a second, and returns nothing after.
func TestAnOnsetLeakIsSuppressedOnceLearned(t *testing.T) {
	const lines, lineMs, pauseMs, delayMs, leakMs = 10, 2500, 1000, 360, 500
	n := lines * (lineMs + pauseMs) * 8
	speech := botVoice.speak(n, 0, 0)
	bot := make([]float64, n)
	for l := 0; l < lines; l++ {
		at := l * (lineMs + pauseMs) * 8
		copy(bot[at:at+lineMs*8], speech[at:at+lineMs*8])
	}
	// A leak as loud as the line (0 dB), 4 dB louder and 8 dB louder (a far
	// end at full volume). Before the onset gain has learned anything, a
	// slot passes when it is more than 6 dB louder than the loudest bot audio
	// in reach, so only the loudest leak gets through, and only on its first
	// line.
	for _, gainDb := range []float64{0, 4, 8} {
		leak := make([]float64, n)
		echo := returnPath(speech, 0, -gainDb)
		for l := 0; l < lines; l++ {
			at := l * (lineMs + pauseMs) * 8
			for i := 0; i < leakMs*8; i++ {
				if j := at + delayMs*8 + i; j < n {
					leak[j] = echo[at+i]
				}
			}
		}
		up := toPCM(mix(leak, lineNoise(n, 4)))
		r := runSuppressed(t, media.LawMu, toPCM(bot), up, nil, nil)
		for l := 0; l < lines; l++ {
			from := (l*(lineMs+pauseMs) + delayMs) / 20
			to := from + leakMs/20
			passed := 0
			for k := from; k < to; k++ {
				if bytes.Equal(r.out[k], r.in[k]) && frameDb(media.LawMu, r.in[k]) > -40 {
					passed++
				}
			}
			raw := speechStarts(media.LawMu, r.in, from, to+10)
			starts := speechStarts(media.LawMu, r.out, from, to+10)
			t.Logf("leak %+.0f dB, line %d: %d of %d leak slots passed, %d utterance starts (%d unprocessed)",
				gainDb, l+1, passed, to-from, len(starts), len(raw))
			if len(raw) == 0 {
				t.Fatalf("leak %+.0f dB, line %d: the leak starts nothing even unprocessed", gainDb, l+1)
			}
			if (l >= 1 || gainDb <= 4) && (passed > 2 || len(starts) > 0) {
				t.Errorf("leak %+.0f dB, line %d: %d leak slots passed (%d starts)", gainDb, l+1, passed, len(starts))
			}
		}
	}
}

// openWithin reports the first slot from from on whose frame reached the
// provider unchanged, or -1.
func (r *suppressRun) firstPassed(from, to int) int {
	for k := from; k < to; k++ {
		if !r.changed[k] && r.open[k] {
			return k
		}
	}
	return -1
}

// callerOver plays caller, who starts speaking at callerAt (seconds) and talks
// for 3 s over a bot that speaks the whole time, on an uplink that carries
// echo (nil for none).
func callerOver(seconds int, callerAt float64, caller voice, echo func(bot []float64) []float64) (bot, up []int16, start, end int) {
	n := seconds * 8000
	b := botVoice.speak(n, 40, 250)
	c := caller.speak(n, 30, 120)
	start = int(callerAt * 50)
	end = start + 150
	for i := range c {
		if i < start*frameSamples || i >= end*frameSamples {
			c[i] = 0
		}
	}
	parts := [][]float64{c, lineNoise(n, 5)}
	if echo != nil {
		parts = append(parts, echo(b))
	}
	// Start the caller on a syllable: the first slot with sound in it.
	for start < end && isSilent(toPCM(c[start*frameSamples:(start+1)*frameSamples])) {
		start++
	}
	return toPCM(b), toPCM(mix(parts...)), start, end
}

// assertCallerPasses checks that the caller's speech reaches the provider
// from at most two slots after it starts, untouched while it lasts.
func assertCallerPasses(t *testing.T, r *suppressRun, start, end int) {
	t.Helper()
	first := r.firstPassed(start, end)
	if first < 0 || first > start+2 {
		t.Fatalf("the caller starting at slot %d first reached the provider unchanged at slot %d", start, first)
	}
	// From then on, every slot in which the caller is audible passes as it
	// came: the gate holds across the gaps between syllables.
	for k := first; k < end; k++ {
		if frameDb(media.LawMu, r.in[k]) > -45 && !bytes.Equal(r.out[k], r.in[k]) {
			t.Fatalf("caller slot %d (%.1f dBFS) was changed after the gate opened at %d", k,
				frameDb(media.LawMu, r.in[k]), first)
		}
	}
	t.Logf("caller from slot %d: first passed unchanged at slot %d (%d slots of onset lost)", start, first, first-start)
}

func TestARealCallerOverNoEchoPassesAfterAtMostTwoSlots(t *testing.T) {
	// The bot has been speaking for 8 s with nothing coming back: the learned
	// gain has decayed, and a caller 6 dB below the bot opens the gate.
	quieter := callerVoice
	quieter.levelDb = botVoice.levelDb - 6
	bot, up, start, end := callerOver(12, 8, quieter, nil)
	r := runSuppressed(t, media.LawMu, bot, up, nil, nil)
	assertCallerPasses(t, r, start, end)
}

// Over a steady echo the gate opens for a caller louder than the echo the call
// has learned by the margin: with a 12 dB return loss, one within about 6 dB
// of the bot's loudest recent 10 ms. A quieter caller is not heard until the
// bot stops (a known cost).
func TestARealCallerOverASteadyEchoOpensTheGate(t *testing.T) {
	louder := callerVoice
	louder.levelDb = botVoice.levelDb + 4
	bot, up, start, end := callerOver(12, 6, louder, func(b []float64) []float64 { return returnPath(b, 400, 12) })
	r := runSuppressed(t, media.LawMu, bot, up, nil, nil)
	// The echo alone was being suppressed before the caller spoke.
	if s := speechStarts(media.LawMu, r.out, 100, start); len(s) > 0 {
		t.Fatalf("the echo before the caller started utterances at %v", s)
	}
	assertCallerPasses(t, r, start, end)
}

// The study's fast learner learned a caller as echo: at a caller's onset after
// a near-silent uplink, the 80 ms window's energy sits in its last few
// milliseconds, which match some lag of the bot's among thousands by chance,
// and one such slot raised the learned gain to the caller's own level. Here
// the onset is made to match exactly (its first 20 ms are the bot's own
// waveform 300 ms earlier, as loud as the caller), the worst case of that
// chance; the suppressor's guards (two slots agreeing on the lag, at most 6
// dB a slot, never above -3 dB) must keep the caller passing.
func TestTheCallersOwnVoiceIsNotLearnedAsEcho(t *testing.T) {
	bot, up, start, end := callerOver(10, 6, callerVoice, nil)
	// The loudest slot of the bot's in reach, as loud as the caller's own
	// first slot, replaces it.
	at := start * frameSamples
	from := at - 600*8
	for lag := 200; lag <= 600; lag += 20 {
		if b := at - lag*8; levelDb(bot[b:b+frameSamples]) > levelDb(bot[from:from+frameSamples]) {
			from = b
		}
	}
	copy(up[at:at+frameSamples], bot[from:from+frameSamples])

	r := runSuppressed(t, media.LawMu, bot, up, nil, nil)
	assertCallerPasses(t, r, start, end)
	if r.m.sup.est > -20 {
		t.Errorf("learned echo gain %.1f dB after the caller spoke on a call with no echo", r.m.sup.est)
	}

	// The same call with the fast learner's rules: the chance match is
	// learned, and the caller is held back. This is what the guards prevent.
	fast := runSuppressed(t, media.LawMu, bot, up, func(m *Monitor) {
		m.sup.p.stableSlots, m.sup.p.attackDb, m.sup.p.ceilingDb, m.sup.p.learnCorr = 1, math.Inf(1), math.Inf(1), 0.6
	}, nil)
	if first := fast.firstPassed(start, end); first >= 0 && first <= start+10 {
		t.Errorf("the fast learner let the caller through at slot %d: the scenario no longer provokes the failure", first)
	}
}

func TestWithNoBotAudioEveryFramePassesBitExact(t *testing.T) {
	for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
		n := 10 * 8000
		r0 := rand.New(rand.NewSource(9))
		up := make([]int16, n)
		for i := range up {
			up[i] = int16(r0.Intn(65536) - 32768)
		}
		// A caller talking, then line noise, then every G.711 byte value.
		copy(up, toPCM(callerVoice.speak(n/2, 30, 120)))
		r := runSuppressed(t, law, nil, up, nil, nil)
		for k := range r.in {
			if r.changed[k] || !bytes.Equal(r.in[k], r.out[k]) {
				t.Fatalf("%s: slot %d changed with no bot audio", law, k)
			}
		}
		if r.m.sup.examined != 0 || r.m.sup.correlated != 0 {
			t.Errorf("%s: %d slots examined, %d correlated with no bot audio", law, r.m.sup.examined, r.m.sup.correlated)
		}
	}
}

func TestADelayWrongByFortyMillisecondsStillSuppresses(t *testing.T) {
	const delayMs = 400
	n := 20 * 8000
	bot := phrased(botVoice.speak(n, 40, 250), 2500, 800)
	up := toPCM(mix(returnPath(bot, delayMs, 6), lineNoise(n, 6)))
	for _, errMs := range []int{-40, 40} {
		known := (delayMs + errMs) / binMs
		r := runSuppressed(t, media.LawMu, toPCM(bot), up, nil, func(m *Monitor, _ int) {
			// The measurement holds a delay errMs off for the whole call.
			m.track.isFound, m.track.lagBins = true, known
		})
		if !r.m.sup.isHinted || r.m.sup.fromMs != known*binMs-80 {
			t.Fatalf("error %+d ms: the window was %d-%d ms (from the delay: %v)", errMs, r.m.sup.fromMs, r.m.sup.toMs, r.m.sup.isHinted)
		}
		if raw := speechStarts(media.LawMu, r.in, 0, len(r.in)); len(raw) < 5 {
			t.Fatalf("the scenario's echo starts only %d utterances", len(raw))
		}
		if after := speechStarts(media.LawMu, r.out, 100, len(r.out)); len(after) != 0 {
			t.Errorf("delay known %+d ms off: the echo still starts utterances at slots %v", errMs, after)
		}
	}
}

// failLog counts the suppressor's failure lines.
type failLog struct {
	mu    sync.Mutex
	lines []string
}

func (f *failLog) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.lines = append(f.lines, string(p))
	f.mu.Unlock()
	return len(p), nil
}

func (f *failLog) count(msg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.lines {
		if strings.Contains(l, msg) {
			n++
		}
	}
	return n
}

func TestAnInternalFailureTurnsSuppressionOffAndPassesTheUplink(t *testing.T) {
	n := 12 * 8000
	bot := botVoice.speak(n, 40, 250)
	up := toPCM(mix(returnPath(bot, 400, 6), lineNoise(n, 7)))
	for _, tt := range []struct {
		name   string
		break_ func(m *Monitor)
	}{
		// A panic inside the correlation (its working memory gone).
		{"panic", func(m *Monitor) { m.sup.xc = nil }},
		// A state the rules cannot reach.
		{"impossible gain", func(m *Monitor) { m.sup.est = math.NaN() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := &failLog{}
			const breakAt = 300
			start := time.Unix(1_000_000, 0)
			m := New(Config{Start: start, Law: media.LawMu, Suppress: true, Logger: slog.New(slog.NewTextHandler(logs, nil))})
			ref := make([]byte, 0, frameSamples)
			gatedBefore, failedAt := 0, -1
			for k := 0; k < len(up)/frameSamples; k++ {
				if k == breakAt {
					tt.break_(m)
				}
				m.PushReference(media.LawMu.Encode(ref, toPCM(bot[k*frameSamples:(k+1)*frameSamples])),
					start.Add(time.Duration(k)*20*time.Millisecond))
				in := media.LawMu.Encode(make([]byte, 0, frameSamples), up[k*frameSamples:(k+1)*frameSamples])
				frame := bytes.Clone(in)
				m.FeedUplink(frame)
				changed := m.Suppress(frame)
				if k < breakAt && changed {
					gatedBefore++
				}
				if failedAt < 0 && m.sup.failed {
					failedAt = k
				}
				if failedAt >= 0 && (changed || !bytes.Equal(frame, in)) {
					t.Fatalf("slot %d changed after the failure at %d", k, failedAt)
				}
			}
			if gatedBefore == 0 {
				t.Fatal("nothing was suppressed before the failure: the test proves nothing")
			}
			// It fails at the first slot that reaches the broken part: the
			// next one with bot audio in reach and a caller loud enough to
			// correlate.
			if failedAt < breakAt || failedAt > breakAt+50 {
				t.Fatalf("broken at slot %d, failed at %d", breakAt, failedAt)
			}
			if got := logs.count("echo suppression disabled for this call"); got != 1 {
				t.Errorf("%d failure lines, want 1:\n%s", got, strings.Join(logs.lines, ""))
			}
			sum := m.Summary()
			if !sum.Suppression.IsOn || !sum.Suppression.IsFailed {
				t.Errorf("summary %+v does not report the failure", sum.Suppression)
			}
			// The measurement goes on: it is not the suppressor's to stop.
			if sum.IsFailed || !sum.IsMeasured {
				t.Errorf("the measurement stopped with the suppressor: %+v", sum)
			}
		})
	}
}

func TestTheGateFadesWhereItOpensAndCloses(t *testing.T) {
	for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
		m := New(Config{Start: time.Unix(1, 0), Law: law, Suppress: true})
		loud := make([]int16, frameSamples)
		for i := range loud {
			loud[i] = 8000
		}
		frame := func() []byte { return law.Encode(make([]byte, 0, frameSamples), loud) }
		decide := func(wasOpen, isOpen bool) []int16 {
			m.sup.wasOpen, m.sup.isOpen, m.sup.decidedBins = wasOpen, isOpen, m.upBins
			f := frame()
			m.Suppress(f)
			return law.Decode(make([]int16, 0, frameSamples), f)
		}
		if got := decide(true, true); got[0] != frameValue(law, 8000) || got[159] != frameValue(law, 8000) {
			t.Errorf("%s: an open gate changed the frame: %d..%d", law, got[0], got[159])
		}
		if got := decide(false, false); !bytes.Equal(law.Encode(nil, got), media.SilenceFrame(law)) {
			t.Errorf("%s: a closed gate is not the law's silence", law)
		}
		in := decide(false, true)
		if abs(int(in[0])) > 16 || in[159] < 7000 || in[40] >= in[120] {
			t.Errorf("%s: opening does not fade in: %d, %d, %d, %d", law, in[0], in[40], in[120], in[159])
		}
		out := decide(true, false)
		if out[0] < 7000 || abs(int(out[159])) > 200 || out[40] <= out[120] {
			t.Errorf("%s: closing does not fade out: %d, %d, %d, %d", law, out[0], out[40], out[120], out[159])
		}
	}
}

func frameValue(law media.Law, v int16) int16 {
	return law.Decode(nil, law.Encode(nil, []int16{v}))[0]
}

func TestSuppressChangesNothingItWasNotAskedTo(t *testing.T) {
	loud := bytes.Repeat([]byte{0x10}, frameSamples)
	// A measurement only.
	plain := New(Config{Start: time.Unix(1, 0), Law: media.LawMu})
	plain.FeedUplink(loud)
	if plain.Suppress(loud) || !bytes.Equal(loud, bytes.Repeat([]byte{0x10}, frameSamples)) {
		t.Error("a Monitor built without Suppress changed a frame")
	}
	if plain.Summary().Suppression.IsOn {
		t.Error("a Monitor built without Suppress reports suppression")
	}
	// A slot that was not judged (Suppress without FeedUplink), and a frame of
	// another size.
	m := New(Config{Start: time.Unix(1, 0), Law: media.LawMu, Suppress: true})
	m.FeedUplink(loud)
	m.sup.isOpen, m.sup.wasOpen = false, false
	m.upBins += 2
	if m.Suppress(loud) {
		t.Error("a slot that was not judged was suppressed")
	}
	m.upBins -= 2
	if m.Suppress(loud[:80]) {
		t.Error("a short frame was suppressed")
	}
}
