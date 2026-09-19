// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// Nothing this package starts may outlive the session that started it.
//
// A leaked read loop holds a socket and a call's worth of provider state; a
// leaked pacer goes on writing audio into a session that has ended. Neither
// shows up as a failing assertion — it shows up as a process that grows over a
// day of calls — so every stop path here is checked for one.
//
// The check is a goroutine-stack diff rather than a dependency: it names the
// owners a session can leave behind and looks for them by name. The speech
// detector is not among them because it has none: it runs on the call's own
// goroutine, inside SendAudio.
var goroutineOwners = []string{
	"gemini.(*Session)",
	"gemini.(*pacer)",
	"pacer.(*Pacer)",
	"provider.(*Watchdog)",
	"wsconn.(*Conn)",
}

// noGoroutinesLeft snapshots what is running and returns the check to defer.
// Stopping is not instantaneous — a read loop ends when its socket does — so the
// check is given a window to come good in before it fails.
func noGoroutinesLeft(t *testing.T) func() {
	t.Helper()
	before := len(sessionGoroutines())

	return func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			after := sessionGoroutines()
			if len(after) <= before {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d session goroutines are still running (there were %d before):\n%s",
					len(after), before, strings.Join(after, "\n\n"))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// sessionGoroutines is every running goroutine owned by a session.
func sessionGoroutines() []string {
	buffer := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buffer, true)
		if n < len(buffer) {
			buffer = buffer[:n]
			break
		}
		buffer = make([]byte, 2*len(buffer))
	}

	var out []string
	for _, stack := range strings.Split(string(buffer), "\n\n") {
		for _, owner := range goroutineOwners {
			if strings.Contains(stack, owner) {
				out = append(out, stack)
				break
			}
		}
	}
	return out
}
