// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"runtime"
	"strings"
	"sync"
	syncatomic "sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// A response.create the provider never answers — no response.created, no
// error — must not hold the floor for the rest of the call (issue #231). The
// fake provider answers nothing it is not told to, so every request in these
// tests is dropped unless the test creates it.

// unansweredDeadline is the bound these tests run with: short, so a test does
// not wait seconds for behaviour that takes seconds on a real call, and long
// enough that the fake's round trip never trips it.
const unansweredDeadline = 300 * time.Millisecond

// The global meter provider can be set once; every test reads the same reader
// and compares counts before and after.
var (
	unansweredReader     = sdkmetric.NewManualReader()
	setUnansweredReaders sync.Once
)

// unansweredCounts reads aicc_provider_response_unanswered_total by
// "KIND/OUTCOME" for one provider.
func unansweredCounts(t *testing.T, provider string) map[string]int64 {
	t.Helper()
	setUnansweredReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(unansweredReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := unansweredReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_provider_response_unanswered_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if p, _ := dp.Attributes.Value("provider"); p.AsString() != provider {
					continue
				}
				kind, _ := dp.Attributes.Value("kind")
				outcome, _ := dp.Attributes.Value("outcome")
				out[kind.AsString()+"/"+outcome.AsString()] += dp.Value
			}
		}
	}
	return out
}

// assertUnanswered checks what was counted since before, and that each count
// has exactly one WARN line to go with it.
func assertUnanswered(t *testing.T, provider string, before map[string]int64, logs *syncBuffer, want map[string]int64) {
	t.Helper()
	after := unansweredCounts(t, provider)
	var total int64
	for key, n := range want {
		total += n
		if got := after[key] - before[key]; got != n {
			t.Errorf("aicc_provider_response_unanswered_total{%s} rose by %d, want %d", key, got, n)
		}
	}
	for key, n := range after {
		if _, ok := want[key]; !ok && n != before[key] {
			t.Errorf("aicc_provider_response_unanswered_total{%s} rose by %d, want 0", key, n-before[key])
		}
	}
	if got := int64(countWarnings(logs, "never created")); got != total {
		t.Errorf("%d WARN lines about an unanswered request, want %d:\n%s", got, total, logs.String())
	}
}

func countWarnings(logs *syncBuffer, words string) int {
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, words) {
			n++
		}
	}
	return n
}

// newCountingFake is a provider that accepts the session and hands every
// response.create, numbered from 1 (the greeting's), to onRequest on the
// server's goroutine. Answering a request there, the moment it arrives, keeps
// a test's timing out of the question of which request was answered.
func newCountingFake(t *testing.T, onRequest func(f *fakeProvider, n int)) *fakeProvider {
	t.Helper()
	var mu sync.Mutex
	var requests int
	return newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		acceptSession(f, message)
		if message["type"] != "response.create" || onRequest == nil {
			return
		}
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		onRequest(f, n)
	})
}

// createOn is an onRequest that creates the requests numbered in it and leaves
// every other one unanswered.
func createOn(numbers ...int) func(f *fakeProvider, n int) {
	return func(f *fakeProvider, n int) {
		for _, number := range numbers {
			if n == number {
				f.send(map[string]any{"type": "response.created"})
			}
		}
	}
}

// startUnansweredCall starts a session on a profile with the short bound and
// plays its greeting out, so the floor is free and every request from here on
// is one the fake leaves unanswered unless onRequest answers it.
func startUnansweredCall(t *testing.T, profile Profile, cfg SessionConfig,
	onRequest func(f *fakeProvider, n int)) (*fakeProvider, *Realtime, *syncBuffer) {
	t.Helper()
	f := newCountingFake(t, onRequest)
	session, logs := loggedSession(t, f, profile)
	session.responseCreatedDeadline = unansweredDeadline
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)
	return f, session, logs
}

// endTurn waits for the response the fake created and ends it.
func endTurn(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
}

// nthIndex is the index of the nth (from 1) occurrence of want, or -1.
func nthIndex(values []string, want string, nth int) int {
	for i, v := range values {
		if v == want {
			nth--
			if nth == 0 {
				return i
			}
		}
	}
	return -1
}

func floorState(session *Realtime) (isRequested bool, pending string, isOwed bool) {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.isResponseRequested, session.pendingSpeak, session.isTurnOwed
}

// textOf is the text of a caller message item.
func textOf(t *testing.T, item map[string]any) string {
	t.Helper()
	content := nested(t, item, "item", "content").([]any)[0].(map[string]any)
	return content["text"].(string)
}

// The grok defect, on a profile that sends the override alone: a mid-call
// line's request is never created. It is asked for once more, this time with
// its direction in the conversation, which is the form such an engine answers.
func TestAnUnansweredLineIsAskedAgainWithItsDirectionInTheConversation(t *testing.T) {
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), createOn(3))
	before := unansweredCounts(t, "openai")

	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	requests := f.awaitMessages("response.create", 3)
	items := f.awaitMessages("conversation.item.create", 1)
	direction := nested(t, requests[2], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the retry does not ask for the line: %q", direction)
	}
	if got := textOf(t, items[0]); got != direction {
		t.Errorf("the item carries %q, want the retry's direction %q", got, direction)
	}
	// The first request carried the override alone; the item came with the
	// retry, ahead of it.
	if _, ok := requests[1]["response"]; !ok {
		t.Errorf("the first request for the line carries no override: %v", requests[1])
	}
	sent := typesOf(f.messages())
	if at := indexOf(sent, "conversation.item.create"); at < nthIndex(sent, "response.create", 2) ||
		at > nthIndex(sent, "response.create", 3) {
		t.Errorf("client sent %v, want the item after the first request and before the retry", sent)
	}

	// The retry is answered, and that is the end of it.
	endTurn(t, f, session)
	f.awaitMessages("response.create", 3)
	assertUnanswered(t, "openai", before, logs, map[string]int64{"LINE/RETRIED": 1})
	if isRequested, _, _ := floorState(session); isRequested {
		t.Error("the floor is still claimed after the retry was answered")
	}
}

// Where the first request already put the direction in the conversation
// (IgnoresOverrideOnceTalking), the retry resends the request alone: a second
// identical caller message would ask for the line twice.
func TestARetriedLineNeverPutsItsDirectionInTheConversationTwice(t *testing.T) {
	f, session, logs := startUnansweredCall(t, GrokProfile(), grokConfig(), createOn(3))
	before := unansweredCounts(t, NameGrok)

	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	requests := f.awaitMessages("response.create", 3)
	f.awaitMessages("conversation.item.create", 1)
	if requests[1]["response"] == nil || requests[2]["response"] == nil {
		t.Errorf("the request and its retry should both carry the override: %v", requests[1:])
	}

	endTurn(t, f, session)
	assertUnanswered(t, NameGrok, before, logs, map[string]int64{"LINE/RETRIED": 1})
}

// A tool result's turn that is never created is asked for again, bare: the
// result is already in the conversation for it to read.
func TestAnUnansweredToolTurnIsAskedForAgain(t *testing.T) {
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), createOn(3))
	before := unansweredCounts(t, "openai")

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	requests := f.awaitMessages("response.create", 3)
	if _, ok := requests[2]["response"]; ok {
		t.Errorf("the retry of a bare request carries an override: %v", requests[2])
	}
	if got := len(f.messagesOfType("conversation.item.create")); got != 1 {
		t.Errorf("the client sent %d items, want only the tool result", got)
	}

	endTurn(t, f, session)
	f.awaitMessages("response.create", 3)
	assertUnanswered(t, "openai", before, logs, map[string]int64{"TOOL/RETRIED": 1})
}

// The same for a cue: its item is in once, and the request goes again.
func TestAnUnansweredCueIsAskedForAgain(t *testing.T) {
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), createOn(3))
	before := unansweredCounts(t, "openai")

	if err := session.SendUserText("(The caller pressed 0 on their keypad.)"); err != nil {
		t.Fatalf("send cue: %v", err)
	}
	f.awaitMessages("response.create", 3)
	f.awaitMessages("conversation.item.create", 1)

	endTurn(t, f, session)
	f.awaitMessages("response.create", 3)
	assertUnanswered(t, "openai", before, logs, map[string]int64{"CUE/RETRIED": 1})
}

// The greeting's request is bounded too. Where the cue is already in the
// conversation (qwen), only the request is resent.
func TestAnUnansweredOpeningTurnIsAskedForAgain(t *testing.T) {
	f := newCountingFake(t, createOn(2))
	session, logs := loggedSession(t, f, QwenProfile())
	session.responseCreatedDeadline = unansweredDeadline
	before := unansweredCounts(t, "qwen")
	if err := session.Start(t.Context(), qwenConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	f.awaitMessages("response.create", 2)
	f.awaitMessages("conversation.item.create", 1)
	endTurn(t, f, session)
	assertUnanswered(t, "qwen", before, logs, map[string]int64{"OPENING/RETRIED": 1})
}

// A line asked for while the unanswered request holds the floor supersedes it,
// as a newer line always does: the dropped request is not retried, and the line
// goes out once, within the bound. Of two lines, only the later one is said.
func TestALineQueuedBehindAnUnansweredRequestSupersedesIt(t *testing.T) {
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), createOn(3))
	before := unansweredCounts(t, "openai")

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	askedAt := time.Now()
	if err := session.SpeakText("One moment please.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak again: %v", err)
	}

	// Within the bound, with room for the fake's round trip.
	deadline := time.Now().Add(unansweredDeadline + 500*time.Millisecond)
	for len(f.messagesOfType("response.create")) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if waited := time.Since(askedAt); len(f.messagesOfType("response.create")) < 3 {
		t.Fatalf("the waiting line was not asked for within %v", waited)
	}
	awaitEvent(t, session, EventTypeResponseStarted)

	requests := f.awaitMessages("response.create", 3)
	direction := nested(t, requests[2], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") || strings.Contains(direction, "One moment") {
		t.Errorf("the request after the drop is not the later line alone: %q", direction)
	}
	f.awaitMessages("response.cancel", 0)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	f.awaitMessages("response.create", 3)
	assertUnanswered(t, "openai", before, logs, map[string]int64{"TOOL/SUPERSEDED": 1})
}

// The ending the issue was about: a closing line waits behind a request that is
// never created. It goes out, with its direction in the conversation where the
// profile needs that, and once it has played the floor is free.
func TestAnArmedClosingLineBehindAnUnansweredRequestGoesOut(t *testing.T) {
	f, session, logs := startUnansweredCall(t, QwenProfile(), qwenConfig(), createOn(3))
	f.awaitMessages("conversation.item.create", 1) // the opening cue
	before := unansweredCounts(t, "qwen")

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	if err := session.SpeakText("感谢来电，再见。", true); err != nil {
		t.Fatalf("speak: %v", err)
	}

	requests := f.awaitMessages("response.create", 3)
	items := f.messagesOfType("conversation.item.create")
	if len(items) != 3 {
		t.Fatalf("the client sent %d items, want the cue, the tool result and the line's", len(items))
	}
	assertLineIsAlsoInTheConversation(t, f, items, requests, "感谢来电，再见。")

	endTurn(t, f, session)
	f.awaitMessages("response.create", 3)
	if isRequested, pending, isOwed := floorState(session); isRequested || pending != "" || isOwed {
		t.Errorf("after the closing line the floor is requested=%v pending=%q owed=%v, want free",
			isRequested, pending, isOwed)
	}
	assertUnanswered(t, "qwen", before, logs, map[string]int64{"TOOL/SUPERSEDED": 1})
}

// The request was only late. Its response.created arrives just after the
// retry went out, and the provider refuses the retry because that response is
// running. The refusal is the answer to our own retry: no error is reported,
// nothing more is asked for, and the call has exactly one turn.
func TestALateCreatedAfterTheRetryMakesOneTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile Profile
		cfg     SessionConfig
		refusal map[string]any
	}{
		{"openai", OpenAIProfile(), basicConfig(), map[string]any{
			"type": "invalid_request_error", "code": "conversation_already_has_active_response",
			"message": "Conversation already has an active response in progress: resp_1. " +
				"Wait until the response is finished before creating a new one.",
		}},
		{"qwen", QwenProfile(), qwenConfig(), map[string]any{
			"type": "invalid_request_error", "code": "invalid_value",
			"message": "Cannot create response while another response is in progress",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := make(chan struct{})
			f := newCountingFake(t, func(f *fakeProvider, n int) {
				// The third request is the retry: the late original
				// arrives just behind it, and the retry is refused.
				if n == 3 {
					f.send(map[string]any{"type": "response.created"})
					f.send(map[string]any{"type": "error", "error": tc.refusal})
					close(refused)
				}
			})
			session, logs := loggedSession(t, f, tc.profile)
			session.responseCreatedDeadline = unansweredDeadline
			if err := session.Start(t.Context(), tc.cfg); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)
			finishTheOpeningTurn(t, f, session)
			before := unansweredCounts(t, tc.profile.Name)

			if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			awaitEvent(t, session, EventTypeResponseStarted)
			// A provider refuses the retry while the late turn is running,
			// so the refusal is on the wire before that turn's end. The
			// fake writes it from its own goroutine: wait for it, or the
			// end can overtake it.
			<-refused
			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": "completed"}})

			for _, event := range drainUntil(t, session, EventTypeResponseDone) {
				switch event.Type {
				case EventTypeError:
					t.Errorf("the refusal of our own retry was reported: %v", event.Err)
				case EventTypeResponseStarted:
					t.Error("a second turn started")
				}
			}
			f.awaitMessages("response.create", 3)
			if !strings.Contains(logs.String(), "refused the request sent in place of one it was only late to create") {
				t.Errorf("the refusal was not recognised as ours:\n%s", logs.String())
			}
			if isRequested, _, _ := floorState(session); isRequested {
				t.Error("the floor is still claimed after the late turn ended")
			}
			assertUnanswered(t, tc.profile.Name, before, logs, map[string]int64{"TOOL/RETRIED": 1})

			// And the floor works as before: a line now goes straight out.
			if err := session.SpeakText("I am putting you through now.", false); err != nil {
				t.Fatalf("speak: %v", err)
			}
			f.awaitMessages("response.create", 4)
		})
	}
}

// The same race with a line in the picture: the tool turn was only late, and
// the line sent in its place is refused because that turn is running. The line
// is still the newer thing to say, so it pre-empts the late turn as any line
// does, and is asked for when that turn ends.
func TestALineRefusedForALateTurnIsAskedForWhenThatTurnEnds(t *testing.T) {
	f := newCountingFake(t, func(f *fakeProvider, n int) {
		switch n {
		case 3:
			f.send(map[string]any{"type": "response.created"})
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "code": "conversation_already_has_active_response",
				"message": "Conversation already has an active response in progress: resp_1.",
			}})
		case 4:
			f.send(map[string]any{"type": "response.created"})
		}
	})
	session, logs := loggedSession(t, f, OpenAIProfile())
	session.responseCreatedDeadline = unansweredDeadline
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}

	// The late turn is stopped to make room for the line.
	f.awaitMessages("response.cancel", 1)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	event := awaitEvent(t, session, EventTypeInterrupted)
	if event.InterruptedBy != InterruptReasonSystem {
		t.Errorf("interruptedBy = %q, want the application's own doing", event.InterruptedBy)
	}

	requests4 := f.awaitMessages("response.create", 4)
	direction := nested(t, requests4[3], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the line was not asked for again: %q", direction)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("an error was logged:\n%s", logs.String())
	}
}

// Two drops in a row: the retry is never created either. The request is given
// up and the floor freed, and a cue that arrived during the retry's wait still
// gets its turn.
func TestTwoDropsInARowGiveTheRequestUp(t *testing.T) {
	// The fake's goroutine needs the session the test is about to make; a
	// socket carries no happens-before the race detector can see.
	var sessionRef syncatomic.Pointer[Realtime]
	var cueErr error
	cueSent := make(chan struct{})
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), func(f *fakeProvider, n int) {
		switch n {
		case 3:
			// A keypress during the retry's wait: its reply is owed.
			cueErr = sessionRef.Load().SendUserText("(The caller pressed 0 on their keypad.)")
			close(cueSent)
		case 4:
			f.send(map[string]any{"type": "response.created"})
		}
	})
	sessionRef.Store(session)
	before := unansweredCounts(t, "openai")

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	<-cueSent
	if cueErr != nil {
		t.Fatalf("send cue: %v", cueErr)
	}
	// The retry expires: the turn owed to the cue is asked for.
	f.awaitMessages("response.create", 4)
	endTurn(t, f, session)
	f.awaitMessages("response.create", 4)
	assertUnanswered(t, "openai", before, logs, map[string]int64{
		"TOOL/RETRIED": 1, "TOOL/DROPPED": 1,
	})
}

// Two drops with nothing waiting: the floor is simply free again, and the next
// request is asked for at once and gets a bound and a retry of its own.
func TestTwoSeparateRequestsEachDroppedAreEachGivenUp(t *testing.T) {
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), nil)
	before := unansweredCounts(t, "openai")

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 3)
	time.Sleep(2 * unansweredDeadline)
	f.awaitMessages("response.create", 3)
	if isRequested, pending, isOwed := floorState(session); isRequested || pending != "" || isOwed {
		t.Fatalf("after two drops the floor is requested=%v pending=%q owed=%v, want free",
			isRequested, pending, isOwed)
	}

	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 5)
	time.Sleep(2 * unansweredDeadline)
	f.awaitMessages("response.create", 5)
	if isRequested, _, _ := floorState(session); isRequested {
		t.Error("the floor is still claimed after the second request was given up")
	}
	assertUnanswered(t, "openai", before, logs, map[string]int64{
		"TOOL/RETRIED": 1, "TOOL/DROPPED": 1, "LINE/RETRIED": 1, "LINE/DROPPED": 1,
	})
}

// A request answered in time is never retried, and its bound leaves nothing
// behind.
func TestARequestCreatedInTimeIsNotRetried(t *testing.T) {
	f, session, logs := startUnansweredCall(t, OpenAIProfile(), basicConfig(), createOn(2))
	before := unansweredCounts(t, "openai")

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	endTurn(t, f, session)
	time.Sleep(3 * unansweredDeadline)
	f.awaitMessages("response.create", 2)
	assertUnanswered(t, "openai", before, logs, map[string]int64{})
}

// A request in flight when the session closes leaves no timer to fire and no
// goroutine behind.
func TestAnUnansweredRequestLeavesNothingBehindAfterClose(t *testing.T) {
	// Goroutines a previous test left exiting only lower the count, so the
	// baseline is read at once and the end is allowed to settle.
	baseline := runtime.NumGoroutine()
	settle := func(limit int) int {
		deadline := time.Now().Add(2 * time.Second)
		n := runtime.NumGoroutine()
		for n > limit && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}

	f := newFakeProvider(t, acceptSession)
	session, logs := loggedSession(t, f, OpenAIProfile())
	session.responseCreatedDeadline = unansweredDeadline
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	f.awaitMessages("response.create", 1)
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	for range session.Events() {
	}
	f.closeOnce.Do(func() { f.server.Close() })

	time.Sleep(3 * unansweredDeadline)
	if n := countWarnings(logs, "never created"); n != 0 {
		t.Errorf("the bound fired after the session closed:\n%s", logs.String())
	}
	if n := settle(baseline); n > baseline {
		buf := make([]byte, 1<<16)
		t.Errorf("%d goroutines after close, %d before:\n%s", n, baseline, buf[:runtime.Stack(buf, true)])
	}
}
