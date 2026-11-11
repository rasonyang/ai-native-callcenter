// SPDX-License-Identifier: Apache-2.0

package echoprobe

import "math"

// The estimator works on energy envelopes, not waveforms: one value per 10 ms
// bin, the mean absolute sample. A far end that returns the bot's own speech
// returns a delayed, attenuated, filtered copy; filtering and the room smear
// the waveform but leave the syllable-rate envelope recognisable, and the
// envelope is cheap (two bins per frame, no FFT) and needs no alignment finer
// than the bin.
const (
	// binMs is the envelope step.
	binMs = 10
	// binSamples is samples per bin at 8 kHz.
	binSamples = 80
	// envBins is the envelope ring size, a power of two: 10.24 s of history,
	// more than the window plus the longest lag.
	envBins = 1024
	envMask = envBins - 1

	// maxLagBins is the longest delay searched: 1000 ms.
	maxLagBins = 100
	// windowBins is the analysis window: 4 s of uplink, ending slightly
	// before the newest bin so that a reference frame the pacer has not
	// pushed yet cannot be missing from it.
	windowBins = 400
	// settleBins is how far the analysis end trails the newest uplink bin.
	settleBins = 4

	// activeLevel is the mean absolute sample (of 32768) above which a
	// reference bin counts as speech: about -50 dBFS.
	activeLevel = 100
	// noiseSlack is added to the double-talk bound so a quiet line's own
	// noise does not read as the caller speaking.
	noiseSlack = 40
	// doubleTalkRatio: an uplink bin louder than this multiple of the loudest
	// reference in the lag range cannot be an echo of it (an echo is quieter
	// than its source), so a caller is talking and the bin is left out.
	doubleTalkRatio = 1.25

	// The estimate schedule counts usable bins: uplink bins whose lag range
	// holds bot speech, the bins an estimate can use. They are the bot's speech
	// and the second after it in which its echo is still arriving, so a bot
	// that is cut off after half a second keeps gathering evidence while its
	// echo comes back, and silence never triggers an estimate.
	//
	// firstEstimateBins is the usable bins before the first estimate runs:
	// 400 ms, the fewest an estimate accepts (earlyPairs).
	firstEstimateBins = earlyPairs
	// nextEstimateBins is the usable bins between estimates: about 5 s, more
	// than the window, so the agreeing estimates that publish a delay share
	// no data.
	nextEstimateBins = 500
	// retryBins is the wait after an estimate that had too little to go on:
	// 200 ms of usable bins. An estimate costs well under a millisecond.
	retryBins = 20

	// minPairs is the usable (bot-solo) bins an estimate needs at the
	// ordinary bar: 1.2 s.
	minPairs = 120
	// minCorr is the normalised correlation a lag needs to be called an echo.
	// Envelopes of unrelated speech correlate by chance, so this is set well
	// clear of the chance level measured in the tests.
	minCorr = 0.6
	// farLagBins and farRatio: every lag farther than 200 ms from the best
	// must correlate at most farRatio times as well as it (the best explains
	// at least twice their variance), so a broad envelope autocorrelation or
	// a periodic prosody cannot pass for a sharp echo. A fixed margin let a
	// broad 0.89 pass over a 0.77 elsewhere; echoes found on live calls sat
	// at 0.34 to 0.57.
	farLagBins = 20
	farRatio   = 0.7
	// edgeBins: a best lag this close to either end of the range searched
	// (30 ms) is not an echo found. It cannot be seen to fall on both sides,
	// and is evidence of a delay outside the range, or of nothing.
	edgeBins = 3
	// earlyPairs is the fewest usable bins an estimate runs on: 400 ms. From
	// earlyPairs up to minPairs an estimate counts only when its peak is
	// both high (earlyMinCorr) and far clear of every lag 200 ms away
	// (earlyMargin); otherwise it had too little to go on and waits for more.
	// A strong echo, the kind that cuts the bot off within a second, passes
	// this from its first half second: on a recorded call, 0.90 to 0.98 with
	// a margin of 0.58. Unrelated speech on 40 to 120 bins correlates by
	// chance far more often than on more (its envelope has few independent
	// syllables), which is what the stricter bar is for.
	earlyPairs   = 40
	earlyMinCorr = 0.9
	earlyMargin  = 0.5
	// maxGain: an echo louder than its source is not an echo (ERL below
	// -1.6 dB), it is the caller.
	maxGain = 1.2
	minGain = 1e-3

	// agreeBins is the tolerance between estimates that agree: 20 ms.
	agreeBins = 2
	// confirmAfter is the agreeing estimates that change what is published.
	confirmAfter = 3
)

// estimate is one run of the cross-correlation.
type estimate struct {
	// isEnough is false when the window held too little bot speech to judge;
	// such a run counts for nothing.
	isEnough bool
	isFound  bool
	lagBins  int
	corr     float64
	// gain is the amplitude ratio uplink/reference at the lag.
	gain float64
	// pairs is the bins that took part; second is the best correlation at a
	// lag farther than farLagBins from the best.
	pairs  int
	second float64
}

// returnLossDb is the echo return loss for this estimate.
func (e estimate) returnLossDb() float64 {
	return returnLossDb(e.gain)
}

func returnLossDb(gain float64) float64 {
	if gain <= 0 {
		return 0
	}
	return -20 * math.Log10(gain)
}

// tracker turns the stream of estimates into the published answer: the delay
// changes only after confirmAfter consecutive estimates that agree.
type tracker struct {
	// Counts for the call-end line: the estimates that ran, and those that
	// found an echo.
	estimates   int
	echoesFound int
	// loudestGain and quietestGain are the range across the estimates that
	// found an echo: the far end's return loss changes within a call, and
	// loudestGain is the loudest echo it has been measured to return.
	loudestGain  float64
	quietestGain float64

	// The current run of agreeing estimates: all finding no echo, or all
	// finding one within agreeBins of each other.
	runFound bool
	runLag   int
	runCount int
	runLags  [confirmAfter]int
	runGains [confirmAfter]float64

	// What is published.
	isFound  bool
	lagBins  int
	gain     float64
	hasLast  bool
	lastSeen estimate
}

func (t *tracker) add(e estimate) {
	t.estimates++
	t.hasLast = true
	t.lastSeen = e
	if e.isFound {
		if t.echoesFound == 0 || e.gain > t.loudestGain {
			t.loudestGain = e.gain
		}
		if t.echoesFound == 0 || e.gain < t.quietestGain {
			t.quietestGain = e.gain
		}
		t.echoesFound++
	}

	agrees := t.runCount > 0 && t.runFound == e.isFound &&
		(!e.isFound || abs(e.lagBins-t.runLag) <= agreeBins)
	if agrees {
		t.runCount++
	} else {
		t.runFound = e.isFound
		t.runCount = 1
	}
	t.runLag = e.lagBins
	if e.isFound {
		slot := (t.runCount - 1) % confirmAfter
		t.runLags[slot] = e.lagBins
		t.runGains[slot] = e.gain
	}
	if t.runCount < confirmAfter {
		return
	}
	if !e.isFound {
		t.isFound, t.lagBins, t.gain = false, 0, 0
		return
	}
	var lags, gains float64
	for i := range confirmAfter {
		lags += float64(t.runLags[i])
		gains += t.runGains[i]
	}
	t.isFound = true
	t.lagBins = int(math.Round(lags / confirmAfter))
	t.gain = gains / confirmAfter
}

// current is what a question about the uplink should use: the published
// delay, else the latest estimate's if it found one, and the loudest echo the
// call has been measured to return.
func (t *tracker) current() (lagBins int, loudestGain float64, ok bool) {
	switch {
	case t.isFound:
		return t.lagBins, t.loudestGain, true
	case t.hasLast && t.lastSeen.isFound:
		return t.lastSeen.lagBins, t.loudestGain, true
	}
	return 0, 0, false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// correlate searches the lags for the one at which the uplink envelope is a
// scaled copy of the reference envelope. up and ref are envelope rings; end is
// the exclusive newest bin of the window.
//
// A bin takes part when the reference was active somewhere in the lag range
// (bot speech, or its echo still arriving) and the uplink is not louder than
// any echo of it could be (no caller talking over it). Nothing here can tell a
// quiet caller from an echo, which is why the answer is published only after
// confirmAfter agreeing estimates.
func correlate(up, ref *[envBins]float32, end int64) estimate {
	start := end - windowBins
	if start < maxLagBins {
		start = maxLagBins
	}
	if end-start < earlyPairs {
		return estimate{}
	}

	// valid[i] for each window bin, from the loudest reference within reach.
	var valid [windowBins]bool
	var pairs int
	for i := start; i < end; i++ {
		peak := peakReference(ref, i)
		if peak < activeLevel {
			continue
		}
		if float64(up[i&envMask]) > doubleTalkRatio*float64(peak)+noiseSlack {
			continue
		}
		valid[i-start] = true
		pairs++
	}
	if pairs < earlyPairs {
		return estimate{pairs: pairs}
	}

	var corr [maxLagBins + 1]float64
	var gain [maxLagBins + 1]float64
	for l := 0; l <= maxLagBins; l++ {
		var n, su, sr, suu, srr, sur float64
		for i := start; i < end; i++ {
			if !valid[i-start] {
				continue
			}
			u := float64(up[i&envMask])
			r := float64(ref[(i-int64(l))&envMask])
			n++
			su += u
			sr += r
			suu += u * u
			srr += r * r
			sur += u * r
		}
		varU := suu - su*su/n
		varR := srr - sr*sr/n
		if varU <= 0 || varR <= 0 {
			continue
		}
		cov := sur - su*sr/n
		corr[l] = cov / math.Sqrt(varU*varR)
		gain[l] = cov / varR
	}

	best := 0
	for l := 1; l <= maxLagBins; l++ {
		if corr[l] > corr[best] {
			best = l
		}
	}
	e := estimate{isEnough: true, lagBins: best, corr: corr[best], gain: gain[best], pairs: pairs}
	for l := 0; l <= maxLagBins; l++ {
		if abs(l-best) > farLagBins && corr[l] > e.second {
			e.second = corr[l]
		}
	}
	isSharp := e.second <= farRatio*e.corr
	needCorr := minCorr
	if pairs < minPairs {
		needCorr, isSharp = earlyMinCorr, e.second <= e.corr-earlyMargin
	}
	isInside := best >= edgeBins && best <= maxLagBins-edgeBins
	isPeak := e.corr >= needCorr && isSharp && isInside &&
		e.gain >= minGain && e.gain <= maxGain
	switch {
	case isPeak:
		e.isFound = true
	case pairs < minPairs:
		// Too little to call it either way: no echo is not shown by 40 bins.
		e.isEnough = false
	}
	return e
}

// peakReference is the loudest reference bin in the lag range of uplink bin i.
func peakReference(ref *[envBins]float32, i int64) float32 {
	var peak float32
	for l := int64(0); l <= maxLagBins; l++ {
		if v := ref[(i-l)&envMask]; v > peak {
			peak = v
		}
	}
	return peak
}
