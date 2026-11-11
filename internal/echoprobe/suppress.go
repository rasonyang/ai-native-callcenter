// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"fmt"
	"math"
	"math/bits"
	"math/cmplx"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// The suppressor keeps the bot's own voice out of what the provider hears. A
// far end that returns it makes the provider's turn detection take the bot for
// the caller and cut it off, and on an engine that cuts by itself nothing on
// this side can refuse the cut: the echo has to be kept out of its input.
//
// It does not cancel. Subtracting an adapted copy of the reference removes
// little on the paths recorded so far (a loudspeaker and a microphone, a far
// end's own canceller that leaks while it converges); deciding, slot by slot,
// whether the uplink holds anything louder than the echo the call is known to
// return, and silencing it when it does not, is what keeps the provider from
// hearing the bot. It judges each 20 ms slot on the measurement's own time
// axis, from the reference and uplink rings the measurement keeps:
//
//   - Reach: the loudest 10 ms reference bin whose echo could land in the
//     slot, over the lag window (the measurement's delay ±80 ms once one is
//     known, published or the latest estimate's candidate; 20 to 700 ms
//     before, and the measurement's own search reaches 1000 ms). With no bot
//     audio in reach the slot passes and nothing else is computed (the first
//     such slot after a closed gate is faded in).
//   - The slot is the caller when it is louder than that reach by more than
//     the echo gain the call has learned plus a 6 dB margin.
//   - The gain is learned from slots whose last 80 ms follow the reference's
//     waveform (normalised cross-correlation of at least 0.5 at a lag in the
//     window) at a lag that held within 2.5 ms over two consecutive slots: it
//     rises towards what such a slot shows by at most 6 dB a slot, never above
//     -3 dB (an echo is not louder than its source), and otherwise decays by
//     2 dB a second of bot audio in reach, down to -40 dB. It starts at -12 dB.
//     The guards are what keep the caller's own voice from being learned as
//     echo: one 80 ms window of speech follows some lag of the bot's by chance
//     far more often than two consecutive ones follow the same lag; a chance
//     pair that gets through moves the gain by 6 dB, not to the caller's
//     level; and a gain at the ceiling still lets through a caller louder than
//     the bot. In the offline replay of recorded calls, removing both the
//     stable-lag guard and the rate limit let a caller be learned as echo and
//     held a real barge-in back by most of a second; removing either one alone
//     did not.
//   - A line that starts after 400 ms without bot audio has an onset window,
//     the first second of its echo (with no delay known, from the window's
//     near end to a second past its far end), with a gain of its own
//     (starting at -6 dB, learned only there and not capped), for a far end
//     whose canceller leaks the start of each line far louder than the rest.
//   - The gate opens on two caller slots of the last three and holds 200 ms
//     after the last one. A closed gate makes the slot silence in the leg's
//     law; the slot in which it opens or closes is faded over its 20 ms, so
//     the provider hears no click.
//
// Only the uplink pump's goroutine runs it, under the monitor's lock, beside
// the measurement it follows. Its cost is the correlation, two 2048-point
// transforms per 176 ms of lags searched, and it is skipped when no bot audio
// is in reach or the uplink is near silence.

const (
	// sxWindow is the uplink compared with the reference: the last 80 ms.
	sxWindow = 640
	// sxSize is the transform length; one transform covers sxChunkLags lags.
	sxSize      = 2048
	sxLogSize   = 11
	sxChunkLags = sxSize - sxWindow + 1
	// sxMinLagMs and sxMaxLagMs bound every lag window: the reference of the
	// newest 20 ms may not be on the axis yet, and the reference ring holds
	// what the longest delay the measurement searches needs.
	sxMinLagMs = 20
	sxMaxLagMs = maxLagBins * binMs
	// samplesPerMs at 8 kHz.
	samplesPerMs = binSamples / binMs
	// slotMs is one uplink slot.
	slotMs = 2 * binMs
	// silenceDb is the level of digital silence: -120 dBFS.
	silenceDb = -120.0
)

// suppressParams are the suppressor's rules. The product runs
// defaultSuppressParams; the offline replay compares variants of it.
type suppressParams struct {
	// activeDb: a reference bin quieter than this (dBFS, mean square) is no
	// bot audio.
	activeDb float64
	// learnMinDb: an uplink slot quieter than this is not correlated; it
	// teaches nothing.
	learnMinDb float64
	// learnCorr: the normalised correlation that says a slot follows the
	// reference.
	learnCorr float64
	// The learned gains, in dB of the uplink over the reference in reach.
	initDb      float64
	onsetInitDb float64
	floorDb     float64
	ceilingDb   float64
	// attackDb bounds how far one slot raises a gain; +Inf is no bound.
	attackDb float64
	// decayDb is per slot with bot audio in reach and nothing learned.
	decayDb  float64
	marginDb float64
	// stableSlots consecutive slots that follow the reference, at lags within
	// stableSamples of each other, before one is learned from.
	stableSlots   int
	stableSamples int
	// A line starts after onsetQuietSlots without bot audio; its onset window
	// is onsetSlots long from the moment its echo arrives.
	onsetQuietSlots int
	onsetSlots      int64
	// confirm caller slots of the last three open the gate; it holds
	// hangSlots after the last.
	confirm   int
	hangSlots int
	// hintHalfMs is the lag window's half width around a known delay;
	// wideFromMs and wideToMs the window before one is known.
	hintHalfMs int
	wideFromMs int
	wideToMs   int
}

var defaultSuppressParams = suppressParams{
	activeDb:        -50,
	learnMinDb:      -55,
	learnCorr:       0.5,
	initDb:          -12,
	onsetInitDb:     -6,
	floorDb:         -40,
	ceilingDb:       -3,
	attackDb:        6,
	decayDb:         2.0 * slotMs / 1000,
	marginDb:        6,
	stableSlots:     2,
	stableSamples:   20,
	onsetQuietSlots: 20,
	onsetSlots:      50,
	confirm:         2,
	hangSlots:       10,
	hintHalfMs:      80,
	wideFromMs:      sxMinLagMs,
	wideToMs:        700,
}

// sxScratch is the correlation's working memory, one per suppressing call.
type sxScratch struct {
	z    [sxSize]complex128
	up   [sxWindow]int16
	span [sxSize]int16
}

// suppressor is one call's echo suppression state. The monitor owns it and
// guards it with its lock; its decisions are taken (judgeSlot) and applied
// (Suppress) only on the uplink goroutine.
type suppressor struct {
	p suppressParams
	// refDb is the reference level of each 10 ms bin, in dBFS, beside the
	// monitor's envelope ring. Whoever places reference bins on the axis
	// writes it (the uplink goroutine, or Judge and Summary taking frames
	// still pending).
	refDb [envBins]float32
	xc    *sxScratch
	// failed turns the suppressor off for the rest of the call.
	failed bool

	// est is the learned echo gain, onest the onset windows' own.
	est   float64
	onest float64
	// hist holds the caller verdicts of the last three slots, newest in bit 0.
	hist uint8
	hang int
	// isOpen is the gate after the latest slot, wasOpen before it.
	isOpen  bool
	wasOpen bool
	// The run of slots that follow the reference.
	runGood  int
	prevGood bool
	prevLag  int
	// Line onsets: slots since bot audio was last sent, and the slot the
	// latest line started in.
	quiet     int
	lastOnset int64
	hasOnset  bool
	// decidedBins is the monitor's upBins when the latest decision was taken:
	// Suppress applies a decision only to the slot it was taken for.
	decidedBins int64

	// The lag window of the latest slot judged with bot audio in reach.
	fromMs   int
	toMs     int
	isHinted bool

	// Counters for the call-end line.
	examined   int64
	suppressed int64
	openings   int64
	correlated int64
	hinted     int64

	pcm [frameSamples]int16
	enc [frameSamples]byte
}

func newSuppressor(p suppressParams) *suppressor {
	// The transform's tables are built once per process; build them here, at
	// call setup, rather than on the first slot the uplink goroutine judges.
	fftTables()
	return &suppressor{
		p:       p,
		xc:      new(sxScratch),
		est:     p.initDb,
		onest:   p.onsetInitDb,
		isOpen:  true,
		wasOpen: true,
		quiet:   p.onsetQuietSlots,
		fromMs:  p.wideFromMs,
		toMs:    p.wideToMs,
	}
}

// judgeSlot decides what the provider gets for the slot FeedUplink has just
// recorded. The caller holds mu. A panic or an impossible state turns the
// suppressor off for the call; the measurement goes on.
func (m *Monitor) judgeSlot(isReal bool) {
	s := m.sup
	if s.failed {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.failSuppressor(r)
		}
	}()
	s.step(m, isReal)
	if math.IsNaN(s.est) || math.IsInf(s.est, 0) || math.IsNaN(s.onest) || math.IsInf(s.onest, 0) {
		m.failSuppressor(fmt.Sprintf("learned echo gain %v, onset %v", s.est, s.onest))
		return
	}
	s.decidedBins = m.upBins
}

// failSuppressor turns suppression off for the rest of the call, leaving the
// uplink as it comes, and says so once.
func (m *Monitor) failSuppressor(reason any) {
	m.sup.failed = true
	m.sup.isOpen = true
	m.log.Error("echo suppression disabled for this call", "reason", reason)
}

// step is one slot's decision. The current slot's samples are in m.scratch;
// it covers bins upBins-2 and upBins-1.
func (s *suppressor) step(m *Monitor, isReal bool) {
	p := &s.p
	slot := m.upBins/2 - 1
	b1 := m.upBins - 1
	b0 := b1 - 1
	s.wasOpen = s.isOpen

	// A line starts where bot audio is sent after a quiet stretch: the
	// reference of the last 0-20 ms.
	if m.referenceDb(b0-2, b1) > p.activeDb {
		if s.quiet >= p.onsetQuietSlots {
			s.lastOnset, s.hasOnset = slot, true
		}
		s.quiet = 0
	} else {
		s.quiet++
	}

	fromMs, toMs, onsetFrom, onsetTo, isHinted := s.window(m)
	reach := m.referenceDb(b0-int64(ceilDiv(toMs, binMs)), b1-int64(fromMs/binMs))
	if reach < p.activeDb {
		// No bot audio can be in this slot: it passes, and nothing carries
		// over to the next stretch of bot audio but the learned gains.
		s.isOpen, s.hist, s.hang = true, 0, 0
		s.runGood, s.prevGood = 0, false
		return
	}
	s.fromMs, s.toMs, s.isHinted = fromMs, toMs, isHinted
	if isReal {
		s.examined++
		if isHinted {
			s.hinted++
		}
	}

	level := levelDb(m.scratch[:])
	obs := level - reach
	isGood, lag := false, 0
	if isReal && level > p.learnMinDb {
		var corr float64
		corr, lag = m.followsReference(b1, fromMs, toMs)
		s.correlated++
		isGood = corr >= p.learnCorr
	}
	switch {
	case isGood && s.prevGood && abs(lag-s.prevLag) <= p.stableSamples:
		s.runGood++
	case isGood:
		s.runGood = 1
	default:
		s.runGood = 0
	}
	s.prevGood, s.prevLag = isGood, lag

	since := slot - s.lastOnset
	inOnset := s.hasOnset && since >= onsetFrom && since < onsetTo
	if isGood && s.runGood >= p.stableSlots {
		if inOnset {
			s.onest = max(s.onest, min(obs, s.onest+p.attackDb))
		} else {
			s.est = min(p.ceilingDb, max(s.est, min(obs, s.est+p.attackDb)))
		}
	} else {
		s.est = max(p.floorDb, s.est-p.decayDb)
		if inOnset {
			s.onest = max(p.floorDb, s.onest-p.decayDb)
		}
	}
	echo := s.est
	if inOnset {
		echo = max(s.est, s.onest)
	}

	isCaller := obs > echo+p.marginDb
	s.hist = (s.hist << 1) & 7
	if isCaller {
		s.hist |= 1
		if bits.OnesCount8(s.hist) >= p.confirm {
			s.hang = p.hangSlots
		}
	}
	s.isOpen = s.hang > 0
	if s.isOpen {
		s.hang--
		if !s.wasOpen {
			s.openings++
		}
	}
}

// window is the lag window of this slot (milliseconds) and the onset window
// (slots after a line's start), from the delay the measurement knows when it
// knows one.
func (s *suppressor) window(m *Monitor) (fromMs, toMs int, onsetFrom, onsetTo int64, isHinted bool) {
	p := &s.p
	if lagBins, _, ok := m.track.current(); ok {
		at := lagBins * binMs
		onsetFrom = int64((at + slotMs/2) / slotMs)
		return max(at-p.hintHalfMs, sxMinLagMs), min(at+p.hintHalfMs, sxMaxLagMs),
			onsetFrom, onsetFrom + p.onsetSlots, true
	}
	// No delay known: the echo of a line may start anywhere in the window.
	return p.wideFromMs, p.wideToMs, int64(p.wideFromMs / slotMs),
		int64(ceilDiv(p.wideToMs, slotMs)) + p.onsetSlots, false
}

// referenceDb is the loudest reference bin from bin from to bin to, both
// included, among those on the axis; silence when there are none.
func (m *Monitor) referenceDb(from, to int64) float64 {
	from = max(from, 0, m.refNext-envBins)
	to = min(to, m.refNext-1)
	best := float32(silenceDb)
	for b := from; b <= to; b++ {
		if v := m.sup.refDb[b&envMask]; v > best {
			best = v
		}
	}
	return float64(best)
}

// followsReference is how closely the last 80 ms of uplink, up to the end of
// bin b1, follow the reference at some lag from fromMs to toMs: the best
// normalised cross-correlation (0 to 1, either polarity) and its lag in
// samples. A near-silent uplink follows nothing; a lag whose reference is near
// silence is not counted.
func (m *Monitor) followsReference(b1 int64, fromMs, toMs int) (float64, int) {
	x := m.sup.xc
	w0 := (b1+1)*binSamples - sxWindow
	ringSamples(x.up[:], &m.upPCM, w0, m.upBins)
	var eu float64
	for _, v := range x.up {
		eu += float64(v) * float64(v)
	}
	if eu < sxWindow*4 {
		return 0, 0
	}
	lo, hi := fromMs*samplesPerMs, toMs*samplesPerMs
	best, bestLag := 0.0, 0
	for a := lo; a <= hi; a += sxChunkLags {
		b := min(a+sxChunkLags-1, hi)
		n := b - a + 1
		span := x.span[:n+sxWindow-1]
		ringSamples(span, &m.refPCM, w0-int64(b), m.refNext)
		if c, k := correlateChunk(x, span, eu, n); c > best {
			best, bestLag = c, b-k
		}
	}
	return best, bestLag
}

// correlateChunk correlates x.up with span at n offsets: offset k compares the
// window with span[k : k+sxWindow]. It returns the best normalised value and
// its offset.
func correlateChunk(x *sxScratch, span []int16, eu float64, n int) (float64, int) {
	// Both real signals in one complex transform, as correlateWaveform does:
	// the reference span in the real part, the window in the imaginary part.
	for i := range x.z {
		var re, im float64
		if i < len(span) {
			re = float64(span[i])
		}
		if i < sxWindow {
			im = float64(x.up[i])
		}
		x.z[i] = complex(re, im)
	}
	fft2048(&x.z, false)
	for k := 0; k <= sxSize/2; k++ {
		j := (sxSize - k) % sxSize
		zk, zj := x.z[k], x.z[j]
		rk := (zk + cmplx.Conj(zj)) / 2
		uk := (zk - cmplx.Conj(zj)) / complex(0, 2)
		rj := (zj + cmplx.Conj(zk)) / 2
		uj := (zj - cmplx.Conj(zk)) / complex(0, 2)
		x.z[k] = rk * cmplx.Conj(uk)
		x.z[j] = rj * cmplx.Conj(uj)
	}
	fft2048(&x.z, true)
	// x.z[k] is now sxSize times sum_j span[k+j] up[j].

	var er int64 // of span[k : k+sxWindow], slid along
	for _, v := range span[:sxWindow] {
		er += int64(v) * int64(v)
	}
	best, bestK := 0.0, 0
	for k := 0; k < n; k++ {
		if k > 0 {
			out, in := int64(span[k-1]), int64(span[k-1+sxWindow])
			er += in*in - out*out
		}
		if er < sxWindow*100 {
			continue
		}
		c := math.Abs(real(x.z[k])/sxSize) / math.Sqrt(eu*float64(er))
		if c > best {
			best, bestK = c, k
		}
	}
	return best, bestK
}

// fft2048 is fft for sxSize points, on the same twiddle table.
func fft2048(z *[sxSize]complex128, inverse bool) {
	t := fftTables()
	for i := range z {
		if j := int(bits.Reverse16(uint16(i)) >> (16 - sxLogSize)); i < j {
			z[i], z[j] = z[j], z[i]
		}
	}
	for size := 2; size <= sxSize; size <<= 1 {
		half, step := size/2, xcSize/size
		for start := 0; start < sxSize; start += size {
			for k := 0; k < half; k++ {
				w := t.twiddle[k*step]
				if inverse {
					w = cmplx.Conj(w)
				}
				a, b := z[start+k], z[start+k+half]*w
				z[start+k], z[start+k+half] = a+b, a-b
			}
		}
	}
}

// ringSamples copies the samples from absolute sample index start into dst out
// of a 10 ms bin ring whose first unwritten bin is next. Samples before the
// call, not yet written, or overwritten are silence.
func ringSamples(dst []int16, ring *[pcmBins * binSamples]int16, start, next int64) {
	for i := 0; i < len(dst); {
		at := start + int64(i)
		b := at / binSamples
		if at < 0 && at%binSamples != 0 {
			b--
		}
		off := int(at - b*binSamples)
		n := min(binSamples-off, len(dst)-i)
		if b < 0 || b >= next || b < next-pcmBins {
			clear(dst[i : i+n])
		} else {
			from := int(b%pcmBins)*binSamples + off
			copy(dst[i:i+n], ring[from:from+n])
		}
		i += n
	}
}

// levelDb is the RMS level of x in dBFS, silenceDb for digital silence.
func levelDb(x []int16) float64 {
	var sq int64
	for _, v := range x {
		sq += int64(v) * int64(v)
	}
	ms := float64(sq) / float64(len(x))
	const floor = (32768 * 1e-6) * (32768 * 1e-6)
	return 10 * math.Log10(max(ms, floor)/(32768.0*32768.0))
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// Suppress applies the decision on the slot FeedUplink was just given to
// frame, that slot's G.711 payload, in place. A slot the gate holds closed
// becomes the law's silence; a slot in which the gate opens or closes is
// decoded, faded in or out over its 20 ms and re-encoded; every other slot is
// left byte for byte as it was. It reports whether the frame changed.
//
// It changes nothing on a Monitor built without Config.Suppress, after a
// failure, for a frame of another size, or for a slot it did not judge. Call
// it on the uplink goroutine, after FeedUplink and with the same frame.
func (m *Monitor) Suppress(frame []byte) (isChanged bool) {
	if m.sup == nil || len(frame) != frameSamples {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sup
	if s.failed || m.failed || s.decidedBins != m.upBins || (s.isOpen && s.wasOpen) {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			m.failSuppressor(r)
			isChanged = false
		}
	}()
	if !s.isOpen && !s.wasOpen {
		copy(frame, media.SilenceFrame(m.law))
	} else {
		// The gate opens (fade in) or closes (fade out) in this slot. The
		// frame is written only once the whole slot has been worked out.
		pcm := m.law.Decode(s.pcm[:0], frame)
		for i, v := range pcm {
			g := float32(i) / frameSamples
			if !s.isOpen {
				g = 1 - g
			}
			pcm[i] = int16(float32(v) * g)
		}
		copy(frame, m.law.Encode(s.enc[:0], pcm))
	}
	if !s.isOpen {
		s.suppressed++
	}
	return true
}

// SuppressionSummary is what the suppressor did on a call.
type SuppressionSummary struct {
	// IsOn is true when the call suppressed (Config.Suppress); everything
	// below is zero otherwise.
	IsOn bool
	// IsFailed is true when the suppressor, or the measurement it runs on,
	// turned itself off during the call; the uplink passed unchanged from then.
	IsFailed bool
	// FramesExamined is the caller frames judged with bot audio in reach;
	// FramesWithDelay those of them judged on the measurement's delay rather
	// than the wide window. FramesCorrelated is the frames whose waveform was
	// compared with the reference.
	FramesExamined   int64
	FramesWithDelay  int64
	FramesCorrelated int64
	// FramesSuppressed is the frames sent to the provider as silence, or
	// faded out as the gate closed.
	FramesSuppressed int64
	// GateOpenings is the times the gate opened for the caller over bot audio.
	GateOpenings int64
	// LagFromMs and LagToMs are the lag window of the last frame judged with
	// bot audio in reach; IsLagFromDelay says it came from the measurement's
	// delay.
	LagFromMs      int
	LagToMs        int
	IsLagFromDelay bool
	// EchoGainDb and OnsetEchoGainDb are the learned gains at the end: how
	// loud the uplink may be, against the bot audio in reach, and still be
	// taken for its echo (minus the margin).
	EchoGainDb      float64
	OnsetEchoGainDb float64
}

func (s *suppressor) summary(isMeasurementFailed bool) SuppressionSummary {
	return SuppressionSummary{
		IsOn:             true,
		IsFailed:         s.failed || isMeasurementFailed,
		FramesExamined:   s.examined,
		FramesWithDelay:  s.hinted,
		FramesCorrelated: s.correlated,
		FramesSuppressed: s.suppressed,
		GateOpenings:     s.openings,
		LagFromMs:        s.fromMs,
		LagToMs:          s.toMs,
		IsLagFromDelay:   s.isHinted,
		EchoGainDb:       round1(s.est),
		OnsetEchoGainDb:  round1(s.onest),
	}
}
