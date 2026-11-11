// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// speech makes speech-like audio: white noise under a syllable-rate envelope
// (120-300 ms bursts of varying level, pauses between them and longer gaps
// between phrases). Pure tones would make every lag correlate.
func speech(r *rand.Rand, seconds float64, level float64, talkRatio float64) []float64 {
	n := int(seconds * 8000)
	out := make([]float64, n)
	i := 0
	for i < n {
		if r.Float64() > talkRatio {
			i += int((0.3 + r.Float64()*0.9) * 8000) // a pause between phrases
			continue
		}
		for syl := 3 + r.Intn(6); syl > 0 && i < n; syl-- {
			length := int((0.12 + r.Float64()*0.18) * 8000)
			amp := level * (0.25 + 0.75*r.Float64())
			for k := 0; k < length && i < n; k, i = k+1, i+1 {
				out[i] = amp * math.Sin(math.Pi*float64(k)/float64(length)) * r.NormFloat64()
			}
			i += int((0.02 + r.Float64()*0.08) * 8000)
		}
	}
	return out
}

// path is one scenario: what the bot sent and what came back.
type path struct {
	ref []float64
	up  []float64
}

// echoPath returns the reference and an uplink that holds ref delayed by
// delayMs, scaled by gain, over noise of noiseLevel. A negative delay is no
// echo at all.
func echoPath(r *rand.Rand, seconds float64, delayMs int, gain, noiseLevel float64) path {
	ref := speech(r, seconds, 3000, 0.8)
	up := make([]float64, len(ref))
	for i := range up {
		up[i] = noiseLevel * r.NormFloat64()
		if delayMs >= 0 {
			if j := i - delayMs*8; j >= 0 {
				up[i] += gain * ref[j]
			}
		}
	}
	return path{ref: ref, up: up}
}

func addSpeech(up []float64, from, to float64, caller []float64) {
	for i := int(from * 8000); i < int(to*8000) && i < len(up) && i < len(caller); i++ {
		up[i] += caller[i]
	}
}

func clip(v float64) int16 {
	return int16(math.Max(-32000, math.Min(32000, v)))
}

// drive feeds the scenario slot by slot in virtual time. drop removes frames
// of the uplink (the uplink clock dropping the oldest): from slot dropAt on,
// dropSlots frames are skipped. onSlot is called after every slot.
func drive(m *Monitor, start time.Time, p path, dropAt, dropSlots int, onSlot func(slot int)) {
	law := m.law
	slots := len(p.ref) / frameSamples
	var refBuf, upBuf [frameSamples]int16
	refEnc := make([]byte, 0, frameSamples)
	upEnc := make([]byte, 0, frameSamples)
	upSlot := 0
	for k := 0; k < slots; k++ {
		for i := range refBuf {
			refBuf[i] = clip(p.ref[k*frameSamples+i])
		}
		refEnc = law.Encode(refEnc, refBuf[:])
		m.PushReference(refEnc, start.Add(time.Duration(k)*20*time.Millisecond))

		src := k
		if dropSlots > 0 && k >= dropAt {
			src = k + dropSlots
		}
		if src >= slots {
			break
		}
		for i := range upBuf {
			upBuf[i] = clip(p.up[src*frameSamples+i])
		}
		upEnc = law.Encode(upEnc, upBuf[:])
		m.FeedUplink(upEnc)
		upSlot++
		if onSlot != nil {
			onSlot(k)
		}
	}
}

func newMonitor() (*Monitor, time.Time) {
	start := time.Unix(1_000_000, 0)
	return New(Config{Start: start, Law: media.LawMu}), start
}

func TestDelayIsFoundWithinTolerance(t *testing.T) {
	for _, delayMs := range []int{80, 240, 600} {
		for seed := int64(1); seed <= 3; seed++ {
			r := rand.New(rand.NewSource(seed*100 + int64(delayMs)))
			m, start := newMonitor()
			drive(m, start, echoPath(r, 40, delayMs, 0.25, 30), 0, 0, nil)
			s := m.Summary()
			if !s.IsEchoFound {
				t.Fatalf("delay %d seed %d: echo not found (%+v)", delayMs, seed, s)
			}
			if d := s.DelayMs - delayMs; d < -20 || d > 20 {
				t.Errorf("delay %d seed %d: estimated %d ms", delayMs, seed, s.DelayMs)
			}
			// 0.25 is 12 dB; the noise floor and quantisation bend it a little.
			if s.ReturnLossDb < 9 || s.ReturnLossDb > 15 {
				t.Errorf("delay %d seed %d: return loss %.1f dB, want about 12", delayMs, seed, s.ReturnLossDb)
			}
			if s.Estimates < 3 || s.EstimatesFoundEcho < 3 || s.FinalRunEstimates < 3 {
				t.Errorf("delay %d seed %d: %d estimates, %d found an echo, a final run of %d",
					delayMs, seed, s.Estimates, s.EstimatesFoundEcho, s.FinalRunEstimates)
			}
		}
	}
}

func TestFirstEstimateRunsDuringTheOpeningLine(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	m, start := newMonitor()
	var firstAt int
	drive(m, start, echoPath(r, 20, 240, 0.25, 30), 0, 0, func(slot int) {
		if firstAt == 0 && m.track.estimates > 0 {
			firstAt = slot
		}
	})
	if firstAt == 0 || time.Duration(firstAt)*20*time.Millisecond > 6*time.Second {
		t.Errorf("first estimate at slot %d (%v)", firstAt, time.Duration(firstAt)*20*time.Millisecond)
	}
}

func TestNoEchoIsNotFound(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		r := rand.New(rand.NewSource(seed))
		m, start := newMonitor()
		p := echoPath(r, 60, -1, 0, 30)
		// A caller speaking now and then, unrelated to the bot.
		addSpeech(p.up, 5, 55, speech(r, 60, 1500, 0.5))
		drive(m, start, p, 0, 0, nil)
		s := m.Summary()
		if s.IsEchoFound {
			t.Fatalf("seed %d: made up an echo: %+v", seed, s)
		}
		if s.Estimates == 0 {
			t.Fatalf("seed %d: no estimate ran", seed)
		}
		if v := m.Judge().Verdict; v == VerdictEcho {
			t.Errorf("seed %d: verdict %s without an echo", seed, v)
		}
	}
}

func TestSilentUplinkIsNotFound(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	m, start := newMonitor()
	drive(m, start, echoPath(r, 30, -1, 0, 0), 0, 0, nil)
	if s := m.Summary(); s.IsEchoFound {
		t.Fatalf("found an echo on a silent uplink: %+v", s)
	}
}

func TestDoubleTalkDoesNotMoveTheDelay(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	m, start := newMonitor()
	p := echoPath(r, 60, 240, 0.25, 30)
	// The caller talks loudly over the bot for 15 s in the middle.
	addSpeech(p.up, 20, 35, speech(r, 60, 2500, 0.7))
	var bad []int
	drive(m, start, p, 0, 0, func(slot int) {
		if m.track.isFound && abs(m.track.lagBins-24) > agreeBins {
			bad = append(bad, m.track.lagBins*10)
		}
	})
	if len(bad) > 0 {
		t.Fatalf("published delay left 240 ms: %v", bad)
	}
	if s := m.Summary(); !s.IsEchoFound || abs(s.DelayMs-240) > 20 {
		t.Fatalf("summary %+v", s)
	}
}

func TestDelayChangesOnlyAfterThreeAgreeingEstimates(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	m, start := newMonitor()
	// 240 ms for 30 s, then 400 ms.
	a := echoPath(r, 80, 240, 0.25, 30)
	for i := 30 * 8000; i < len(a.up); i++ {
		a.up[i] = 30 * r.NormFloat64()
		if j := i - 400*8; j >= 0 {
			a.up[i] += 0.25 * a.ref[j]
		}
	}
	changeSlot := 30 * 50
	var published []int // delay in ms each time it changes
	lastPub, lastEst := -1, 0
	var runAtChange int
	drive(m, start, a, 0, 0, func(slot int) {
		if m.track.estimates != lastEst {
			lastEst = m.track.estimates
		}
		pub := -1
		if m.track.isFound {
			pub = m.track.lagBins * 10
		}
		if pub != lastPub {
			published = append(published, pub)
			lastPub = pub
			if slot > changeSlot {
				runAtChange = m.track.runCount
			}
		}
	})
	if len(published) < 2 || published[len(published)-1] < 380 || published[len(published)-1] > 420 {
		t.Fatalf("published delays %v, want to end near 400", published)
	}
	for _, d := range published[1:] {
		if d != 400 && abs(d-240) > 20 && abs(d-400) > 20 {
			t.Errorf("published a delay that is neither path: %v", published)
		}
	}
	if runAtChange < confirmAfter {
		t.Errorf("delay changed after a run of %d agreeing estimates, want %d", runAtChange, confirmAfter)
	}
}

func TestTheUplinkClockDroppingFramesMovesTheDelayByTheDroppedTime(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	m, start := newMonitor()
	p := echoPath(r, 80, 300, 0.25, 30)
	// 100 ms of the uplink disappears at 30 s: the echo then arrives 100 ms
	// sooner relative to the reference.
	drive(m, start, p, 30*50, 5, nil)
	s := m.Summary()
	if !s.IsEchoFound || abs(s.DelayMs-200) > 20 {
		t.Fatalf("after a 100 ms drop want about 200 ms, got %+v", s)
	}
}

func TestTheVerdictTellsEchoFromALoudCaller(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	m, start := newMonitor()
	p := echoPath(r, 50, 240, 0.25, 30)
	// 40 s on: the caller speaks over the bot, louder than any echo.
	addSpeech(p.up, 40, 50, speech(r, 50, 6000, 0.9))
	// Judged every 100 ms, where the window holds the echo of bot speech (an
	// uplink of line noise alone interrupts nothing).
	var echoes, echoJudged, likelyDuringCaller, hadCaller int
	drive(m, start, p, 0, 0, func(slot int) {
		sec := float64(slot) * 0.02
		if slot%5 != 0 {
			return
		}
		switch {
		case sec > 30 && sec < 38 && isSpeaking(p.ref, sec-0.24-0.3, sec-0.24):
			if v := m.Judge().Verdict; v != VerdictUnknown {
				echoJudged++
				if v == VerdictEcho {
					echoes++
				}
			}
		case sec > 41 && sec < 49:
			if v := m.Judge().Verdict; v != VerdictUnknown {
				hadCaller++
				if v == VerdictEcho {
					likelyDuringCaller++
				}
			}
		}
	})
	if echoJudged < 40 || echoes < echoJudged*95/100 {
		t.Errorf("echo judged ECHO at %d of %d moments of the echo stretch", echoes, echoJudged)
	}
	if hadCaller < 60 || likelyDuringCaller > hadCaller/10 {
		t.Errorf("a loud caller judged ECHO at %d of %d moments", likelyDuringCaller, hadCaller)
	}
}

// isSpeaking reports whether signal has speech between from and to seconds.
func isSpeaking(signal []float64, from, to float64) bool {
	var sum float64
	n := 0
	for i := int(from * 8000); i < int(to*8000) && i < len(signal); i++ {
		if i >= 0 {
			sum += math.Abs(signal[i])
			n++
		}
	}
	return n > 0 && sum/float64(n) >= activeLevel
}

func TestAMonitorWithoutAReferenceIsNotMeasured(t *testing.T) {
	m, _ := newMonitor()
	frame := make([]byte, frameSamples)
	for i := 0; i < 2000; i++ {
		m.FeedUplink(frame)
	}
	if s := m.Summary(); s.IsMeasured || s.IsEchoFound {
		t.Fatalf("%+v", s)
	}
}

func TestReferenceJitterDoesNotShiftTheDelay(t *testing.T) {
	r := rand.New(rand.NewSource(21))
	m, start := newMonitor()
	p := echoPath(r, 40, 240, 0.25, 30)
	// Push the reference up to 6 ms off its slot, as a busy pacer would.
	law := m.law
	var refBuf, upBuf [frameSamples]int16
	refEnc, upEnc := make([]byte, 0, frameSamples), make([]byte, 0, frameSamples)
	for k := 0; k < len(p.ref)/frameSamples; k++ {
		for i := range refBuf {
			refBuf[i] = clip(p.ref[k*frameSamples+i])
			upBuf[i] = clip(p.up[k*frameSamples+i])
		}
		jitter := time.Duration(r.Intn(13)-6) * time.Millisecond
		m.PushReference(law.Encode(refEnc, refBuf[:]), start.Add(time.Duration(k)*20*time.Millisecond+jitter))
		m.FeedUplink(law.Encode(upEnc, upBuf[:]))
	}
	if s := m.Summary(); !s.IsEchoFound || abs(s.DelayMs-240) > 20 {
		t.Fatalf("%+v", s)
	}
}

func TestAFailingMeasurementDisablesItselfOnce(t *testing.T) {
	m, _ := newMonitor()
	m.fail("boom")
	m.FeedUplink(make([]byte, frameSamples))
	if s := m.Summary(); !s.IsFailed || s.IsMeasured || m.Judge().Verdict != VerdictUnknown {
		t.Fatalf("%+v", s)
	}
}

func TestPushReferenceDropsTheOldestWhenFull(t *testing.T) {
	m, start := newMonitor()
	frame := make([]byte, frameSamples)
	for i := 0; i < pendingFrames+5; i++ {
		m.PushReference(frame, start.Add(time.Duration(i)*20*time.Millisecond))
	}
	if m.txCount != pendingFrames || m.txDropped != 5 {
		t.Fatalf("count %d dropped %d", m.txCount, m.txDropped)
	}
}

// cutBot is a call on which the bot is cut off again and again from its first
// words: lines of 0.5 to 0.8 s, 1.5 to 2.5 s apart, from 0.8 s on, the way an
// echo that the provider takes for the caller cuts it. The echo comes back
// delayMs later at gainAt(seconds) over a quiet line. cuts are the moments the
// lines ended.
func cutBot(r *rand.Rand, seconds float64, delayMs int, gainAt func(sec float64) float64) (p path, cuts []float64) {
	n := int(seconds * 8000)
	p = path{ref: make([]float64, n), up: make([]float64, n)}
	for at := 0.8; at < seconds-3; {
		length := 0.5 + 0.3*r.Float64()
		for i, v := range speech(r, length, 3000, 1.0) {
			p.ref[int(at*8000)+i] = v
		}
		cuts = append(cuts, at+length)
		at += length + 1.5 + r.Float64()
	}
	for i := range p.up {
		p.up[i] = 30 * r.NormFloat64()
		if j := i - delayMs*8; j >= 0 {
			p.up[i] += gainAt(float64(i)/8000) * p.ref[j]
		}
	}
	return p, cuts
}

// verdictsAfterCutsWith drives p and asks for the verdict 200 ms after each
// cut, when a provider's turn detection has reacted to the echo. onSlot, when
// set, runs after every slot.
func verdictsAfterCutsWith(m *Monitor, start time.Time, p path, cuts []float64, onSlot func(slot int)) []Verdict {
	var out []Verdict
	drive(m, start, p, 0, 0, func(slot int) {
		if onSlot != nil {
			onSlot(slot)
		}
		if len(out) < len(cuts) && float64(slot)*0.02 >= cuts[len(out)]+0.2 {
			out = append(out, m.Judge().Verdict)
		}
	})
	return out
}

// The calls the measurement matters most on are those where the echo cuts the
// bot off within its first second, every time it speaks: the bot then never
// says two seconds of anything. The waveform verdict needs no delay known in
// advance, so even the first cut is judged; and the estimates, which need one
// for the call-end line, know it by the second: the echo of each cut-off line
// keeps arriving after the bot has stopped, and a loud echo shows its delay
// from the first half second of it.
func TestAnEchoPathIsKnownWithinTheFirstLinesOfABotThatKeepsBeingCut(t *testing.T) {
	var verdicts []Verdict
	for seed := int64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		m, start := newMonitor()
		p, cuts := cutBot(r, 30, 400, func(float64) float64 { return 0.5 })
		var candidateAt float64
		got := verdictsAfterCutsWith(m, start, p, cuts, func(slot int) {
			if candidateAt == 0 && m.track.hasLast && m.track.lastSeen.isFound {
				candidateAt = float64(slot) * 0.02
			}
		})
		if candidateAt == 0 || candidateAt > cuts[1]+0.2 {
			t.Errorf("seed %d: no estimate found the delay by the second cut (%.1f s): first at %.1f s",
				seed, cuts[1], candidateAt)
		}
		verdicts = append(verdicts, got...)
		if s := m.Summary(); !s.IsEchoFound || abs(s.DelayMs-400) > 20 {
			t.Errorf("seed %d: %+v", seed, s)
		}
	}
	echoes := 0
	for _, v := range verdicts {
		if v == VerdictEcho {
			echoes++
		}
	}
	if echoes < len(verdicts)*95/100 {
		t.Errorf("%d of %d cuts by the echo judged ECHO, want nearly all", echoes, len(verdicts))
	}
}

// A far end's return loss changes within a call. Here it starts at 6 dB, falls
// to 22 dB for half a minute (which is what gets published) and comes back at
// 10 dB: an echo 10 dB louder than the published return loss is still the
// bot's own echo. The published value is an average; the verdict never
// holds an interruption against it.
func TestAnEchoLouderThanThePublishedReturnLossIsStillEcho(t *testing.T) {
	gainAt := func(sec float64) float64 {
		switch {
		case sec < 10:
			return 0.5
		case sec < 40:
			return 0.08
		}
		return 0.3
	}
	for seed := int64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		m, start := newMonitor()
		p, cuts := cutBot(r, 60, 400, gainAt)
		var publishedLossDb float64
		var late []Verdict
		drive(m, start, p, 0, 0, func(slot int) {
			sec := float64(slot) * 0.02
			if sec < 40.5 {
				publishedLossDb = returnLossDb(m.track.gain)
			}
			if len(cuts) > 0 && sec >= cuts[0]+0.2 {
				if cuts[0] > 40.5 {
					late = append(late, m.Judge().Verdict)
				}
				cuts = cuts[1:]
			}
		})
		if publishedLossDb < 16 {
			t.Fatalf("seed %d: published %.1f dB entering the louder part, want the quiet middle's", seed, publishedLossDb)
		}
		for i, v := range late {
			if v != VerdictEcho {
				t.Errorf("seed %d: late cut %d judged %s, want ECHO (published %.1f dB)", seed, i, v, publishedLossDb)
			}
		}
		// And the call-end line shows the range, not only the published value.
		if s := m.Summary(); s.ReturnLossMinDb > 7.5 || s.ReturnLossMaxDb < 19 || s.EstimatesFoundEcho < 3 {
			t.Errorf("seed %d: %+v", seed, s)
		}
	}
}

// A caller who starts speaking over the bot is the interruption the verdict
// must not call echo. With no echo on the line the caller's waveform does not
// follow the bot's at any delay. Over an echo it is the hard case: 400 ms
// into a caller as loud as the bot (about when a provider's turn detection has
// cut in and the flush has run), part of the window is still the echo alone,
// and while the caller is quiet the echo still dominates it. The same moments
// without the caller are all echo, so it is the caller that is told apart.
func TestACallerSpeakingOverTheBotIsNotEcho(t *testing.T) {
	type scenario struct {
		name       string
		echoGain   float64
		withCaller bool
	}
	counts := map[string]map[Verdict]int{}
	for _, sc := range []scenario{
		{"caller over no echo", 0, true},
		{"caller over a 10 dB echo", 0.3, true},
		{"the 10 dB echo alone", 0.3, false},
	} {
		counts[sc.name] = map[Verdict]int{}
		for seed := int64(1); seed <= 20; seed++ {
			r := rand.New(rand.NewSource(seed))
			m, start := newMonitor()
			ref := speech(r, 60, 3000, 1.0) // the bot talks throughout
			p := path{ref: ref, up: make([]float64, len(ref))}
			for i := range p.up {
				p.up[i] = 30 * r.NormFloat64()
				if j := i - 240*8; j >= 0 {
					p.up[i] += sc.echoGain * ref[j]
				}
			}
			var asks []float64
			for at := 20.0; at < 58; at += 10 {
				if sc.withCaller {
					caller := append(make([]float64, int(at*8000)), speech(r, 1.5, 3000, 1.0)...)
					addSpeech(p.up, at, at+1.5, caller)
				}
				asks = append(asks, at+0.4)
			}
			drive(m, start, p, 0, 0, func(slot int) {
				if len(asks) > 0 && float64(slot)*0.02 >= asks[0] {
					counts[sc.name][m.Judge().Verdict]++
					asks = asks[1:]
				}
			})
		}
	}
	if got := counts["caller over no echo"]; got[VerdictNotEcho] != 80 {
		t.Errorf("a caller over no echo: %v, want 80 NOT_ECHO", got)
	}
	if got := counts["the 10 dB echo alone"]; got[VerdictEcho] != 80 {
		t.Errorf("the echo alone: %v, want 80 ECHO", got)
	}
	// 73 of 80 when this was written; 300 ms into the caller it is 60.
	if got := counts["caller over a 10 dB echo"]; got[VerdictNotEcho] < 70 {
		t.Errorf("a caller over the echo: %v, want at least 70 NOT_ECHO", got)
	}
}

// A device with a canceller of its own lets the bot's voice through for a
// moment when each line starts and suppresses the rest. Over seconds there is
// too little of it for an estimate to find a delay; the waveform of the burst
// is the bot's all the same.
func TestALeakingCancellersBurstsAreEcho(t *testing.T) {
	var verdicts []Verdict
	for seed := int64(1); seed <= 10; seed++ {
		r := rand.New(rand.NewSource(seed))
		m, start := newMonitor()
		const delay = 360
		p := path{up: make([]float64, 40*8000)}
		p.ref = make([]float64, len(p.up))
		var asks []float64
		for at := 1.0; at < 37; at += 4 {
			line := speech(r, 2.5, 3000, 1.0)
			copy(p.ref[int(at*8000):], line)
			// The leak: the first 500 ms of the line, 360 ms later, at 3 dB.
			for i := 0; i < 4000; i++ {
				p.up[int(at*8000)+delay*8+i] += 0.7 * line[i]
			}
			// Interrupted 300 ms into the leak.
			asks = append(asks, at+float64(delay)/1000+0.3)
		}
		for i := range p.up {
			p.up[i] += 30 * r.NormFloat64()
		}
		drive(m, start, p, 0, 0, func(slot int) {
			if len(asks) > 0 && float64(slot)*0.02 >= asks[0] {
				verdicts = append(verdicts, m.Judge().Verdict)
				asks = asks[1:]
			}
		})
	}
	echoes := 0
	for _, v := range verdicts {
		if v == VerdictEcho {
			echoes++
		}
	}
	if echoes != len(verdicts) {
		t.Errorf("%d of %d leaked bursts judged ECHO", echoes, len(verdicts))
	}
}

// Unknown is not "not echo": it is the answer only when there is nothing to
// judge from. With no bot speech within reach of the window there is nothing
// the uplink could be the echo of, whatever it holds.
func TestTheVerdictIsUnknownOnlyWithNothingToJudge(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	m, start := newMonitor()
	if v := m.Judge().Verdict; v != VerdictUnknown {
		t.Fatalf("a fresh monitor says %s", v)
	}
	// A caller talks and the bot says nothing for 10 s.
	p := path{ref: make([]float64, 10*8000), up: speech(r, 10, 2000, 1.0)}
	unknown, judged := 0, 0
	drive(m, start, p, 0, 0, func(slot int) {
		if slot%5 == 0 {
			judged++
			if m.Judge().Verdict == VerdictUnknown {
				unknown++
			}
		}
	})
	if unknown != judged {
		t.Errorf("%d of %d moments with no bot speech judged UNKNOWN", unknown, judged)
	}
}

// A call whose estimates all found no echo says so plainly at its end: every
// estimate ran, none found an echo, and they all agreed.
func TestTheCallEndCountsOfACallWithNoEcho(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	m, start := newMonitor()
	p := echoPath(r, 30, -1, 0, 30)
	addSpeech(p.up, 0, 30, speech(r, 30, 1500, 0.5))
	drive(m, start, p, 0, 0, nil)
	s := m.Summary()
	if s.IsEchoFound || s.Estimates < 3 || s.EstimatesFoundEcho != 0 || s.FinalRunEstimates != s.Estimates {
		t.Fatalf("%+v", s)
	}
}

// A delay at the end of the range searched, or beyond it, is not one the
// measurement has found: the correlation peaks at the edge because it is still
// rising past it, or because there is nothing. Neither the estimates nor the
// verdict take it as an echo.
func TestADelayAtTheEdgeOfTheRangeIsNotAnEcho(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		r := rand.New(rand.NewSource(seed))
		m, start := newMonitor()
		p := echoPath(r, 40, 1060, 0.5, 30) // just past 1000 ms
		var verdicts []Verdict
		atEdge := 0
		drive(m, start, p, 0, 0, func(slot int) {
			if slot%50 == 0 && slot > 50*5 {
				verdicts = append(verdicts, m.Judge().Verdict)
			}
			if e := m.track.lastSeen; m.track.hasLast && e.isFound && e.lagBins > maxLagBins-edgeBins {
				t.Fatalf("seed %d: an estimate found %d ms", seed, e.lagBins*binMs)
			}
			// Without the edge rule this is what would have been found.
			if e := correlate(&m.upEnv, &m.refEnv, m.upBins-settleBins); slot%50 == 0 && e.lagBins == maxLagBins {
				atEdge++
			}
		})
		if atEdge == 0 {
			t.Errorf("seed %d: the correlation never peaked at the edge; the case is not what it means to be", seed)
		}
		if m.track.isFound {
			t.Errorf("seed %d: published %d ms", seed, m.track.lagBins*binMs)
		}
		for i, v := range verdicts {
			if v == VerdictEcho {
				t.Errorf("seed %d: judged ECHO at %d s with the echo beyond the range", seed, i+6)
			}
		}
	}
}

// A correlation that is high at many lags is the envelope's own rhythm, not a
// delay: a peak that a lag 200 ms away comes close to is not an echo found,
// even with enough bins for the ordinary bar and a margin over it that a fixed
// 0.12 would have let through (a live call's first estimate was 0.89 over
// 0.77, at a delay 230 ms off the real one).
func TestABroadPeakIsNotAnEchoFound(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	var ref, up [envBins]float32
	// A slow, nearly periodic envelope (600 ms bumps) with a little of its
	// own on top, so lags a period apart correlate nearly as well as the true
	// one.
	for i := 100; i < 600; i++ {
		ref[i] = float32(max(0, 1000+900*math.Sin(2*math.Pi*float64(i)/60)+300*r.NormFloat64()))
	}
	for i := 113; i < 600; i++ {
		up[i] = 0.9*ref[i-13] + 600*float32(r.Float64())
	}
	e := correlate(&up, &ref, 600)
	if e.pairs < minPairs || e.lagBins != 13 || e.second > e.corr-0.12 {
		t.Fatalf("the case is not what it means to be: %+v", e)
	}
	if e.isFound {
		t.Fatalf("a peak of %.2f with %.2f 200 ms away was found: %+v", e.corr, e.second, e)
	}
}

// Fewer than minPairs usable bins are enough for an estimate only when its
// peak is high and stands far clear of every other lag: that is how a loud
// echo shows itself from half a second of the bot's speech, and how chance
// correlation of unrelated speech, common on so few bins, does not pass.
func TestAnEstimateFromFewBinsNeedsASharperPeak(t *testing.T) {
	r := rand.New(rand.NewSource(23))
	// 80 bins of syllables from bin 150 on, beside an uplink that follows them
	// 30 bins later.
	var ref, clean, noisy [envBins]float32
	for i := 150; i < 230; {
		for n := 8 + r.Intn(15); n > 0 && i < 230; n, i = n-1, i+1 {
			ref[i] = float32(500 + r.Intn(3000))
		}
		i += 2 + r.Intn(8)
	}
	for i := 180; i < 260; i++ {
		clean[i] = 0.5 * ref[i-30]
		// The same echo under an uplink of its own that follows the bot only
		// loosely: a correlation the ordinary bar would accept.
		noisy[i] = clean[i] + float32(r.Intn(1200))
	}
	end := int64(250)
	if e := correlate(&clean, &ref, end); !e.isFound || e.lagBins != 30 || e.pairs >= minPairs {
		t.Fatalf("a clean echo on %d bins: %+v", e.pairs, e)
	}
	e := correlate(&noisy, &ref, end)
	if e.pairs >= minPairs || e.corr < minCorr || e.corr >= earlyMinCorr {
		t.Fatalf("the noisy case is not what it means to be: %+v", e)
	}
	if e.isFound || e.isEnough {
		t.Fatalf("a %.2f peak on %d bins was taken as an answer: %+v", e.corr, e.pairs, e)
	}
}
