// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"math"
	"math/cmplx"
	"sync"
)

// The verdict on an interruption is taken from the waveform, not the envelope.
// A far end whose own canceller leaks short bursts of the bot's voice when a
// line starts, and suppresses the rest, returns too little for an envelope
// estimate over seconds to see; 600 ms of its uplink still correlates with the
// bot's waveform at the echo's delay, sample for sample, far above what
// unrelated speech reaches.
const (
	// xcWindow is the uplink judged: the last 600 ms, in 8 kHz samples.
	xcWindow = 60 * binSamples
	// xcMaxLag is the longest delay searched: 1000 ms.
	xcMaxLag = maxLagBins * binSamples
	// xcSpan is the reference the window is compared with: 1.6 s.
	xcSpan = xcWindow + xcMaxLag
	// xcSize is the FFT length: a power of two no shorter than xcSpan, so the
	// circular correlation of every lag searched is the linear one.
	xcSize    = 16384
	xcLogSize = 14
)

// xcScratch is one judgement's working memory: the copies taken under the
// monitor's lock and the FFT buffer. It is large (about 360 kB) and needed
// only while a judgement runs, so it comes from a pool and is never held
// between interruptions.
type xcScratch struct {
	ref [xcSpan]int16
	up  [xcWindow]int16
	// refOn marks the reference bins with bot speech in them.
	refOn [xcSpan / binSamples]bool
	z     [xcSize]complex128
	// ncc is the normalised correlation at each k (lag xcMaxLag-k), -1 where
	// the lag is not searched.
	ncc [xcMaxLag + 1]float64
}

var xcPool = sync.Pool{New: func() any { return new(xcScratch) }}

// xcTables are the FFT's twiddle factors and bit-reversal permutation, built
// on first use.
type xcTables struct {
	twiddle [xcSize / 2]complex128
	rev     [xcSize]uint16
}

var (
	xcTablesOnce sync.Once
	xcTab        *xcTables
)

func fftTables() *xcTables {
	xcTablesOnce.Do(func() {
		t := new(xcTables)
		for k := range t.twiddle {
			s, c := math.Sincos(-2 * math.Pi * float64(k) / xcSize)
			t.twiddle[k] = complex(c, s)
		}
		for i := range t.rev {
			var r uint16
			for b := 0; b < xcLogSize; b++ {
				if i&(1<<b) != 0 {
					r |= 1 << (xcLogSize - 1 - b)
				}
			}
			t.rev[i] = r
		}
		xcTab = t
	})
	return xcTab
}

// fft transforms z in place (radix 2); inverse transforms back, unscaled.
func fft(z *[xcSize]complex128, inverse bool) {
	t := fftTables()
	for i := range z {
		if j := int(t.rev[i]); i < j {
			z[i], z[j] = z[j], z[i]
		}
	}
	for size := 2; size <= xcSize; size <<= 1 {
		half, step := size/2, xcSize/size
		for start := 0; start < xcSize; start += size {
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

// xcPeak is what the waveform correlation found.
type xcPeak struct {
	// isJudged is false when there was nothing to judge: no bot speech in
	// reach of the window at any lag searched, or a silent uplink.
	isJudged bool
	// corr is the normalised correlation (0 to 1, either polarity) at lagMs;
	// second is the best at a lag more than xcGuardMs from it.
	corr   float64
	lagMs  int
	second float64
	// lagSamples is the lag exactly.
	lagSamples int
	// gain is the uplink's amplitude over the reference's at the lag, as an
	// echo of it: the regression coefficient.
	gain float64
}

const (
	// xcEdgeSamples: lags this close to either end of the range searched are
	// not searched. A peak there cannot be seen to fall on both sides, and is
	// evidence of a delay outside the range, or of nothing: 20 ms.
	xcEdgeSamples = 2 * binSamples
	// xcMinOnBins: a lag is searched only when the reference it compares the
	// window with holds this much bot speech: 100 ms. With less, a little
	// overlap of the bot's last syllable can correlate by chance.
	xcMinOnBins = 10
	// xcGuardMs: the second-best peak is taken farther than this from the
	// best. Voiced speech repeats at its pitch period, so lags within a few
	// periods of an echo correlate with it too.
	xcGuardMs = 40
)

// correlateWaveform finds the lag at which the uplink window is most like the
// reference: the normalised cross-correlation of s.up against s.ref at every
// lag from 0 to xcMaxLag, by FFT. It reads s.ref, s.up and s.refOn and
// overwrites s.z and s.ncc.
func correlateWaveform(s *xcScratch) xcPeak {
	var upSum, upSq float64
	for _, v := range s.up {
		upSum += float64(v)
		upSq += float64(v) * float64(v)
	}
	upMean := upSum / xcWindow
	upEnergy := upSq - upSum*upMean
	if upEnergy <= 0 {
		return xcPeak{}
	}

	// Both real signals in one complex transform: the reference in the real
	// part, the (centred) uplink in the imaginary part.
	for i := range s.z {
		var re, im float64
		if i < xcSpan {
			re = float64(s.ref[i])
		}
		if i < xcWindow {
			im = float64(s.up[i]) - upMean
		}
		s.z[i] = complex(re, im)
	}
	fft(&s.z, false)
	// R(k) = (Z(k) + conj Z(N-k)) / 2 and U(k) = (Z(k) - conj Z(N-k)) / 2i;
	// the correlation's spectrum is R(k) conj U(k).
	for k := 0; k <= xcSize/2; k++ {
		n := (xcSize - k) % xcSize
		zk, zn := s.z[k], s.z[n]
		rk := (zk + cmplx.Conj(zn)) / 2
		uk := (zk - cmplx.Conj(zn)) / complex(0, 2)
		rn := (zn + cmplx.Conj(zk)) / 2
		un := (zn - cmplx.Conj(zk)) / complex(0, 2)
		s.z[k] = rk * cmplx.Conj(uk)
		s.z[n] = rn * cmplx.Conj(un)
	}
	fft(&s.z, true)
	// s.z[k] is now xcSize times sum_j ref[k+j] up[j]: the window against the
	// reference xcMaxLag-k samples earlier.

	var onBefore [xcSpan/binSamples + 1]int
	for b, on := range s.refOn {
		onBefore[b+1] = onBefore[b]
		if on {
			onBefore[b+1]++
		}
	}
	var refEnergy int64 // of ref[k : k+xcWindow], slid along
	for _, v := range s.ref[:xcWindow] {
		refEnergy += int64(v) * int64(v)
	}
	best := xcPeak{}
	bestK := -1
	for k := 0; k <= xcMaxLag; k++ {
		if k > 0 {
			out, in := int64(s.ref[k-1]), int64(s.ref[k-1+xcWindow])
			refEnergy += in*in - out*out
		}
		s.ncc[k] = -1
		lag := xcMaxLag - k
		b := k / binSamples
		onBins := onBefore[min(b+xcWindow/binSamples, len(s.refOn))] - onBefore[b]
		if lag < xcEdgeSamples || lag > xcMaxLag-xcEdgeSamples || onBins < xcMinOnBins || refEnergy <= 0 {
			continue
		}
		cov := real(s.z[k]) / xcSize
		c := math.Abs(cov) / math.Sqrt(upEnergy*float64(refEnergy))
		s.ncc[k] = c
		if bestK < 0 || c > best.corr {
			best = xcPeak{isJudged: true, corr: c, gain: math.Abs(cov) / float64(refEnergy)}
			bestK = k
		}
	}
	if bestK < 0 {
		return best
	}
	best.lagSamples = xcMaxLag - bestK
	best.lagMs = best.lagSamples / (binSamples / binMs)
	guard := xcGuardMs * (binSamples / binMs)
	for k, c := range s.ncc {
		if abs(k-bestK) > guard && c > best.second {
			best.second = c
		}
	}
	return best
}

// copyForJudgement takes what a judgement needs under mu: the newest
// xcWindow of uplink and the xcSpan of reference that ends with it, into s,
// and the delay known so far. Bins the reference has not reached yet, or
// before the call, are silence.
func (m *Monitor) copyForJudgement(s *xcScratch) (lagBins int, loudestGain float64, hasPath, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed {
		return 0, 0, false, false
	}
	m.takeReference()
	end := m.upBins
	for i := range xcWindow / binSamples {
		b := end - xcWindow/binSamples + int64(i)
		dst := s.up[i*binSamples : (i+1)*binSamples]
		if b < 0 || b < m.upBins-pcmBins {
			clear(dst)
			continue
		}
		copy(dst, m.upPCM[(b%pcmBins)*binSamples:])
	}
	for i := range xcSpan / binSamples {
		b := end - xcSpan/binSamples + int64(i)
		dst := s.ref[i*binSamples : (i+1)*binSamples]
		if b < 0 || b >= m.refNext || b < m.refNext-pcmBins {
			clear(dst)
			s.refOn[i] = false
			continue
		}
		copy(dst, m.refPCM[(b%pcmBins)*binSamples:])
		s.refOn[i] = m.refEnv[b&envMask] >= activeLevel
	}
	lagBins, loudestGain, hasPath = m.track.current()
	return lagBins, loudestGain, hasPath, true
}

// The thresholds of a waveform verdict, from live calls whose ground truth is
// known: echoes peaked at 0.17 to 0.84, with at most 0.57 of the peak at any
// lag 40 ms away; a caller over the bot peaked at 0.07 to 0.15, or at 0.29
// to 0.47 with 0.5 to 0.7 of it elsewhere and 3 to 6 dB louder than the bot.
const (
	// xcStrongCorr: a peak this high is the bot's waveform whether or not a
	// delay is known. On live calls a caller over the bot reached 0.47; the
	// echoes it has to take without a delay known peaked at 0.60 to 0.69.
	xcStrongCorr = 0.55
	// xcMinCorr: below this the uplink does not follow the bot at any delay:
	// not echo.
	xcMinCorr = 0.15
	// xcMaxSecondRatio: the best lag elsewhere may correlate at most this
	// share of the peak, or the peak is not a delay but chance: not echo.
	xcMaxSecondRatio = 0.6
	// xcAgreeMs: a weaker peak is echo only within this of a delay known on
	// the call (published, an estimate's candidate, or a strong verdict's).
	// A far end's delay drifts by tens of milliseconds within a call.
	xcAgreeMs = 40
	// xcLoudRatio: a weaker peak is echo only when the uplink is no louder
	// than this many times the loudest echo measured on the call (6 dB), and
	// never louder than doubleTalkRatio times the bot's own speech at the
	// delay. A caller over an echo correlates with it too, less, and is
	// louder than it.
	xcLoudRatio = 2.0
)

// Judgement is the measurement's answer about an interruption.
type Judgement struct {
	Verdict Verdict
	// IsCorrelated is set when the waveform was correlated; the fields below
	// describe the peak, whatever the verdict, so that the thresholds can be
	// re-derived from logs.
	IsCorrelated bool
	// Correlation is the normalised cross-correlation (0 to 1, either
	// polarity) of the last 600 ms of uplink with the bot's audio LagMs
	// earlier; SecondCorrelation the best more than 40 ms away from it.
	Correlation       float64
	SecondCorrelation float64
	LagMs             int
	// UplinkLevelDb is the uplink's level over the bot's at LagMs.
	UplinkLevelDb float64
}

// Judge says whether the uplink just before now is the bot's own voice coming
// back. The copies it works on are taken under mu; the correlation (two
// transforms of 16384 points) runs outside it, so neither the pacer nor the
// uplink goroutine waits on it. Call it after the barge-in's flush.
func (m *Monitor) Judge() Judgement {
	s := xcPool.Get().(*xcScratch)
	defer xcPool.Put(s)
	lagBins, loudestGain, hasPath, ok := m.copyForJudgement(s)
	if !ok {
		return Judgement{Verdict: VerdictUnknown}
	}
	p := correlateWaveform(s)
	if !p.isJudged {
		return Judgement{Verdict: VerdictUnknown}
	}
	var upSq, refSq float64
	for _, v := range s.up {
		upSq += float64(v) * float64(v)
	}
	at := xcSpan - xcWindow - p.lagSamples
	for _, v := range s.ref[at : at+xcWindow] {
		refSq += float64(v) * float64(v)
	}
	level := math.Sqrt(upSq / refSq)
	j := Judgement{
		IsCorrelated: true, Correlation: round2(p.corr), SecondCorrelation: round2(p.second),
		LagMs: p.lagMs, UplinkLevelDb: round1(20 * math.Log10(level)),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	isSharp := p.corr >= xcMinCorr && p.second <= xcMaxSecondRatio*p.corr
	switch {
	case !isSharp:
		j.Verdict = VerdictNotEcho
	case p.corr >= xcStrongCorr:
		// Neither the part of the uplink the bot's waveform explains nor the
		// rest may be louder than the bot: an echo is not louder than its
		// source, and what is left over louder than it is someone talking
		// over it (a leaking canceller leaves a remainder as loud as the
		// bot, a caller more).
		if p.gain > maxGain || level*math.Sqrt(1-p.corr*p.corr) > doubleTalkRatio {
			j.Verdict = VerdictNotEcho
			break
		}
		j.Verdict = VerdictEcho
		m.waveLagMs, m.hasWaveLag = p.lagMs, true
		m.waveLoudestGain = max(m.waveLoudestGain, p.gain)
	case !hasPath && !m.hasWaveLag:
		// A weaker peak and no delay to hold it against.
		j.Verdict = VerdictUnknown
	default:
		isOnPath := (hasPath && abs(p.lagMs-lagBins*binMs) <= xcAgreeMs) ||
			(m.hasWaveLag && abs(p.lagMs-m.waveLagMs) <= xcAgreeMs)
		bound := min(doubleTalkRatio, xcLoudRatio*max(loudestGain, m.waveLoudestGain))
		if isOnPath && level <= bound {
			j.Verdict = VerdictEcho
		} else {
			j.Verdict = VerdictNotEcho
		}
	}
	return j
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
