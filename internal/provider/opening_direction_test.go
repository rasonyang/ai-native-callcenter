// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// #360: on qwen the opening line's direction is put in the conversation as the
// caller's first message, and used to stay there for the whole call. A turn
// later that answered tool results with nothing of its own to say sometimes
// fulfilled it again and spoke the opener word for word (qwen-findings W-Q11).
// It is taken out when the response that answers the opening request ends.

const openingLine = "Hello, this is StarCom Mobile. Is now a good moment?"

// startOpeningCall starts a session on a profile with an opening line, on a
// fake that names every caller message (announceItems) and creates the
// response.create requests numbered in created, and returns once the client
// has asked for the opening turn. The opening turn itself is left to the test.
func startOpeningCall(t *testing.T, profile Profile, text string, created ...int) (*fakeProvider, *Realtime, *syncBuffer) {
	t.Helper()
	var mu sync.Mutex
	var requests int
	f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		announceItems(f, message)
		if message["type"] != "response.create" {
			return
		}
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		for _, number := range created {
			if n == number {
				f.send(map[string]any{"type": "response.created"})
			}
		}
	})
	session, logs := loggedSession(t, f, profile)
	session.responseCreatedDeadline = unansweredDeadline
	cfg := basicConfig()
	cfg.OpeningText = text
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	f.awaitMessages("response.create", 1)
	awaitDirectionNamed(t, session)
	return f, session, logs
}

// awaitDirectionNamed waits until the client has read the provider's
// conversation.item.created for the opening's direction and learned its id.
// The fake announces the item from its read loop, and the test pushes
// response.done from its own goroutine: without this wait the two frames can
// reach the client in either order, and a direction not yet named cannot be
// taken out. A call with no named direction has nothing to wait for.
func awaitDirectionNamed(t *testing.T, session *Realtime) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		isNamed := session.openingItemID != "" || session.openingDirection == ""
		session.mu.Unlock()
		if isNamed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the client never learned the provider's name for the opening's direction")
}

// openResponse has the fake create a response and waits for the client to
// report it.
func openResponse(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
}

// endResponse ends the open response with a status and waits for the client to
// report it.
func endResponse(t *testing.T, f *fakeProvider, session *Realtime, status string) {
	t.Helper()
	f.send(map[string]any{"type": "response.done", "response": map[string]any{"status": status}})
	if status == statusCancelled {
		awaitEvent(t, session, EventTypeInterrupted)
		return
	}
	awaitEvent(t, session, EventTypeResponseDone)
}

// The defect: the direction stays the caller's first message for the whole
// call. It is the first item the client created, named user_1 by the fake, and
// goes when the opening response ends, not before, whether it completed or the
// caller took the floor and it was cancelled.
func TestTheOpeningDirectionIsTakenOutWhenTheOpeningEnds(t *testing.T) {
	for _, status := range []string{statusCompleted, statusCancelled} {
		t.Run(status, func(t *testing.T) {
			f, session, logs := startOpeningCall(t, QwenProfile(), openingLine)
			texts, ids := lineItems(t, f, openingLine)
			if len(texts) != 1 || texts[0] != SayExactly(openingLine, "en") {
				t.Fatalf("caller items %q, want the opening's direction", texts)
			}

			openResponse(t, f, session)
			f.awaitMessages("conversation.item.delete", 0)
			endResponse(t, f, session, status)

			f.awaitMessages("conversation.item.delete", 1)
			if got := deletedIDs(f); got[0] != ids[0] {
				t.Errorf("deleted %v, want the opening's direction (%s)", got, ids[0])
			}
			if !strings.Contains(logs.String(), "took the direction of the opening line out") {
				t.Errorf("no log line about taking the direction out:\n%s", logs.String())
			}
			// Once: the next turn's end deletes nothing.
			openResponse(t, f, session)
			endResponse(t, f, session, statusCompleted)
			f.awaitMessages("conversation.item.delete", 1)
		})
	}
}

// A line waiting for the floor goes out when the opening ends, and the
// direction is gone before it is asked for: the line's turn is made without
// it. The cancel the line sent to make room ends the opening as a cancelled
// response.
func TestTheOpeningDirectionIsTakenOutBeforeTheRequestsTheEndReleases(t *testing.T) {
	f, session, _ := startOpeningCall(t, QwenProfile(), openingLine)
	openResponse(t, f, session)
	if err := session.SpeakText(phaseLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 1)
	endResponse(t, f, session, statusCancelled)

	f.awaitMessages("response.create", 2)
	f.awaitMessages("conversation.item.delete", 1)
	types := typesOf(f.messages())
	if indexOf(types, "conversation.item.delete") > nthIndex(types, "response.create", 2) {
		t.Errorf("the line was asked for before the opening's direction was taken out: %v", types)
	}
}

// An opening request the provider never creates is asked for once more, with
// its direction still in the conversation: the direction stays until the
// response that finally answers it has ended, and the retry puts no second
// copy in.
func TestARetriedOpeningKeepsItsDirectionUntilTheRetryEnds(t *testing.T) {
	f, session, logs := startOpeningCall(t, QwenProfile(), openingLine, 2)
	f.awaitMessages("response.create", 2)
	if texts, _ := lineItems(t, f, openingLine); len(texts) != 1 {
		t.Fatalf("caller items %q, want the direction once", texts)
	}
	if !strings.Contains(logs.String(), "asking once more") {
		t.Fatalf("the opening was not retried:\n%s", logs.String())
	}
	// The fake created the request numbered 2 as it arrived.
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("conversation.item.delete", 0)
	endResponse(t, f, session, statusCompleted)
	f.awaitMessages("conversation.item.delete", 1)
}

// An opening request that is never created and is replaced by a line waiting
// for the floor: the line's turn is the one that answers the opening, and the
// direction goes when it ends.
func TestAnOpeningReplacedByALineKeepsItsDirectionUntilTheLineEnds(t *testing.T) {
	f, session, _ := startOpeningCall(t, QwenProfile(), openingLine, 2)
	if err := session.SpeakText(phaseLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 2)
	// The fake created the request numbered 2 as it arrived.
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("conversation.item.delete", 0)
	endResponse(t, f, session, statusCompleted)
	f.awaitMessages("conversation.item.delete", 1)
}

// An item the provider never names cannot be taken out and stays.
func TestAnOpeningDirectionNeverNamedStays(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session, logs := loggedSession(t, f, QwenProfile())
	cfg := basicConfig()
	cfg.OpeningText = openingLine
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)
	f.awaitMessages("conversation.item.delete", 0)
	if !strings.Contains(logs.String(), "opening line was never named") {
		t.Errorf("no log line about the item never named:\n%s", logs.String())
	}
}

// Out of scope and unmeasured: the bare greeting cue of a flow that names no
// opening line stays in the conversation, and the profiles that need no cue
// put nothing in it, so there is nothing to take out.
func TestNothingIsTakenOutWhereTheOpeningHasNoNamedDirection(t *testing.T) {
	t.Run("qwen without an opening line", func(t *testing.T) {
		f, session, _ := startOpeningCall(t, QwenProfile(), "")
		if n := len(f.messagesOfType("conversation.item.create")); n != 1 {
			t.Fatalf("%d caller items, want the greeting cue", n)
		}
		openResponse(t, f, session)
		endResponse(t, f, session, statusCompleted)
		f.awaitMessages("conversation.item.delete", 0)
	})
	for _, profile := range []Profile{OpenAIProfile(), GrokProfile(), GatewayProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session, _ := startOpeningCall(t, profile, openingLine)
			if n := len(f.messagesOfType("conversation.item.create")); n != 0 {
				t.Fatalf("%d caller items, want none", n)
			}
			openResponse(t, f, session)
			endResponse(t, f, session, statusCompleted)
			f.awaitMessages("conversation.item.delete", 0)
		})
	}
}
