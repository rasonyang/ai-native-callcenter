// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// #310: a response the provider's watchdog gave up before any of it was heard
// did not answer the caller, although it started after their speech. The dead
// air that follows is the #448 path (answer what the caller said), not a plain
// reprompt, and only for a turn nothing of which was heard.
//

// abandon ends the response in progress the way the provider does when its
// watchdog gives it up: the turn is over for the session, flagged.
func (r *turnRig) abandon(t *testing.T) {
	t.Helper()
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone, IsAbandoned: true}
	awaitBridgeEvent(t, r.session, EventTypeTurnDone)
}

func TestADeadAirAfterANoAudioAbandonedTurnIsADroppedTurnWithTheCallersWords(t *testing.T) {
	cases := []struct {
		name string
		// wordsFirst puts the final transcript before the response starts (the
		// start clears it), otherwise it arrives while that response is the
		// latest (the timer drops it).
		wordsFirst bool
	}{
		{"words before the response", true},
		{"words after the response started", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachProfile(t, func(t *testing.T, profile provider.Profile) {
				rig := startTurnRig(t, profile, 100*time.Millisecond)
				rig.callerSpeaks(t)
				if c.wordsFirst {
					rig.transcript(t, rma1003Words)
				}
				rig.responseInProgress(t)
				if !c.wordsFirst {
					rig.transcript(t, rma1003Words)
				}
				rig.abandon(t)

				event := awaitBridgeEvent(t, rig.session, EventTypeNoInput)
				if !event.IsCallerUnanswered || event.Text != rma1003Words {
					t.Fatalf("NO_INPUT = unanswered %v, text %q; want the caller's words", event.IsCallerUnanswered, event.Text)
				}
				rig.want(t, 0, 1, "NO_INPUT")

				// The cue is answered, and the next dead air is a plain one: the
				// turn is asked about once, so repeated stalls still reach the
				// flow's NO_INPUT rules.
				if err := rig.session.SendCue("(the caller has been silent)"); err != nil {
					t.Fatal(err)
				}
				rig.reply(t)
				event = awaitBridgeEvent(t, rig.session, EventTypeNoInput)
				if event.IsCallerUnanswered || event.Text != "" {
					t.Errorf("second NO_INPUT = unanswered %v, text %q; want a plain silence", event.IsCallerUnanswered, event.Text)
				}
			})
		})
	}
}

func TestAnAbandonedTurnThatQueuedAudioStaysAnsweredAndItsDeadAirIsPlain(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.callerSpeaks(t)
		rig.transcript(t, rma1003Words)
		rig.responseInProgress(t)
		rig.model.events <- provider.Event{
			Type:  provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples*2*5),
		}
		rig.abandon(t)

		event := awaitBridgeEvent(t, rig.session, EventTypeNoInput)
		if event.IsCallerUnanswered || event.Text != "" {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want a plain silence (the caller heard part)", event.IsCallerUnanswered, event.Text)
		}
	})
}
