// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"math/bits"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// The constants of a call that listens before it opens (#594). The wait is a
// platform constant, not a flow setting. The levels are the starting values
// and are tuned on a live stack: docs/design/02-ai-voice.md section 4.
const (
	// callerFirstWait is how long after the answer the opening waits for a
	// callee who says nothing.
	callerFirstWait = 2000 * time.Millisecond
	// callerFirstHardCap is how long the opening waits for a callee who is
	// still speaking at callerFirstWait. Past it the bot starts anyway.
	callerFirstHardCap = 4000 * time.Millisecond

	// listenFloorMeanSquare is -40 dBFS: a slot quieter than this is never
	// voice. 32768 squared, times 10^-4.
	listenFloorMeanSquare int64 = 107374
	// listenNoiseRatio is how far above the tracked noise floor a slot must
	// be to count as voice: 8 times the power, about 9 dB.
	listenNoiseRatio int64 = 8
	// listenNoiseShift is the speed of the noise floor's tracking: a 1/32
	// step per quiet slot.
	listenNoiseShift = 5

	// listenOnsetWindow and listenOnsetVoiced: speech has begun when this many
	// of the last listenOnsetWindow slots were voiced (5 of 7, about 100 ms),
	// which a click or a pop never reaches.
	listenOnsetWindow = 7
	listenOnsetVoiced = 5
	// listenEndSlots is how many unvoiced slots in a row end a greeting after
	// its onset: 300 ms.
	listenEndSlots = 300 / frameDurationMs

	// listenGraceMin is how long after the wait has ended the uplink keeps
	// listening: the opening is already generated and is released to the
	// caller at the end of the wait, and the second word of "喂……你好" must
	// not reach the provider as the opening starts. listenGraceCap bounds it
	// when the callee goes on speaking, and listenGraceQuietSlots is the
	// unvoiced run (100 ms) that ends a grace that has run its minimum.
	listenGraceMin        = 800 * time.Millisecond
	listenGraceCap        = 1500 * time.Millisecond
	listenGraceQuietSlots = 100 / frameDurationMs

	// maxHeldEvents bounds the model events kept while the opening waits for
	// the end of the wait: the whole opening can arrive, a few hundred events.
	// Past it the opening is let go early.
	maxHeldEvents = 4096
)

// listenVerdict is what a greetingListener has decided.
type listenVerdict uint8

// The ways the wait ends. LEG_STOPPED is not decided by the listener: it is the
// session's own (call over, context cancelled).
const (
	listenPending listenVerdict = iota
	// listenGreeting: the callee spoke and fell silent.
	listenGreeting
	// listenTimeout: nothing was said within callerFirstWait.
	listenTimeout
	// listenHardCap: the callee was still speaking at callerFirstHardCap.
	listenHardCap
)

// String is the reason as the log and the metric label it.
func (v listenVerdict) String() string {
	switch v {
	case listenGreeting:
		return "GREETING"
	case listenTimeout:
		return "TIMEOUT"
	case listenHardCap:
		return "HARD_CAP"
	}
	return "PENDING"
}

// greetingListener decides when the callee has finished their greeting, from
// the level of the G.711 slots alone: no model, no allocation, one call per
// uplink slot. It is not safe for concurrent use; the uplink pump owns it.
//
// Speech begins at an onset (5 voiced slots of the last 7) and ends after 300
// ms without a voiced slot. A slot is voiced when it is at least -40 dBFS and
// at least 9 dB above a slowly tracked noise floor. A slot with no packet is
// unvoiced: a phone that stops sending, or a media path that is not up yet,
// is silence to this listener. A callee still speaking when the wait would
// end is waited for until the hard cap; one who has not begun is not.
type greetingListener struct {
	law media.Law

	// firstSince is the time the first slot was judged; the wait is measured
	// from it for the log.
	firstSince time.Duration
	isStarted  bool

	noise        int64
	isNoiseKnown bool

	// window holds the last listenOnsetWindow slots, newest in bit 0.
	window uint8
	// isOnset is set once speech has begun.
	isOnset bool
	// unvoicedRun is the unvoiced slots in a row since the last voiced one.
	unvoicedRun int
	// voiced is every voiced slot.
	voiced int

	// speechStart and speechEnd are when speech began and when its last voiced
	// slot ended, from the answer; negative until known.
	speechStart time.Duration
	speechEnd   time.Duration
}

func newGreetingListener(law media.Law) *greetingListener {
	return &greetingListener{law: law, speechStart: -1, speechEnd: -1}
}

// feed judges one slot. since is where the slot ends, from the answer; frame
// is nil when no packet arrived for the slot. It returns listenPending until
// the wait is over, and the listener is not fed after that.
func (l *greetingListener) feed(since time.Duration, frame []byte) listenVerdict {
	if !l.isStarted {
		l.isStarted, l.firstSince = true, since
	}
	isVoiced := l.judge(frame)

	l.window <<= 1
	if isVoiced {
		l.window |= 1
		l.voiced++
		l.unvoicedRun = 0
	} else {
		l.unvoicedRun++
	}
	l.window &= 1<<listenOnsetWindow - 1

	if !l.isOnset && bits.OnesCount8(l.window) >= listenOnsetVoiced {
		l.isOnset = true
		// The oldest voiced slot of the window is where the speech began.
		l.speechStart = since - time.Duration(bits.Len8(l.window))*uplinkSlot
	}

	if l.isOnset && isVoiced {
		l.speechEnd = since
	}

	switch {
	case l.isOnset && l.unvoicedRun >= listenEndSlots:
		return listenGreeting
	case l.isOnset && since >= callerFirstHardCap:
		return listenHardCap
	case !l.isOnset && since >= callerFirstWait:
		return listenTimeout
	}
	return listenPending
}

// judge says whether one slot is voiced (nil: no packet, never) and teaches the
// noise floor from the slots that are not.
func (l *greetingListener) judge(frame []byte) bool {
	if frame == nil {
		return false
	}
	level := l.law.MeanSquare(frame)
	isVoiced := level >= listenFloorMeanSquare && level >= l.noise*listenNoiseRatio
	if !isVoiced {
		if l.isNoiseKnown {
			l.noise += (level - l.noise) >> listenNoiseShift
		} else {
			l.noise, l.isNoiseKnown = level, true
		}
	}
	return isVoiced
}

// waited is how long the listener has been judging, up to since.
func (l *greetingListener) waited(since time.Duration) time.Duration {
	if !l.isStarted {
		return 0
	}
	return since - l.firstSince
}

// voicedFor is the time of the voiced slots.
func (l *greetingListener) voicedFor() time.Duration {
	return time.Duration(l.voiced) * uplinkSlot
}

// b2i is 1 for true.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// openingGate is the gate the model waits on before it asks for the opening
// turn (provider.SessionConfig.OpeningGate), with one hold per reason to wait.
// It closes, which is to say opens the way, when every hold has been released
// once; each holder releases exactly once, on every path that ends the call.
type openingGate struct {
	ch chan struct{}

	mu      sync.Mutex
	pending int
}

func newOpeningGate(holds int) *openingGate {
	return &openingGate{ch: make(chan struct{}), pending: holds}
}

func (g *openingGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending--
	if g.pending == 0 {
		close(g.ch)
	}
}

// listenOutcome is what the listener learned, as the log reports it.
type listenOutcome struct {
	// speechStart and speechEnd are from the answer; negative when there was
	// no speech.
	speechStart, speechEnd time.Duration
	voiced                 time.Duration
}

// noSpeech is the outcome of a wait that ended without the listener deciding.
var noSpeech = listenOutcome{speechStart: -1, speechEnd: -1}

// listenToSlot hands the slot that ends n slots before the clock's owed count
// to the listener, and ends the wait when the listener has decided. It runs on
// the uplink pump and reads the slot only.
//
// The wait's end releases the opening to the caller (the downlink hold,
// pumpModelEvents), but the uplink goes on listening for a grace: at least
// listenGraceMin, longer while the callee is still heard, at most
// listenGraceCap. Then isListening clears and the pump sends from the next
// slot.
func (s *Session) listenToSlot(clock *uplinkClock, n int, frame []byte) {
	end := clock.start.Add(time.Duration(clock.slotsDone-int64(n)+1) * uplinkSlot)
	from := s.listenAnsweredAt
	if from.IsZero() {
		from = clock.start
	}
	since := end.Sub(from)
	if !s.isListenerDone {
		if verdict := s.listener.feed(since, frame); verdict != listenPending {
			l := s.listener
			s.endListening(verdict.String(), l.waited(since), listenOutcome{
				speechStart: l.speechStart, speechEnd: l.speechEnd, voiced: l.voicedFor(),
			})
		} else {
			// Released by another path (the call ending, the held opening
			// overflowing): the grace starts at this slot.
			select {
			case <-s.listenEnded:
			default:
				return
			}
		}
		s.isListenerDone, s.graceFrom = true, since
		return
	}
	if s.listener.judge(frame) {
		s.graceQuiet = 0
	} else {
		s.graceQuiet++
	}
	elapsed := since - s.graceFrom
	if elapsed >= listenGraceCap || (elapsed >= listenGraceMin && s.graceQuiet >= listenGraceQuietSlots) {
		s.isListening.Store(false)
		s.log.Info("caller-first grace ended",
			"graceMs", elapsed.Milliseconds(),
			"isExtended", elapsed > listenGraceMin,
			"isCapped", elapsed >= listenGraceCap)
	}
}

// endListening ends the wait to open, once: it logs how, records the metric
// and releases the opening held for the caller (listenEnded). Any goroutine
// may call it; the first reason wins.
func (s *Session) endListening(reason string, waited time.Duration, out listenOutcome) {
	s.listenOnce.Do(func() {
		isMediaStarted := false
		select {
		case <-s.leg.MediaStarted():
			isMediaStarted = true
		default:
		}
		ms := waited.Milliseconds()
		s.log.Info("caller-first wait ended",
			"reason", reason,
			"waitedMs", ms,
			"speechStartMs", out.speechStart.Milliseconds(),
			"speechEndMs", out.speechEnd.Milliseconds(),
			"voicedMs", out.voiced.Milliseconds(),
			"isMediaStarted", isMediaStarted)
		switch reason {
		case "GREETING", "TIMEOUT", "HARD_CAP":
			obs.RecordBotCallerFirstWait(reason, ms)
		default:
			obs.RecordBotCallerFirstWait("LEG_STOPPED", ms)
		}
		close(s.listenEnded)
	})
}

// watchListening ends the wait when the call ends or the context is cancelled
// before the listener has decided, so that nothing waits on a release nobody
// will make. It ends with the wait, or with the session.
func (s *Session) watchListening(ctx context.Context) {
	select {
	case <-s.leg.Stopped():
	case <-s.done:
	case <-ctx.Done():
	case <-s.listenEnded:
		return
	}
	s.endListening("LEG_STOPPED", time.Since(s.listenBegan), noSpeech)
}

// listensFirst says whether this call waits for the callee before it opens:
// the flow asks (global.isCallerFirst) and the platform placed the call to a
// customer. A flow that also serves an inbound DID keeps the field, and the
// inbound call ignores it.
func listensFirst(spec *flow.Spec, facts *callFacts) bool {
	return spec != nil && spec.Global.IsCallerFirst && facts.isOutboundToCustomer()
}
