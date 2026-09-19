// SPDX-License-Identifier: Apache-2.0

package pacer

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// The one owner this package has. A pacer that outlived its session is found by
// name in a goroutine dump, which is a stack diff and not a dependency.
const pacerOwner = "pacer.(*Pacer)"

// noPacersLeft snapshots what is running and returns the check to defer.
// Stopping is not instantaneous — the goroutine has a tick to finish and a
// ticker to stop — so the check is given a window to come good in before it
// fails.
func noPacersLeft(t *testing.T) func() {
	t.Helper()
	before := len(pacerGoroutines())

	return func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			after := pacerGoroutines()
			if len(after) <= before {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d pacer goroutines are still running (there were %d before):\n%s",
					len(after), before, strings.Join(after, "\n\n"))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// pacerGoroutines is every running goroutine owned by a pacer.
func pacerGoroutines() []string {
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
		if strings.Contains(stack, pacerOwner) {
			out = append(out, stack)
		}
	}
	return out
}
