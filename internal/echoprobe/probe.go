// SPDX-License-Identifier: Apache-2.0

// Package echoprobe measures the bot's own voice coming back on an AI call's
// uplink. It never alters audio: it watches what the leg sent (the reference)
// and what the uplink clock handed the provider, estimates the delay and the
// return loss of any echo between them, and can say whether the uplink of the
// last 600 ms is the bot's own waveform coming back (Judge).
//
// It is pure Go and takes plain frames, so the same code runs on recorded
// calls offline (the dump this package writes is that input).
package echoprobe

import (
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

const (
	// frameSamples is one 20 ms frame at 8 kHz.
	frameSamples = media.FrameSamples
	// pendingFrames is the handoff queue between the RTP send goroutine and
	// the uplink goroutine: 640 ms, far more than the one or two frames that
	// ever wait.
	pendingFrames = 32
	// pcmBins is the reference PCM ring: 2 s in 10 ms bins.
	pcmBins = 200
)

// txFrame is one reference frame waiting to be taken by the uplink goroutine.
type txFrame struct {
	at   time.Time
	g711 [frameSamples]byte
	isOn bool
}

// Monitor is one call's echo measurement. Two goroutines use it: the RTP send
// loop calls PushReference, and the uplink pump calls FeedUplink; the
// barge-in path calls Judge from a third.
//
// The handoff between them is a short mutex around a copy of one 160-byte
// frame into a fixed queue: no allocation, nothing held across a decode or an
// estimate, and the sender never waits on the analysis, which holds only mu.
// A queue that is somehow full drops its oldest frame and counts it.
type Monitor struct {
	law   media.Law
	start time.Time
	log   *slog.Logger

	txMu      sync.Mutex
	tx        [pendingFrames]txFrame
	txHead    int
	txCount   int
	txDropped int64

	// mu guards everything below. The uplink goroutine takes it once per
	// slot, Judge and Summary take it briefly (Judge's computation runs on
	// copies, outside it).
	mu     sync.Mutex
	failed bool

	refEnv [envBins]float32
	upEnv  [envBins]float32
	// refPCM is the last 2 s of what the leg sent, in 10 ms bins.
	refPCM [pcmBins * binSamples]int16
	// refNext is the first reference bin not yet written.
	refNext int64
	// upBins is the uplink bins fed so far; slot k covers bins 2(k-1), 2k-1.
	upBins int64

	// activeBins counts reference bins with speech in them (the bot speech
	// the call-end line reports). usableBins counts uplink bins whose lag
	// range holds bot speech; the estimate schedule runs on it, so silence
	// never triggers one.
	activeBins   int64
	usableBins   int64
	nextEstimate int64
	track        tracker

	// upPCM is the last 2 s of the uplink, beside refPCM; dump is the
	// optional two-way recording, dumpNext the first bin not yet written to it.
	upPCM    [pcmBins * binSamples]int16
	dump     *Dumper
	dumpNext int64

	scratch [frameSamples]int16

	// What strong waveform verdicts have measured (Judge): the delay of the
	// latest, and the loudest echo among them, beside the estimates'.
	waveLagMs       int
	hasWaveLag      bool
	waveLoudestGain float64
}

// Config builds a Monitor.
type Config struct {
	// Start is the uplink clock's start: both signals are placed on the time
	// axis that begins here.
	Start time.Time
	Law   media.Law
	// Dump, when non-nil, receives the two signals as aligned stereo frames.
	Dump *Dumper
	// Logger reports an internal failure, once.
	Logger *slog.Logger
}

// New returns a Monitor.
func New(cfg Config) *Monitor {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Monitor{
		law:          cfg.Law,
		start:        cfg.Start,
		log:          log,
		dump:         cfg.Dump,
		nextEstimate: firstEstimateBins,
	}
}

// PushReference records what the leg put on the wire in the 20 ms slot that
// began at at: frame is the G.711 payload, nil when the slot carried no bot
// audio (silence padding, or nothing). The frame is copied. It is called on
// the RTP send goroutine and does not allocate or wait for the analysis.
func (m *Monitor) PushReference(frame []byte, at time.Time) {
	m.txMu.Lock()
	if m.txCount == pendingFrames {
		m.txHead = (m.txHead + 1) % pendingFrames
		m.txCount--
		m.txDropped++
	}
	slot := &m.tx[(m.txHead+m.txCount)%pendingFrames]
	slot.at = at
	slot.isOn = frame != nil
	if frame != nil {
		n := copy(slot.g711[:], frame)
		// A short frame must not leave an earlier frame's tail in the slot.
		clear(slot.g711[n:])
	}
	m.txCount++
	m.txMu.Unlock()
}

// FeedUplink is called once per uplink slot, in order, with the G.711 frame
// the clock emitted (nil for a silence fill, or a slot nothing was sent in).
// It reads the frame and keeps nothing of it.
func (m *Monitor) FeedUplink(frame []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.fail(r)
		}
	}()

	m.takeReference()

	pcm := m.scratch[:]
	if len(frame) == frameSamples {
		pcm = m.law.Decode(pcm, frame)
	} else {
		clear(pcm)
	}
	for half := range 2 {
		part := pcm[half*binSamples : (half+1)*binSamples]
		bin := m.upBins + int64(half)
		m.upEnv[bin&envMask] = meanAbs(part)
		copy(m.upPCM[(bin%pcmBins)*binSamples:], part)
	}
	m.upBins += 2

	// Bins nothing was pushed for are silence on the reference, but only
	// those clearly in the past: a frame pushed a moment late still belongs
	// where its timestamp says.
	end := m.upBins - settleBins
	m.fillReferenceTo(end)
	if m.dump != nil {
		m.writeDump(end)
	}

	// The two bins that just became analysable.
	for i := end - 2; i < end; i++ {
		if i >= maxLagBins && peakReference(&m.refEnv, i) >= activeLevel {
			m.usableBins++
		}
	}
	m.maybeEstimate()
}

// takeReference moves the pending reference frames onto the time axis.
func (m *Monitor) takeReference() {
	for {
		m.txMu.Lock()
		if m.txCount == 0 {
			m.txMu.Unlock()
			return
		}
		f := m.tx[m.txHead] // a copy: the sender may reuse the slot
		m.txHead = (m.txHead + 1) % pendingFrames
		m.txCount--
		m.txMu.Unlock()
		m.placeReference(&f)
	}
}

// placeReference writes one frame at the bin its send time falls in.
//
// Frames that arrive faster than the axis advances are placed one after the
// other rather than on top of each other, and a one-bin gap repeats the bin
// before it, so pacer jitter of a few milliseconds shifts nothing by more than
// a bin.
func (m *Monitor) placeReference(f *txFrame) {
	b := int64((f.at.Sub(m.start) + binMs*time.Millisecond/2) / (binMs * time.Millisecond))
	if f.at.Before(m.start) {
		b = 0
	}
	if b < m.refNext {
		b = m.refNext
	}
	if gap := b - m.refNext; gap == 1 {
		m.repeatReferenceBin()
	} else if gap > 1 {
		m.fillReferenceTo(b)
	}

	var pcm [frameSamples]int16
	samples := pcm[:]
	if f.isOn {
		samples = m.law.Decode(samples, f.g711[:])
	}
	for half := range 2 {
		part := samples[half*binSamples : (half+1)*binSamples]
		m.setReferenceBin(m.refNext, part)
	}
}

func (m *Monitor) setReferenceBin(bin int64, part []int16) {
	env := meanAbs(part)
	m.refEnv[bin&envMask] = env
	copy(m.refPCM[(bin%pcmBins)*binSamples:], part)
	if env >= activeLevel {
		m.activeBins++
	}
	if bin >= m.refNext {
		m.refNext = bin + 1
	}
}

func (m *Monitor) repeatReferenceBin() {
	if m.refNext == 0 {
		m.fillReferenceTo(1)
		return
	}
	prev := m.refNext - 1
	var part [binSamples]int16
	copy(part[:], m.refPCM[(prev%pcmBins)*binSamples:])
	m.setReferenceBin(m.refNext, part[:])
}

// fillReferenceTo writes silent bins up to (not including) bin.
func (m *Monitor) fillReferenceTo(bin int64) {
	var zero [binSamples]int16
	for m.refNext < bin {
		m.setReferenceBin(m.refNext, zero[:])
	}
}

func meanAbs(x []int16) float32 {
	var sum int32
	for _, v := range x {
		if v < 0 {
			sum -= int32(v)
		} else {
			sum += int32(v)
		}
	}
	return float32(sum) / float32(len(x))
}

func (m *Monitor) maybeEstimate() {
	if m.usableBins < m.nextEstimate {
		return
	}
	e := correlate(&m.upEnv, &m.refEnv, m.upBins-settleBins)
	if !e.isEnough {
		m.nextEstimate = m.usableBins + retryBins
		return
	}
	m.nextEstimate = m.usableBins + nextEstimateBins
	m.track.add(e)
}

// Verdict is the measurement's answer about the uplink just before now.
type Verdict string

const (
	// VerdictEcho: the uplink is the bot's own voice coming back.
	VerdictEcho Verdict = "ECHO"
	// VerdictNotEcho: there was bot speech it could have been the echo of,
	// and it is not.
	VerdictNotEcho Verdict = "NOT_ECHO"
	// VerdictUnknown: nothing to judge from (no bot speech within reach, a
	// silent uplink, a failed measurement), or a correlation too weak to
	// tell with no echo delay known to hold it against. It is not "not
	// echo".
	VerdictUnknown Verdict = "UNKNOWN"
)

// Summary is what the call-end line reports.
type Summary struct {
	// IsMeasured is false when no estimate ran: the bot spoke too little, or
	// the leg offered no reference, or the measurement failed.
	IsMeasured bool
	// IsEchoFound is the published verdict: three agreeing estimates found an
	// echo.
	IsEchoFound bool
	// DelayMs and ReturnLossDb describe the published echo; when none is
	// published they describe the latest estimate's candidate, if it found
	// one (IsCandidateOnly).
	DelayMs         int
	ReturnLossDb    float64
	IsCandidateOnly bool
	// ReturnLossMinDb and ReturnLossMaxDb are the range across every
	// estimate that found an echo (EstimatesFoundEcho > 0): a far end's
	// return loss changes within a call.
	ReturnLossMinDb float64
	ReturnLossMaxDb float64
	// Estimates is the estimates that ran; EstimatesFoundEcho those of them
	// that found an echo path.
	Estimates          int
	EstimatesFoundEcho int
	// FinalRunEstimates is the length of the last run of consecutive
	// estimates that agree: all on one delay within 20 ms, or all on no echo.
	// A run of 3 publishes what it agrees on.
	FinalRunEstimates int
	// BotSpeechMs is the bot speech the estimates had.
	BotSpeechMs int64
	// ReferenceDropped is reference frames lost to a full handoff queue.
	ReferenceDropped int64
	// DumpDropped is stereo frames the dump writer was too slow for.
	DumpDropped int64
	IsFailed    bool
}

// Summary closes the dump and returns the call's totals.
func (m *Monitor) Summary() Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dump != nil {
		m.takeReference()
		m.fillReferenceTo(m.upBins)
		m.writeDump(m.upBins)
		m.dump.Close()
	}
	m.txMu.Lock()
	dropped := m.txDropped
	m.txMu.Unlock()
	s := Summary{
		IsMeasured:         m.track.estimates > 0 && !m.failed,
		IsEchoFound:        m.track.isFound,
		Estimates:          m.track.estimates,
		EstimatesFoundEcho: m.track.echoesFound,
		FinalRunEstimates:  m.track.runCount,
		BotSpeechMs:        m.activeBins * binMs,
		ReferenceDropped:   dropped,
		IsFailed:           m.failed,
	}
	if m.dump != nil {
		s.DumpDropped = m.dump.Dropped()
	}
	if m.track.echoesFound > 0 {
		s.ReturnLossMinDb = round1(returnLossDb(m.track.loudestGain))
		s.ReturnLossMaxDb = round1(returnLossDb(m.track.quietestGain))
	}
	switch {
	case m.track.isFound:
		s.DelayMs = m.track.lagBins * binMs
		s.ReturnLossDb = round1(returnLossDb(m.track.gain))
	case m.track.hasLast && m.track.lastSeen.isFound:
		s.IsCandidateOnly = true
		s.DelayMs = m.track.lastSeen.lagBins * binMs
		s.ReturnLossDb = round1(m.track.lastSeen.returnLossDb())
	}
	return s
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// fail turns the measurement off for the rest of the call and says so once.
func (m *Monitor) fail(reason any) {
	m.failed = true
	m.log.Error("echo measurement disabled for this call", "reason", reason)
}

// writeDump hands the dump every whole slot that ends at or before limit: 20
// ms of what the leg sent (left) beside the uplink frame the clock emitted
// (right), on the same bins. limit trails the newest uplink bin so the
// reference for a slot has arrived before the slot is written.
func (m *Monitor) writeDump(limit int64) {
	var left, right [frameSamples]int16
	for m.dumpNext+2 <= limit {
		for half := range 2 {
			at := ((m.dumpNext + int64(half)) % pcmBins) * binSamples
			copy(left[half*binSamples:], m.refPCM[at:at+binSamples])
			copy(right[half*binSamples:], m.upPCM[at:at+binSamples])
		}
		m.dump.Write(left[:], right[:])
		m.dumpNext += 2
	}
}
