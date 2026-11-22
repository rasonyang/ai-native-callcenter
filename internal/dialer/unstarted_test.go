// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"testing"
)

// The term of #640: taken as the dialer hears of the answer, released when the
// leg's provider session start is counted (SessionCounted) or when the leg
// ends — and every ending is a drop, so nothing else can leak it.
func TestTheUnstartedTermIsTakenAtTheAnswerAndReleasedOnEveryEnd(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Win", func(s *camSpec) { s.max = 50 })
	r.importContacts(c, numbers("9", 3))
	r.start(c)
	live := r.placer.liveCalls()
	if len(live) < 3 {
		t.Fatalf("%d calls live, want the 3 placed", len(live))
	}

	read := func() int {
		t.Helper()
		var n int
		if err := r.dialer().run(context.Background(), func(context.Context) { n = r.dialer().unstarted }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := read(); n != 0 {
		t.Fatalf("unstarted = %d, want 0: ringing legs are not counted", n)
	}

	// Each answer holds its own count from the message that reports it.
	r.dialer().Answered(live[0].Req.CallID)
	r.settle()
	if n := read(); n != 1 {
		t.Fatalf("after the first answer unstarted = %d, want 1", n)
	}
	r.dialer().Answered(live[1].Req.CallID)
	r.settle()
	if n := read(); n != 2 {
		t.Fatalf("after the second answer unstarted = %d, want 2", n)
	}

	// The counted start releases it.
	r.dialer().SessionCounted(live[0].Req.CallID)
	r.settle()
	if n := read(); n != 1 {
		t.Fatalf("after the counted start unstarted = %d, want 1", n)
	}
	// ...and ending a counted leg takes nothing a second time.
	r.end(live[0], endAnswered)
	r.settle()
	if n := read(); n != 1 {
		t.Fatalf("after the counted leg ended unstarted = %d, want 1", n)
	}

	// A callee hanging up inside the window releases it by ending; an
	// answered leg never in the term (the one still ringing) is untouched.
	r.end(live[1], endHangup1s)
	r.settle()
	if n := read(); n != 0 {
		t.Fatalf("after the window hangup unstarted = %d, want 0", n)
	}
	// A count that arrives for a call that was never answered is ignored.
	r.dialer().SessionCounted(live[2].Req.CallID)
	r.settle()
	if n := read(); n != 0 {
		t.Fatalf("after a stranger's count unstarted = %d, want 0", n)
	}
	r.noViolations()
}
