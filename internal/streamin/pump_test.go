// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"testing"
	"time"
)

// A hole in a transcript reads the same whichever way the audio was lost, and
// the three ways want opposite fixes. These pin that the accounting can tell
// them apart, because the first thing anyone will do with C14 is read this log
// line and decide what to change.
func TestTheAccountingSaysHowTheAudioWasLost(t *testing.T) {
	t.Run("one stall shows as one run of drops", func(t *testing.T) {
		p := &pump{frames: make(chan []byte, 2), done: make(chan struct{})}
		// The reader is not running: everything past the queue is a drop, and
		// it is all one uninterrupted run.
		for range 6 {
			p.write([]byte{0, 0})
		}
		if got := p.dropped.Load(); got != 4 {
			t.Errorf("dropped = %d, want 4", got)
		}
		if got := p.dropRuns.Load(); got != 1 {
			t.Errorf("dropRuns = %d, want 1 — one stall is not four separate losses", got)
		}
	})

	t.Run("audio flowing again starts a new run", func(t *testing.T) {
		p := &pump{frames: make(chan []byte, 1), done: make(chan struct{})}
		p.write([]byte{0, 0}) // fills
		p.write([]byte{0, 0}) // drops: run 1
		<-p.frames            // the reader catches up
		p.write([]byte{0, 0}) // fits, ending the run
		p.write([]byte{0, 0}) // drops again: run 2

		if got := p.dropRuns.Load(); got != 2 {
			t.Errorf("dropRuns = %d, want 2 — two separate stalls", got)
		}
	})

	t.Run("a send as long as the frame it carries counts as slow", func(t *testing.T) {
		p := &pump{}
		p.recordSend(frameInterval - time.Millisecond)
		if got := p.sendSlow.Load(); got != 0 {
			t.Errorf("slowSends = %d, want 0 — that send kept up", got)
		}
		p.recordSend(250 * time.Millisecond)
		if got := p.sendSlow.Load(); got != 1 {
			t.Errorf("slowSends = %d, want 1", got)
		}
		if got := p.sendMaxMs.Load(); got != 250 {
			t.Errorf("maxSendMs = %d, want 250 — the worst send is the one that matters", got)
		}
		// A quicker send afterwards must not erase the worst one.
		p.recordSend(time.Millisecond)
		if got := p.sendMaxMs.Load(); got != 250 {
			t.Errorf("maxSendMs = %d after a fast send, want it to stand at 250", got)
		}
	})
}
