// SPDX-License-Identifier: Apache-2.0

package provider

import "time"

// WatchSignal tells the watchdog where a response has got to.
type WatchSignal uint8

const (
	// WatchResponseStarted is a response beginning.
	WatchResponseStarted WatchSignal = iota
	// WatchAudioArrived is some of it being heard.
	WatchAudioArrived
	// WatchResponseEnded is it finishing, however it finished.
	WatchResponseEnded
)

// watchBuffer is how many progress signals may be in flight. Signals are
// droppable, so this is a cushion for a burst rather than a queue.
const watchBuffer = 16

// Watchdog ends a turn the provider has silently abandoned.
//
// A model that accepts a turn and then stops is indistinguishable, to the
// person on the phone, from a call that has died — and the flow engine would
// wait for a completion that is never coming. Rather than hang, the turn is
// closed out with what actually arrived.
//
// It knows nothing about any protocol: a client reports three things happening
// and says what to do about a turn that stops. What a stall costs is a phone
// call either way, so the shape of it is the same for every wire protocol here.
type Watchdog struct {
	cfg     WatchdogConfig
	signals chan WatchSignal
}

// WatchdogConfig is everything the watchdog needs from its client.
type WatchdogConfig struct {
	// FirstAudioDeadline bounds the wait between a response starting and the
	// first of it arriving; DeltaStallDeadline bounds a gap in the middle of
	// one, where audio has already arrived and the right answer is to finish
	// with what there is.
	FirstAudioDeadline time.Duration
	DeltaStallDeadline time.Duration

	// Done stops the watchdog: it is the session's own, so the goroutine ends
	// with the connection it watches.
	Done <-chan struct{}

	// IsResponseOpen reports whether a response is still open. The watchdog
	// consults it before abandoning anything, because its own progress signals
	// are droppable and the completion is the one that must never be missed.
	IsResponseOpen func() bool

	// OnStall is called, on the watchdog's own goroutine, for a turn that has
	// stopped. hasAudioArrived says whether any of it was heard, which is the
	// difference between a provider that never started and one that gave up
	// partway.
	OnStall func(hasAudioArrived bool)
}

// NewWatchdog builds a watchdog. Nothing runs until Run is called.
func NewWatchdog(cfg WatchdogConfig) *Watchdog {
	return &Watchdog{cfg: cfg, signals: make(chan WatchSignal, watchBuffer)}
}

// Signals is where progress arrives. It is exposed for the client that wants to
// feed it directly; Signal is the way that never blocks.
func (w *Watchdog) Signals() chan WatchSignal { return w.signals }

// Signal notifies the watchdog without ever blocking its caller.
func (w *Watchdog) Signal(s WatchSignal) {
	select {
	case w.signals <- s:
	case <-w.cfg.Done:
	default:
		// The watchdog is momentarily behind. A missed signal costs at worst
		// a late re-arm: whether a response is still open is read from state,
		// not inferred from having seen every signal.
	}
}

// Run watches until the session ends. It is one goroutine and the caller owns
// it: nothing else here starts or stops it.
func (w *Watchdog) Run() {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	hasAudioArrived := false
	isWaiting := false

	arm := func(d time.Duration) {
		if isWaiting && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		isWaiting = true
	}
	disarm := func() {
		if isWaiting && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		isWaiting = false
	}

	for {
		select {
		case <-w.cfg.Done:
			return

		case signal := <-w.signals:
			switch signal {
			case WatchResponseStarted:
				hasAudioArrived = false
				arm(w.cfg.FirstAudioDeadline)
			case WatchAudioArrived:
				hasAudioArrived = true
				arm(w.cfg.DeltaStallDeadline)
			case WatchResponseEnded:
				disarm()
			}

		case <-timer.C:
			isWaiting = false
			// Signals are droppable and a turn arrives in a burst: fifty
			// deltas can overflow the channel and take the completion with
			// them, leaving this timer armed on a response that finished
			// cleanly. The state cannot be lost the way a signal can, so it
			// is what decides. Found under sustained load, where about 1% of
			// turns were reported abandoned while the model was fine.
			if !w.cfg.IsResponseOpen() {
				continue
			}
			w.cfg.OnStall(hasAudioArrived)
		}
	}
}
