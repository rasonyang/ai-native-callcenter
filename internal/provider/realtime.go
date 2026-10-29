// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	syncatomic "sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

// Watchdogs on a response. A provider that accepts a turn and then goes quiet
// is indistinguishable from a broken call to the person listening.
const (
	// firstAudioTimeout bounds the wait between asking for a response and
	// hearing the first of it.
	firstAudioTimeout = 3 * time.Second
	// deltaStallTimeout bounds a gap in the middle of a response. Audio has
	// already arrived, so the right answer is to finish with what there is.
	deltaStallTimeout = 2 * time.Second
	// eventBuffer is how far the consumer may fall behind. The consumer is a
	// per-call actor forwarding to a paced send queue, so it should never come
	// close.
	eventBuffer = 256
	// responseCreatedTimeout bounds the wait between this client sending a
	// response.create and the provider creating the response. An engine may
	// drop a request without a word (grok-findings 5.2: no response.created,
	// no error), and nothing else would end that wait: the watchdog watches a
	// response that is open, and this one never opens. See onRequestExpired.
	//
	// Measured 2026-10-19, request sent to response.created reported, mid-call
	// (TestLiveResponseCreatedLatency, nearest-rank figures): openai n=8,
	// median 232 ms, max 561 ms; qwen n=8, median 180 ms, max 550 ms; grok
	// n=4, median 272 ms, max 351 ms, with 4 more bare requests that grok
	// never answered within 30 s (every other one, each followed by one it
	// did answer). Earlier dev-stack logs: qwen n=12, median 236 ms, max
	// 445 ms; grok n=1, 125 ms. The bound is four times the largest sample,
	// rounded up to a whole second: long enough that a slow answer is not
	// mistaken for a drop, short enough that a dropped line is not heard as a
	// call gone dead.
	responseCreatedTimeout = 3 * time.Second
)

// responseKind is what asked for a response: the label the WARN and the
// aicc_provider_response_unanswered_total metric name an unanswered request by.
type responseKind string

const (
	// responseKindOpening is the greeting Start asks for.
	responseKindOpening responseKind = "OPENING"
	// responseKindLine is a line SpeakText asked for.
	responseKindLine responseKind = "LINE"
	// responseKindTool is the turn a tool result asks for (SendToolResult).
	responseKindTool responseKind = "TOOL"
	// responseKindCue is the reply to a text cue (SendUserText).
	responseKindCue responseKind = "CUE"
	// responseKindOwed is a turn owed to a tool result or a cue, asked for once
	// the floor came free (releaseOwedTurn).
	responseKindOwed responseKind = "OWED"
)

// responseRequest describes one response.create: what asked for it and, for a
// line, how it was asked, which is what asking again needs.
type responseRequest struct {
	kind responseKind
	// text and isClosing are a line's, as SpeakText was given them. direction
	// is the SayExactly demand the request carries as its override; empty for
	// a bare request.
	text      string
	isClosing bool
	direction string
	// isDirectionInConversation means the direction was also put in the
	// conversation as a caller message ahead of the request (askForLine, or
	// the opening cue where the profile needs one).
	isDirectionInConversation bool
	// isRetry marks the one retry of a request that was never created.
	isRetry bool
	// isAskedAgain marks the one request for a line asked for again because
	// the turn of its first request called a tool and said nothing
	// (askForLineAgain).
	isAskedAgain bool
}

// Realtime is a session against any provider speaking the realtime protocol.
// Vendor differences live entirely in its Profile.
type Realtime struct {
	profile Profile
	apiKey  string
	log     *slog.Logger

	events chan Event
	conn   *wsconn.Conn

	// ready closes when the provider has accepted the session configuration.
	ready     chan struct{}
	readyOnce sync.Once
	// startErr records why the handshake failed, if it did.
	startErr atomic[error]
	// isResponseOpen is true between a response starting and its completion
	// being handled. The watchdog consults it before abandoning anything,
	// because its own progress signals are droppable and the completion is
	// the one that must never be missed.
	isResponseOpen syncatomic.Bool

	closeOnce sync.Once

	mu sync.Mutex
	// cfg is fixed at Start. Some providers freeze turn detection after the
	// first audio frame, so nothing here is adjusted mid-call except the
	// instructions.
	cfg          SessionConfig
	instructions string
	// responseItemID identifies the assistant's current audio item, which is
	// what truncation refers to. It belongs to one response: it is cleared when
	// the next response is created and once the item has been truncated, so a
	// trim never lands on an item the caller heard in full earlier.
	responseItemID string
	// isSessionRetried guards the one-shot retry of a rejected configuration.
	isSessionRetried bool

	// The three fields a pre-empting spoken line needs. See SpeakText.
	//
	// pendingSpeak is a line waiting for the floor; there is at most one,
	// because a newer line supersedes an older one rather than joining a
	// queue. isResponseRequested covers the window between asking for a turn
	// and the provider creating it, in which there is nothing to cancel and a
	// second request would be refused. That window is bounded: a request the
	// provider never creates gives the floor back after
	// responseCreatedDeadline (onRequestExpired). isPreemptedForSpeak
	// remembers that the turn now ending was stopped to make room, which is
	// what keeps the interruption from being reported as the caller's.
	pendingSpeak string
	// isPendingSpeakClosing is SpeakText's isClosing for pendingSpeak.
	isPendingSpeakClosing bool
	isResponseRequested   bool
	isCancelSentForSpeak  bool
	isPreemptedForSpeak   bool

	// The fields a request for a turn made while the floor is taken needs. See
	// SendToolResult, SendUserText and releaseOwedTurn.
	//
	// isTurnOwed means something is in the conversation that wants a turn
	// (a tool result, or a cue) and the turn it asks for has not been
	// requested yet, because a response still held the floor when it arrived
	// (W-Q4). It is a flag, not a count: however many results and cues one
	// response produced, they are answered in one turn.
	// isOwedPastCreate marks a turn owed while a request was in flight and
	// nothing was open yet. The response that request makes was asked for
	// before the item, so it is not the turn that was owed and its creation
	// must not discharge it.
	// isCallerSpeaking means the provider has reported the caller starting to
	// speak and neither the end of that speech nor a new response since. Its
	// own turn detection answers that speech with a response of its own.
	isTurnOwed       bool
	isOwedPastCreate bool
	isCallerSpeaking bool

	// The fields that bound the wait for response.created. See
	// onRequestExpired.
	//
	// request is the response.create isResponseRequested stands for.
	// requestGen numbers every request, so a deadline that fires after its
	// request was created, failed or was replaced knows it is stale.
	// requestTimer is that request's deadline and requestSentAt when it was
	// claimed.
	request       responseRequest
	requestGen    uint64
	requestTimer  *time.Timer
	requestSentAt time.Time
	// recoverySentAt is when this client last sent a request in place of one
	// that was never created: its retry, or a line that superseded it. If the
	// first one was only late, the provider refuses the one sent in its place
	// (see isAnsweredRecovery). recoveryLine is that superseding line, if a
	// line is what was sent, so it can be asked for again.
	recoverySentAt time.Time
	recoveryLine   *responseRequest

	// callItems are the function calls the response now open has begun and not
	// yet finished, by item id (the call id as the value), and finishedCalls the
	// ones whose arguments did arrive, by both ids. They belong to one response:
	// response.created clears them and response.done settles them. See
	// removeUnfinishedCalls.
	callItems     map[string]string
	finishedCalls map[string]struct{}
	// isLineResponse says the response now open is the one a line asked for
	// (onResponseCreated). A function call it makes is never handed out, and
	// it is taken out of the conversation when the response ends; see
	// onLineCall.
	isLineResponse bool
	// lineTurn is the request that response was created for, and lineCalls
	// the tools it called instead of only saying the line, each named once:
	// what asking for the line again needs (askForLineAgain). They belong to
	// one response, as isLineResponse does.
	lineTurn  responseRequest
	lineCalls []string
	// againDirection is the direction of the line asked for again, put in the
	// conversation as a caller message, until the provider names that item
	// (conversation.item.created); againItemID is its name. The item steers
	// that one turn and is taken back out when the turn ends
	// (removeAgainDirection).
	againDirection string
	againItemID    string
	// openingDirection is the direction of the opening line, put in the
	// conversation as the caller's first message where the profile needs a cue
	// for its first turn (NeedsCueForFirstTurn) and a line is named, until the
	// provider names that item; openingItemID is its name. isOpeningResponse says
	// the response now open answers the opening request. isOpeningDropped says
	// the opening request was never created and a line took its place, so the
	// next response is the one that answers it. The item steers the opening
	// turn and is taken back out when that turn ends (removeOpeningDirection).
	openingDirection  string
	openingItemID     string
	isOpeningResponse bool
	isOpeningDropped  bool

	// spokenItems are the items the response now open has begun to speak
	// (output_item.added of a message), and unheardItems the ones the
	// consumer has reported the caller heard none of (Interrupt with 0 ms)
	// while that response was still open. hasResponseAudio says audio of the
	// response now open has reached the consumer. They belong to one response:
	// response.created clears them and response.done settles them. See
	// removeUnheardTurn.
	spokenItems      []string
	unheardItems     []string
	hasResponseAudio syncatomic.Bool

	// The fields that tell the model a tool's answer went unheard. See
	// noteUnheardResult.
	//
	// isResultUnheard means a tool result is in the conversation and no turn
	// made with it in view has yet run to its end with its audio handed to
	// the consumer. isAnswerResponse says the response now open is such a
	// turn (not a line's), and answerItemID is the spoken item of the latest
	// one, which is what lets a later Interrupt with 0 ms recognise it.
	// isUnheardNoted means the note has been put in the conversation for the
	// results now waiting, so a second cut turn does not repeat it.
	isResultUnheard  bool
	isAnswerResponse bool
	answerItemID     string
	isUnheardNoted   bool

	// deleteSentAt is when this client last asked the provider to delete an
	// item, which is what lets a refusal of that be recognised; see
	// isAnsweredDelete.
	deleteSentAt time.Time

	// cancelSentAt is when this client last asked the provider to cancel a
	// response. It is what lets a refusal of that cancel be recognised as
	// the answer to our own request; see isAnsweredCancel.
	cancelSentAt time.Time
	// cancelReason is why that cancel was sent, so the provider's confirmation
	// of it (a response that ends cancelled) names the decision that caused it
	// rather than the default, speech. Spent when a response ends.
	cancelReason InterruptReason

	// dog ends a turn the provider has walked away from, and watch is the
	// channel its progress signals travel on.
	dog   *Watchdog
	watch chan WatchSignal
	// Watchdog deadlines, held as fields so tests need not wait seconds for
	// behaviour that is measured in seconds on a real call.
	firstAudioDeadline time.Duration
	deltaStallDeadline time.Duration
	// responseCreatedDeadline is responseCreatedTimeout, held the same way.
	responseCreatedDeadline time.Duration
}

// The names this client knows the watchdog's signals by.
const (
	watchResponseStarted = WatchResponseStarted
	watchAudioArrived    = WatchAudioArrived
	watchResponseEnded   = WatchResponseEnded
)

// atomic is a tiny typed holder; sync/atomic's generic Pointer would need a
// pointer indirection for an interface value.
type atomic[T any] struct {
	mu sync.Mutex
	v  T
}

func (a *atomic[T]) set(v T) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomic[T]) get() T  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// New builds a session for a profile. The credential is read from the
// environment named by the profile, so a missing key fails here rather than
// mid-call.
func New(profile Profile, log *slog.Logger) (*Realtime, error) {
	apiKey := os.Getenv(profile.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("provider %s: %s is not set", profile.Name, profile.APIKeyEnv)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Realtime{
		profile: profile,
		apiKey:  apiKey,
		log:     log.With("provider", profile.Name, "model", profile.Model),
		events:  make(chan Event, eventBuffer),
		ready:   make(chan struct{}),

		firstAudioDeadline:      firstAudioTimeout,
		deltaStallDeadline:      deltaStallTimeout,
		responseCreatedDeadline: responseCreatedTimeout,
	}, nil
}

func (r *Realtime) Events() <-chan Event { return r.events }

// Start connects, configures the session, and asks for the opening turn.
func (r *Realtime) Start(ctx context.Context, cfg SessionConfig) error {
	r.mu.Lock()
	r.cfg = cfg
	r.instructions = cfg.Instructions
	r.mu.Unlock()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+r.apiKey)

	conn, err := wsconn.Dial(ctx, r.profile.endpointURL(), headers, r.log)
	if err != nil {
		return err
	}
	r.conn = conn
	r.dog = NewWatchdog(WatchdogConfig{
		FirstAudioDeadline: r.firstAudioDeadline,
		DeltaStallDeadline: r.deltaStallDeadline,
		Done:               conn.Done(),
		IsResponseOpen:     r.isResponseOpen.Load,
		OnStall:            r.onResponseStalled,
	})
	r.watch = r.dog.Signals()
	go r.readLoop()
	go r.dog.Run()

	if err := r.sendEvent(r.buildSessionUpdate(cfg, false)); err != nil {
		r.conn.Close()
		return fmt.Errorf("configure session: %w", err)
	}

	// The configuration must be accepted before any audio is sent, because on
	// at least one provider the turn-detection settings freeze at the first
	// frame.
	select {
	case <-r.ready:
	case <-ctx.Done():
		r.conn.Close()
		return ctx.Err()
	case <-time.After(wsconn.DialTimeout):
		r.conn.Close()
		if err := r.startErr.get(); err != nil {
			return fmt.Errorf("session rejected: %w", err)
		}
		return errors.New("provider did not confirm the session configuration")
	}
	if err := r.startErr.get(); err != nil {
		r.conn.Close()
		return fmt.Errorf("session rejected: %w", err)
	}

	// The session is ready; the request for the first turn may still be held
	// back (SessionConfig.OpeningGate). The handshake deadline above is over,
	// so a long wait is not a timeout, and nothing is sent meanwhile.
	if err := AwaitOpeningGate(ctx, cfg.OpeningGate, r.conn.Done()); err != nil {
		r.conn.Close()
		return fmt.Errorf("wait to greet: %w", err)
	}

	// The opening turn is the flow's first node speaking; the caller is
	// already on the line waiting to be greeted.
	//
	// Two shapes, and the flow chooses. Saying nothing about the words leaves
	// the model to write its greeting from the standing instructions, which is
	// what every call did before a phase could carry a line — and the request
	// is the bare one it always was. Naming a line asks for that line instead.
	opening := map[string]any{"type": "response.create"}
	request := responseRequest{kind: responseKindOpening}
	cue := greetingCue(cfg)
	if cfg.OpeningText != "" {
		direction := sayExactly(cfg.OpeningText, cfg.Language)
		opening["response"] = map[string]any{"instructions": direction}
		request.text, request.direction = cfg.OpeningText, direction
		// Where the conversation may not be empty, the direction is also the
		// cue. Anything else would steer the same turn two ways at once, and
		// there is no second thing worth saying to a provider that is about to
		// be handed the sentence anyway.
		cue = direction
	}
	if r.profile.NeedsCueForFirstTurn {
		if cfg.OpeningText != "" {
			// Learned from the event that announces the item, which can
			// arrive before sendEvent returns (noteCreatedItem).
			r.mu.Lock()
			r.openingDirection, r.openingItemID = cue, ""
			r.mu.Unlock()
		}
		if err := r.sendEvent(userTextItem(cue)); err != nil {
			r.conn.Close()
			return fmt.Errorf("prompt opening turn: %w", err)
		}
		request.isDirectionInConversation = true
	}
	if err := r.requestResponse(request, opening); err != nil {
		r.conn.Close()
		return fmt.Errorf("request opening turn: %w", err)
	}
	return nil
}

// userTextItem is the frame that puts text in the conversation as a caller
// message: a cue, a keypress, a directed line.
func userTextItem(text string) map[string]any {
	return map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": text}},
		},
	}
}

// greetingCue is the stage direction that gets a provider talking first.
func greetingCue(cfg SessionConfig) string {
	if strings.HasPrefix(strings.ToLower(cfg.Language), "zh") {
		return "（电话已接通，请按照你的指示开始问候来电者。）"
	}
	return "(The call has connected. Greet the caller as instructed.)"
}

// SendAudio forwards one chunk of caller audio.
//
// The JSON is assembled directly around the base64 rather than marshalled from
// a struct: this runs fifty times a second per call in each direction, and
// marshalling would copy every frame twice more for no benefit. Base64's
// alphabet needs no JSON escaping, so this is safe as well as cheap.
func (r *Realtime) SendAudio(audio []byte) error {
	const prefix = `{"type":"input_audio_buffer.append","audio":"`

	message := make([]byte, 0, len(prefix)+base64.StdEncoding.EncodedLen(len(audio))+2)
	message = append(message, prefix...)
	message = base64.StdEncoding.AppendEncode(message, audio)
	message = append(message, '"', '}')

	return r.conn.Send(message)
}

// SendUserText adds a caller turn that was not spoken and asks for a reply.
//
// While a response holds the floor, or one has been asked for and not yet
// created, a second request is refused, and the refusal comes back as an async
// error that nothing retries: the item would sit in the conversation
// unanswered. So the reply is owed and asked for when that response ends,
// exactly as a tool result's is (SendToolResult).
//
// After an interruption of our own the response stays open until the provider
// confirms the cancel, so the reply to a keypress that cancelled a turn waits
// one round trip for it. A request sent straight behind the cancel was
// accepted by qwen, but that is the one provider it was measured on, and the
// cue cannot tell here whether a cancel just went out; the wait is the
// cheaper mistake than a refused cue.
//
// A request the cue's reply rides on, or its own, that the provider never
// creates is not waited on for ever: onRequestExpired retries it once and then
// asks for whatever is still owed.
func (r *Realtime) SendUserText(text string) error {
	if err := r.sendEvent(userTextItem(text)); err != nil {
		return err
	}
	// Decided after the item is sent, as SendToolResult does, so a turn
	// released by the read loop is never requested ahead of the item it answers.
	r.mu.Lock()
	isOpen := r.isResponseOpen.Load()
	isOwed := isOpen || r.isResponseRequested || r.isTurnOwed
	if isOwed {
		r.isTurnOwed = true
		if !isOpen && r.isResponseRequested {
			r.isOwedPastCreate = true
		}
	}
	r.mu.Unlock()
	if isOwed {
		r.log.Debug("a response holds the floor; the reply to this cue waits for it to end")
		return nil
	}
	return r.requestResponse(responseRequest{kind: responseKindCue},
		map[string]any{"type": "response.create"})
}

// SpeakText says a line the flow chose, in place of whatever is being said now.
//
// This protocol has no way to hand a provider audio to play, so the nearest
// thing it offers is a turn of its own with a per-response instruction: the
// model is told to repeat one sentence and nothing else. That is best effort
// rather than verbatim, and it is the honest limit of this client — an engine
// that speaks text outright gets its own profile and says the line as written.
// Where the override alone loses to the conversation already there, a line
// that ends the call is put in the conversation too (askForLine). A turn that
// calls a tool instead of saying the line, and says nothing, is asked for once
// more, with the direction in the conversation for that turn only
// (askForLineAgain).
//
// The floor may already be taken, and asking for a second response while one
// is open is refused with "conversation already has an active response". Two
// states have to be told apart:
//
//   - a response is open: it is cancelled now, and the request for the line
//     goes out when that response's own response.done arrives. The cancel is
//     sent whatever the profile says about barge-in — CancelsResponseItself
//     describes a provider that stops when it hears the CALLER, and nobody
//     here is speaking.
//   - a response has been asked for and not yet created: there is nothing to
//     cancel yet, so the line waits, and the turn is stopped the moment it
//     exists. SendToolResult asks for a turn on its way out, and a phase
//     change is decided on that very result, so this is the ordinary case
//     rather than the exotic one. The wait is bounded: if the provider never
//     creates that turn, the line is asked for once responseCreatedDeadline
//     has passed (onRequestExpired).
//
// When the floor is free it is claimed under the same lock that found it
// free, before anything is written: a second SpeakText arriving while the
// line's frames are still going out finds it taken and waits, rather than
// asking for a second response the provider would refuse.
//
// The read loop owns both deferred sends. Nothing is emitted from here, for
// the reason Interrupt gives: this is normally called from the goroutine
// draining Events, and emitting would deadlock it.
func (r *Realtime) SpeakText(text string, isClosing bool) error {
	if text == "" {
		return nil
	}

	r.mu.Lock()
	// An owed tool turn counts as a turn requested: the floor is spoken for,
	// and asking now would be the second request the provider refuses.
	isDeferred := r.isResponseOpen.Load() || r.isResponseRequested || r.isTurnOwed
	isCancelNeeded := r.isResponseOpen.Load() && !r.isCancelSentForSpeak
	if isCancelNeeded {
		r.isCancelSentForSpeak = true
		r.isPreemptedForSpeak = true
	}
	r.pendingSpeak, r.isPendingSpeakClosing = "", false
	var request responseRequest
	if isDeferred {
		r.pendingSpeak, r.isPendingSpeakClosing = text, isClosing
	} else {
		request = r.lineRequestLocked(text, isClosing)
		r.claimFloorLocked(request)
	}
	r.mu.Unlock()

	if isCancelNeeded {
		if err := r.sendCancel(InterruptReasonSystem); err != nil {
			return err
		}
	}
	if isDeferred {
		return nil
	}
	return r.askForLine(request)
}

// claimFloorLocked records that a turn is on its way before anything asks for
// it, and starts the wait for the provider to create it: the deadline
// onRequestExpired enforces. Every response.create this client sends is
// claimed here first. The caller holds mu.
//
// Any claim discharges an owed turn: the tool output or cue is already in the
// conversation, so whatever turn comes next is made with it in view, and a
// second request on top of this one would be refused.
func (r *Realtime) claimFloorLocked(request responseRequest) {
	r.isResponseRequested = true
	r.isTurnOwed = false
	r.isOwedPastCreate = false

	r.request = request
	r.requestGen++
	r.requestSentAt = time.Now()
	if r.requestTimer != nil {
		r.requestTimer.Stop()
	}
	deadline := r.responseCreatedDeadline
	if deadline <= 0 {
		deadline = responseCreatedTimeout
	}
	gen := r.requestGen
	// A timer rather than a goroutine that sleeps: nothing runs until it
	// fires, and a stopped one leaves nothing behind.
	r.requestTimer = time.AfterFunc(deadline, func() { r.onRequestExpired(gen) })
}

// settleRequestLocked records that the request in flight is over — created,
// refused, or never sent — and stops its deadline. The caller holds mu.
func (r *Realtime) settleRequestLocked() {
	r.isResponseRequested = false
	r.stopRequestTimerLocked()
}

// stopRequestTimerLocked stops the deadline of the request in flight, if there
// is one. A deadline already firing finds its generation stale or the request
// settled and does nothing. The caller holds mu.
func (r *Realtime) stopRequestTimerLocked() {
	if r.requestTimer != nil {
		r.requestTimer.Stop()
		r.requestTimer = nil
	}
}

// lineRequestLocked describes the request for one line: the direction it
// carries as its override and whether that direction goes in the conversation
// too (see askForLine for when). The caller holds mu.
func (r *Realtime) lineRequestLocked(text string, isClosing bool) responseRequest {
	direction := sayExactly(text, r.cfg.Language)
	if r.profile.NeedsToolFreeLineDirection {
		direction = sayExactlyWithoutTools(text, r.cfg.Language)
	}
	return responseRequest{
		kind:      responseKindLine,
		text:      text,
		isClosing: isClosing,
		direction: direction,
		isDirectionInConversation: r.profile.IgnoresOverrideOnceTalking ||
			(isClosing && r.profile.NeedsDirectedLineInConversation),
	}
}

// askForLine asks for the turn that says one line, on a floor the caller has
// already claimed for it (claimFloorLocked): the request carrying the direction
// as a per-response override and, for a line that ends the call on a profile
// that needs it (NeedsDirectedLineInConversation), the same direction put in
// the conversation as a caller message first. The item goes strictly before the
// request, so the turn the request makes is made with it in view.
//
// Where the engine ignores the override alone once the conversation has a turn
// (IgnoresOverrideOnceTalking), every line is put in the conversation. Where
// only NeedsDirectedLineInConversation is set, just a closing line is: the item
// stays in the history, and what a caller message saying "repeat this
// sentence" does to the turns after it has not been measured — a call that is
// ending has no turns after it to spoil. A line in a phase the conversation
// goes on from keeps the override alone there, for the reason answerToolCall
// keeps such a line out of a tool result.
//
// Both of SpeakText's ways to the floor end here — asked at once, or asked when
// the turn that held it has ended — so a line is steered the same way whichever
// one it took. If nothing could be asked for, the claim is released so that
// the next line is not left waiting for a turn that will never come.
func (r *Realtime) askForLine(request responseRequest) error {
	err := func() error {
		if request.isDirectionInConversation {
			if err := r.sendEvent(userTextItem(request.direction)); err != nil {
				return err
			}
		}
		return r.sendEvent(speakRequest(request.direction, r.profile.AcceptsResponseToolChoice))
	}()
	if err != nil {
		r.mu.Lock()
		r.settleRequestLocked()
		r.mu.Unlock()
	}
	return err
}

// speakRequest is the frame that asks for one line, said as written;
// direction is SayExactly's demand for it.
//
// One frame for both dialects: the type discriminator the GA session object
// carries has no counterpart on a response, and both name the per-response
// override the same way, so there is nothing here for a dialect to disagree
// about.
//
// isToolBarred adds the per-response tool_choice "none", on a profile that
// takes one (AcceptsResponseToolChoice). A line is words the flow chose, and
// the turn that says them must not call a tool: asked for while a slow tool's
// call was still unanswered (the wait line, #286), qwen answered the line's
// request with a second, identical call to that tool, so the backend was hit
// twice and the caller heard the answer twice (qwen-findings W-Q8).
func speakRequest(direction string, isToolBarred bool) map[string]any {
	response := map[string]any{"instructions": direction}
	if isToolBarred {
		response["tool_choice"] = "none"
	}
	return map[string]any{"type": "response.create", "response": response}
}

// SayExactly is how a model is asked for particular words rather than for a
// subject. In the session's own language, because an instruction in the wrong
// one is an invitation to answer in it.
//
// It is exported because a phase's announce is words the flow chose and nobody
// should paraphrase, and a client that can only direct a model has to ask for
// them the same way: the same demand, in the same language, ahead of the same
// text. A client whose engine speaks text outright needs none of it.
func SayExactly(text, language string) string {
	if strings.HasPrefix(strings.ToLower(language), "zh") {
		return "请一字不差地说出下面这句话，不要添加任何其它内容：\n" + text
	}
	return "Say exactly this, word for word, and add nothing else:\n" + text
}

// sayExactly is the name this client's call sites know it by.
var sayExactly = SayExactly

// sayExactlyWithoutTools is SayExactly for a line on a profile that needs the
// direction itself to keep the turn from calling a tool
// (NeedsToolFreeLineDirection): the same demand, plus a ban on any tool call
// and the fact that whatever tool the line mentions is already running.
//
// The second half is what answers the platform's own rule. Every flow with a
// tool carries flow's action-claim rule (#44): never say you are looking
// something up unless you call the tool in the same reply, with "我帮您查询一下"
// as its example. The default wait line, "请稍等，我查一下。", is that sentence, so
// a model that obeys the rule calls the tool to be allowed to say it, or calls
// it and says nothing (qwen-findings W-Q8).
func sayExactlyWithoutTools(text, language string) string {
	if strings.HasPrefix(strings.ToLower(language), "zh") {
		return "这一轮只做一件事：一字不差地说出下面这句话，不要添加任何其它内容，也不要调用任何工具。" +
			"这句话提到的工具已经在运行或已经运行过，结果会另外交给你，" +
			"所以说这句话不违反关于工具调用的规则：\n" + text
	}
	return "This turn does one thing only: say the sentence below word for word, " +
		"add nothing else, and call no tool. Any tool the sentence mentions is " +
		"already running or has already run, and its result will reach you " +
		"separately, so saying it breaks no rule about tool calls:\n" + text
}

// sayExactlyAfterCall is the direction of a line asked for again because its
// first turn called tools instead of saying it (askForLineAgain): the same
// demand, the tools it called named as not to be called now, and the two facts
// the conversation hides from the model. The call was never carried out, and
// the caller has not heard the line, so nothing they said before answers it.
//
// The second fact is the one the turn lost to. On #351's live call the phase's
// standing instruction said "after the line, on any reply, agreement included,
// call hangup", the caller's last words were an agreement given to the phase
// before, and the line's turn called hangup on them.
//
// isToolRunningNoted adds sayExactlyWithoutTools' last sentence, on a profile
// that needs it (NeedsToolFreeLineDirection): a wait line's turn calls the very
// tool still running, and a model that is told its call was dropped would
// otherwise want to make it again.
func sayExactlyAfterCall(text string, tools []string, language string, isToolRunningNoted bool) string {
	if strings.HasPrefix(strings.ToLower(language), "zh") {
		called := strings.Join(tools, "、")
		direction := "上一轮你没有说下面这句话，而是调用了 " + called + "。那次调用没有执行，现在也不需要执行。" +
			"来电者还没有听到这句话，他之前说的话不是对这句话的回答。" +
			"这一轮只做一件事：一字不差地说出下面这句话，不要添加任何其它内容，" +
			"现在不要调用 " + called + "，也不要调用任何其它工具"
		if isToolRunningNoted {
			direction += "。这句话提到的工具已经在运行或已经运行过，结果会另外交给你，" +
				"所以说这句话不违反关于工具调用的规则"
		}
		return direction + "：\n" + text
	}
	called := strings.Join(tools, ", ")
	direction := "Last turn you called " + called + " instead of saying the sentence below. " +
		"That call was not carried out, and it is not needed now. " +
		"The caller has not heard the sentence yet, and nothing they said before was an answer to it. " +
		"This turn does one thing only: say the sentence below word for word, add nothing else, " +
		"and do not call " + called + " now, nor any other tool"
	if isToolRunningNoted {
		direction += ". Any tool the sentence mentions is already running or has already run, " +
			"and its result will reach you separately, so saying it breaks no rule about tool calls"
	}
	return direction + ":\n" + text
}

// requestResponse asks the model for a turn and records that one is on its way
// (claimFloorLocked). Between here and response.created there is nothing to
// cancel, which is what SpeakText has to know before it asks for anything. If
// the frame could not be sent, the claim is released.
func (r *Realtime) requestResponse(request responseRequest, frame map[string]any) error {
	r.mu.Lock()
	r.claimFloorLocked(request)
	r.mu.Unlock()
	err := r.sendEvent(frame)
	if err != nil {
		r.mu.Lock()
		r.settleRequestLocked()
		r.mu.Unlock()
	}
	return err
}

// onRequestExpired ends the wait for a response the provider never created.
// The request's deadline calls it on the timer's own goroutine.
//
// An engine can drop a response.create without a word: grok drops one that
// carries only an override once the conversation has a turn (grok-findings
// 5.2), with no response.created and no error. Without a bound the floor would
// stay claimed for the rest of the call: every later line would wait in
// pendingSpeak, every tool result would ride on a turn that never comes, and
// the watchdog would never fire, because no response is open. The call goes
// quiet while it is still up.
//
// So the floor is given back, and then, in this order:
//
//   - a line waiting for the floor is asked for. It supersedes the dropped
//     request, as a newer line always supersedes an older one; the dropped
//     request is not asked for again.
//   - otherwise the request is retried once. A line goes again with its
//     direction put in the conversation, the form an engine that drops the
//     override alone was measured to answer (grok-findings 5.2), unless the
//     first request already put it there; then only the request is resent,
//     never a second identical item. A bare request is resent bare. The retry
//     claims the floor like any request, so a cue or tool result that arrived
//     during the wait rides on it.
//   - otherwise the retry was dropped too, and the request is given up: the
//     turn owed to anything that arrived meanwhile is asked for, and nothing is
//     reported to the consumer. An armed ending waiting for a closing line that
//     never came still ends on the consumer's own cap.
//
// Each expiry logs one WARN and counts one aicc_provider_response_unanswered_total.
//
// Nothing happens if the request has moved on: created, refused, replaced by a
// newer one, or the session ended.
func (r *Realtime) onRequestExpired(gen uint64) {
	if r.conn != nil {
		select {
		case <-r.conn.Done():
			return
		default:
		}
	}
	r.mu.Lock()
	if gen != r.requestGen || !r.isResponseRequested || r.isResponseOpen.Load() {
		r.mu.Unlock()
		return
	}
	expired := r.request
	waited := time.Since(r.requestSentAt)
	r.isResponseRequested = false
	r.requestTimer = nil

	var outcome string
	retry := expired
	switch {
	case r.pendingSpeak != "":
		outcome = obs.ResponseUnansweredSuperseded
		// The line that takes the opening's place is made with the opening's
		// direction in view, so its turn is the one that answers it.
		r.isOpeningDropped = r.isOpeningDropped || expired.kind == responseKindOpening
	case !expired.isRetry:
		outcome = obs.ResponseUnansweredRetried
		retry.isRetry = true
		// A line's retry is the same line asked for again, and a turn owed
		// past its first request (a result or a cue that arrived while it
		// was on its way) is still owed past it: the line's turn answers
		// neither. Any other retry is a bare request sent after the item and
		// answers it, so the claim discharges it as usual.
		isOwedPastLine := expired.kind == responseKindLine && r.isTurnOwed && r.isOwedPastCreate
		r.claimFloorLocked(retry)
		if isOwedPastLine {
			r.isTurnOwed, r.isOwedPastCreate = true, true
		}
		r.recoverySentAt, r.recoveryLine = time.Now(), nil
	default:
		outcome = obs.ResponseUnansweredDropped
	}
	r.mu.Unlock()

	obs.RecordResponseUnanswered(r.profile.Name, string(expired.kind), outcome)
	// The profile's name is on every line already (the logger's provider).
	attrs := []any{"kind", expired.kind, "waitedMs", waited.Milliseconds(),
		"isRetry", expired.isRetry}

	switch outcome {
	case obs.ResponseUnansweredSuperseded:
		r.log.Warn("the provider never created the response this client asked for; "+
			"asking for the line that was waiting for the floor instead", attrs...)
		r.dispatchPendingSpeak(true)
		r.releaseOwedTurn()

	case obs.ResponseUnansweredRetried:
		isDirectionAdded := expired.direction != "" && !expired.isDirectionInConversation
		r.log.Warn("the provider never created the response this client asked for; asking once more",
			append(attrs, "isDirectionAdded", isDirectionAdded)...)
		if err := r.sendRetry(expired); err != nil {
			r.mu.Lock()
			r.settleRequestLocked()
			r.mu.Unlock()
			r.log.Debug("could not ask again for the response that was never created", "error", err)
		}

	default:
		r.log.Warn("the provider never created the response this client asked for, "+
			"nor the retry; giving it up and freeing the floor", attrs...)
		r.releaseOwedTurn()
	}
}

// sendRetry asks again for a request that was never created, on a floor
// onRequestExpired has already claimed for the retry.
//
// If the first request was only late, both may reach the provider, and it
// refuses the second because the first is running; that refusal is expected
// (isAnsweredRecovery) and the turn is made once. A retry that put the
// direction in the conversation leaves that item in the history even then:
// the cost of one caller message asking for a line the late turn says anyway.
func (r *Realtime) sendRetry(expired responseRequest) error {
	if expired.direction == "" {
		return r.sendEvent(map[string]any{"type": "response.create"})
	}
	if !expired.isDirectionInConversation {
		if err := r.sendEvent(userTextItem(expired.direction)); err != nil {
			return err
		}
	}
	return r.sendEvent(speakRequest(expired.direction, r.profile.AcceptsResponseToolChoice))
}

// onResponseCreated stops a turn that was superseded before it existed.
//
// A line asked for while the previous request was still in flight could not be
// cancelled then, because the provider had created nothing. This is the first
// moment there is something to stop.
//
// It reports whether the response is the one a line asked for: the request in
// flight when it was created was a line's; and whether that line is one asked
// for again (askForLineAgain).
func (r *Realtime) onResponseCreated() (isLine, isAskedAgain bool) {
	r.mu.Lock()
	isLine = r.isResponseRequested && r.request.kind == responseKindLine
	r.isLineResponse = isLine
	r.lineTurn, r.lineCalls = responseRequest{}, nil
	r.isOpeningResponse = r.isResponseRequested &&
		(r.request.kind == responseKindOpening || r.isOpeningDropped)
	if r.isOpeningResponse {
		r.isOpeningDropped = false
	}
	if isLine {
		r.lineTurn = r.request
		isAskedAgain = r.request.isAskedAgain
	}
	r.isAnswerResponse = !isLine && r.isResultUnheard
	r.settleRequestLocked()
	r.isCallerSpeaking = false
	// Whoever asked for this response — this client or the provider's own
	// turn detection answering the caller — it is made with the tool output
	// in view, so it is the turn that output was owed.
	wasTurnOwed := r.isTurnOwed && !r.isOwedPastCreate
	if wasTurnOwed {
		r.isTurnOwed = false
	}
	r.isOwedPastCreate = false
	isCancelNeeded := r.pendingSpeak != "" && !r.isCancelSentForSpeak
	if isCancelNeeded {
		r.isCancelSentForSpeak = true
		r.isPreemptedForSpeak = true
	}
	r.mu.Unlock()

	if wasTurnOwed {
		r.log.Debug("a new response answers the tool result that was waiting for the floor")
	}
	if isCancelNeeded {
		if err := r.sendCancel(InterruptReasonSystem); err != nil {
			r.log.Warn("could not stop the turn a spoken line replaces", "error", err)
		}
	}
	return isLine, isAskedAgain
}

// dispatchPendingSpeak asks for the line that was waiting, now that the turn
// holding the floor has ended or the request holding it was never created. It
// reports whether that turn was stopped to make room, which is what decides who
// the interruption is attributed to.
//
// isRecovery says the line goes out in place of a request that was never
// created (onRequestExpired), which is what lets the provider's refusal of it
// be recognised if that request turns out to have been only late.
func (r *Realtime) dispatchPendingSpeak(isRecovery bool) (wasPreempted bool) {
	r.mu.Lock()
	text, isClosing := r.pendingSpeak, r.isPendingSpeakClosing
	wasPreempted = r.isPreemptedForSpeak
	r.pendingSpeak, r.isPendingSpeakClosing = "", false
	r.isPreemptedForSpeak = false
	r.isCancelSentForSpeak = false
	var request responseRequest
	if text != "" {
		request = r.lineRequestLocked(text, isClosing)
		r.claimFloorLocked(request)
		if isRecovery {
			line := request
			r.recoverySentAt, r.recoveryLine = time.Now(), &line
		}
	}
	r.mu.Unlock()

	if text == "" {
		return wasPreempted
	}
	if err := r.askForLine(request); err != nil {
		r.log.Warn("could not ask for the line that was waiting for the floor", "error", err)
	}
	return wasPreempted
}

// SendToolResult answers a tool call and steers what happens next.
//
// Whatever the engine wants said or done next travels inside output
// (MergeHint), not as a separate instruction update: the model reads it as part
// of what it just learned, which is what makes it act on it immediately.
//
// The result goes into the conversation at once; the turn it asks for may
// not. A model can go on talking in the same response after calling a tool,
// and asking for a response while one is open is refused ("Cannot create
// response while another response is in progress", qwen-findings W-Q4) —
// OpenAI refuses it too. The turn the result was meant to produce is then
// never made. So while a response is open, or a tool turn is already owed,
// the request is owed instead of sent, and releaseOwedTurn asks for it once
// that response is done. Several results in one response owe one turn.
//
// A batch answered after its response has ended is the same case in another
// shape: the first result asks for the turn, and until the provider reports
// response.created nothing is open, so the next result would ask again and be
// refused (qwen-findings W-Q6). A turn already asked for and not yet started
// covers the results that follow it, so one batch of results asks for exactly
// one response. A turn that is never started covers them only until
// responseCreatedDeadline: then it is asked for again, with the results in the
// conversation for it to read (onRequestExpired).
func (r *Realtime) SendToolResult(toolCallID, output string) error {
	if err := r.sendEvent(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": toolCallID,
			"output":  output,
		},
	}); err != nil {
		return err
	}

	// Decided after the output is sent, so a turn released by the read loop
	// can never be requested ahead of the output it is meant to answer. If
	// the response ends in between, the floor is seen free here and the turn
	// is asked for now.
	//
	// A line asked for and not yet created covers nothing: its turn says the
	// line as written and answers no result. The result's turn is owed past
	// that request, as a cue's is (SendUserText), and asked for when the
	// line's turn ends. Without this a result that arrived while a wait line
	// was on its way (#286) rode on the line and was never answered.
	r.mu.Lock()
	r.isResultUnheard, r.isUnheardNoted = true, false
	isOpen := r.isResponseOpen.Load()
	isLineRequested := !isOpen && r.isResponseRequested && r.request.kind == responseKindLine
	isOwed := isOpen || r.isTurnOwed || isLineRequested
	if isOwed {
		r.isTurnOwed = true
		if isLineRequested {
			r.isOwedPastCreate = true
		}
	}
	isCovered := !isOwed && r.isResponseRequested
	r.mu.Unlock()
	if isCovered {
		r.log.Debug("a turn is already asked for; this tool result rides on it",
			"toolCallId", toolCallID)
		return nil
	}
	if isOwed {
		r.log.Debug("a response holds the floor; the turn for this tool result waits for it to end",
			"toolCallId", toolCallID)
		return nil
	}
	return r.requestResponse(responseRequest{kind: responseKindTool},
		map[string]any{"type": "response.create"})
}

// releaseOwedTurn asks for the turn a tool result is owed, now that the
// response that held the floor has ended. The read loop calls it after
// dispatchPendingSpeak, so a line that was waiting has already taken the
// floor and — through requestResponse — discharged the tool turn: the line
// is said with the result in view, and one turn is all the floor holds.
//
// The rule for a caller who is speaking: if the provider has reported the
// caller starting to speak, and neither the end of it nor a new response
// since, the turn is held rather than requested. That is the ordinary shape
// of a barge-in — the cancelled response's done arrives one round trip after
// the speech began, long before the turn detector's silence hold ends it.
// The provider's own turn detection answers that speech with a response of
// its own (this client never turns that off), the output is already in the
// conversation for it to read, and a request of ours would land on top of it
// and be refused — or, where it wins the race, answer the tool while the
// caller is still talking. The provider's response.created
// discharges the held turn (onResponseCreated), as does any request this
// client makes in the meantime: a keypress's SendUserText, a SpeakText, the
// dead-air prompt. What is given up is the case where the provider hears
// speech and then answers nothing; the dead-air prompt is what covers a
// silent line, and it asks for a turn that reads the output too.
//
// Once the speech has ended the turn is no longer held. The provider's reply
// normally follows within a round trip, and a request of ours in that gap
// can be refused; but a provider that heard speech while a response was
// still open may never answer it (echo of the bot's own audio is the usual
// source), and a refused request costs a WARN where a stranded turn costs the
// call its next line. The narrower hold is the cheaper mistake.
func (r *Realtime) releaseOwedTurn() {
	r.mu.Lock()
	if !r.isTurnOwed {
		r.mu.Unlock()
		return
	}
	if r.isCallerSpeaking {
		r.mu.Unlock()
		r.log.Debug("the caller is speaking; the owed turn is left to the provider's reply")
		return
	}
	r.mu.Unlock()

	r.log.Debug("the floor is free; asking for the owed turn (a tool result or a cue was waiting)")
	if err := r.requestResponse(responseRequest{kind: responseKindOwed},
		map[string]any{"type": "response.create"}); err != nil {
		r.log.Warn("could not ask for the owed turn", "error", err)
	}
}

// MergeHint folds steering into a tool result. A result that is already a JSON
// object gains a field; anything else is wrapped so the shape stays predictable.
//
// It is exported because the hint belongs to the flow engine rather than to any
// wire protocol: whatever a client wraps a tool result in, the model has to read
// the steering as part of what it just learned, and in the same shape.
func MergeHint(output, hint string) string {
	if hint == "" {
		return output
	}
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &asObject); err == nil && asObject != nil {
		hintJSON, _ := json.Marshal(hint)
		asObject["hint"] = hintJSON
		if merged, err := json.Marshal(asObject); err == nil {
			return string(merged)
		}
	}
	merged, err := json.Marshal(map[string]string{"result": output, "hint": hint})
	if err != nil {
		return output
	}
	return string(merged)
}

// UpdateInstructions replaces the standing instructions.
//
// Only the instructions are sent. Re-sending the whole configuration would
// re-assert turn detection, which at least one provider treats as an error
// once audio has started flowing.
func (r *Realtime) UpdateInstructions(text string) error {
	r.mu.Lock()
	r.instructions = text
	r.mu.Unlock()

	session := map[string]any{"instructions": text}
	if r.profile.Style == styleGA {
		session["type"] = "realtime"
	}
	return r.sendEvent(map[string]any{"type": "session.update", "session": session})
}

// Interrupt stops the model talking over the caller.
//
// Which side is responsible differs by vendor: one cancels on its own as soon
// as it hears speech and only needs to be told how much was actually heard;
// the other does nothing until asked. That self-cancel is a reaction to the
// provider hearing the caller, so it covers speech and nothing else: a
// keypress or an application decision is something the provider never heard,
// and every profile has to be asked to cancel for it.
//
// The two halves answer different questions and are gated separately. A
// response is cancelled only while one is open — asking a provider to cancel
// a response that already ended is an error on the vendors that accept the
// cancel at all. Truncation has no such limit: the caller goes on hearing an
// utterance for seconds after the provider finished making it, so the amount
// actually heard is worth reporting long after there is anything to stop.
//
// No event is emitted here. The caller already knows it interrupted, and
// emitting from this method would deadlock whenever it is called — as it
// normally is — from the goroutine draining Events. The interruption surfaces
// where the provider confirms it, as a cancelled response.
func (r *Realtime) Interrupt(reason InterruptReason, playedMs int) error {
	r.mu.Lock()
	itemID := r.responseItemID
	r.mu.Unlock()

	isHeardByProvider := reason == InterruptReasonSpeech && r.profile.CancelsResponseItself
	if !isHeardByProvider && r.isResponseOpen.Load() {
		if err := r.sendCancel(reason); err != nil {
			return err
		}
	}

	// Telling the provider how much was heard keeps its conversation history
	// honest: without it the model believes the caller heard a sentence that
	// was cut off after three words.
	//
	// Zero is a report like any other: the turn's audio was queued behind an
	// earlier turn's tail and flushed before a frame reached the caller, so the
	// whole item comes out of the history. The id is spent once truncated, so a
	// second report for the same turn cannot overwrite the first.
	//
	// Zero is also taken further than a truncate: the item is deleted. qwen
	// acknowledges a truncate and leaves the text the model reads back whole
	// (qwen-findings, TestLiveQwenTruncate), so a turn flushed before a frame of
	// it played stayed in its history as said; it honours a delete (W-Q5).
	// The truncate still goes first for an engine that honours it and not the
	// delete. An item is never deleted while its response is open, because
	// the provider is still writing it: then it is deleted when that response
	// ends (removeUnheardTurn).
	if itemID != "" && playedMs >= 0 {
		r.mu.Lock()
		if r.responseItemID == itemID {
			r.responseItemID = ""
		}
		isDeleteDeferred := playedMs == 0 && r.isResponseOpen.Load()
		if isDeleteDeferred {
			r.unheardItems = append(r.unheardItems, itemID)
		}
		r.mu.Unlock()
		if err := r.sendEvent(map[string]any{
			"type":          "conversation.item.truncate",
			"item_id":       itemID,
			"content_index": 0,
			"audio_end_ms":  playedMs,
		}); err != nil {
			return err
		}
		if playedMs == 0 && !isDeleteDeferred {
			if err := r.deleteUnheardItem(itemID, ""); err != nil {
				return err
			}
			r.mu.Lock()
			isAnswer := itemID == r.answerItemID
			r.mu.Unlock()
			if isAnswer {
				return r.noteUnheardResult()
			}
		}
	}
	return nil
}

// sendCancel asks the provider to stop the response in progress, and remembers
// when it did.
//
// Whether a response is in progress is only ever this side's belief: the
// provider may finish it in the round trip before the cancel arrives, and then
// it refuses the cancel. No local state closes that window, so the refusal is
// expected and has to be recognisable as ours — see isAnsweredCancel.
func (r *Realtime) sendCancel(reason InterruptReason) error {
	r.mu.Lock()
	r.cancelSentAt = time.Now()
	r.cancelReason = reason
	r.mu.Unlock()
	return r.sendEvent(map[string]any{"type": "response.cancel"})
}

// cancelAnswerWindow is how long after a cancel a refusal of it is still taken
// as its answer. A refusal arrives one round trip later (74 ms measured on
// qwen); the window is generous so a slow link does not turn it into a WARN,
// and bounded so the same words long after any cancel still surface.
const cancelAnswerWindow = 5 * time.Second

// isAnsweredCancel reports whether an error is the provider refusing a cancel
// this client sent because the response had already ended — the benign losing
// side of the race sendCancel describes, not a fault.
//
// Matched narrowly, on the code and the words qwen was seen to use
// ("invalid_value: Conversation has no active response"), and only within
// cancelAnswerWindow of a cancel we sent. The error event names neither the
// frame it refuses nor the response it is about, so a time bound is the one
// correlation available; ordering-based bookkeeping would be wrong whenever a
// new response is requested before the refusal arrives, which is exactly what
// a keypress or a spoken line does right after an interruption.
func (r *Realtime) isAnsweredCancel(err *wireError) bool {
	// The code differs by vendor: qwen "invalid_value", OpenAI
	// "response_cancel_not_active", Grok the generic "invalid_request_error".
	switch err.Code {
	case "invalid_value", "response_cancel_not_active", "invalid_request_error":
	default:
		return false
	}
	if !strings.Contains(strings.ToLower(err.Message), "no active response") {
		return false
	}
	r.mu.Lock()
	sentAt := r.cancelSentAt
	r.mu.Unlock()
	return !sentAt.IsZero() && time.Since(sentAt) <= cancelAnswerWindow
}

// isAnsweredRecovery reports whether an error is the provider refusing a
// request this client sent in place of one that was never created (a retry, or
// a line that superseded it) because the first one turned out to be only late
// and is now running. line is the superseding line, if that is what was
// refused. Spent once matched: one recovery draws at most one refusal.
//
// Matched like isAnsweredCancel: within cancelAnswerWindow of the recovery and
// before any response has ended since (handleResponseDone), on the words the
// providers refuse a second response with (OpenAI
// "conversation_already_has_active_response", qwen "another response is in
// progress"), because the error names neither the frame it refuses nor the
// response that is running.
func (r *Realtime) isAnsweredRecovery(err *wireError) (isRecovery bool, line *responseRequest) {
	message := strings.ToLower(err.Message)
	if err.Code != "conversation_already_has_active_response" &&
		!strings.Contains(message, "already has an active response") &&
		!strings.Contains(message, "another response is in progress") {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recoverySentAt.IsZero() || time.Since(r.recoverySentAt) > cancelAnswerWindow {
		return false, nil
	}
	line = r.recoveryLine
	r.recoverySentAt, r.recoveryLine = time.Time{}, nil
	return true, line
}

// Close ends the session.
func (r *Realtime) Close(_ context.Context) error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.stopRequestTimerLocked()
		r.mu.Unlock()
		if r.conn != nil {
			r.conn.Close()
		}
	})
	return nil
}

//
// Session payloads.
//

// buildSessionUpdate assembles the configuration in this profile's dialect.
// isReduced drops the optional fields, for the one retry after a rejection.
func (r *Realtime) buildSessionUpdate(cfg SessionConfig, isReduced bool) map[string]any {
	session := map[string]any{"instructions": cfg.Instructions}

	if tools := r.buildTools(cfg.Tools); len(tools) > 0 {
		session["tools"] = tools
	}
	voice := cfg.Voice
	if voice == "" {
		voice = r.profile.Voice
	}

	if r.profile.Style == styleGA {
		inputName, inputRate := r.profile.formatName(cfg.InputFormat)
		outputName, outputRate := r.profile.formatName(cfg.OutputFormat)

		input := map[string]any{
			"format":         audioFormatPayload(inputName, inputRate),
			"turn_detection": r.buildTurnDetection(cfg.Turn),
		}
		if r.profile.TranscribeModel != "" && !isReduced {
			input["transcription"] = map[string]any{"model": r.profile.TranscribeModel}
		}
		output := map[string]any{"format": audioFormatPayload(outputName, outputRate)}
		if voice != "" {
			output["voice"] = voice
		}

		session["type"] = "realtime"
		session["audio"] = map[string]any{"input": input, "output": output}
		if r.profile.AcceptsTemperature && cfg.Temperature != nil && !isReduced {
			session["temperature"] = *cfg.Temperature
		}
		return map[string]any{"type": "session.update", "session": session}
	}

	inputName, _ := r.profile.formatName(cfg.InputFormat)
	outputName, _ := r.profile.formatName(cfg.OutputFormat)
	session["modalities"] = []string{"text", "audio"}
	session["input_audio_format"] = inputName
	session["output_audio_format"] = outputName
	session["turn_detection"] = r.buildTurnDetection(cfg.Turn)
	if voice != "" {
		session["voice"] = voice
	}
	// Only the opening session.update carries it; mid-call updates are
	// instructions alone. It is optional, so the reduced retry drops it: a
	// session at the provider's default beats a caller with no bot.
	if r.profile.AcceptsTemperature && cfg.Temperature != nil && !isReduced {
		session["temperature"] = *cfg.Temperature
	}
	return map[string]any{"type": "session.update", "session": session}
}

func audioFormatPayload(name string, rateHz int) map[string]any {
	format := map[string]any{"type": name}
	if rateHz > 0 {
		format["rate"] = rateHz
	}
	return format
}

// buildTurnDetection maps the declarative setting onto the vendor's names.
func (r *Realtime) buildTurnDetection(turn TurnDetection) any {
	detection := map[string]any{"type": "server_vad"}
	if turn.SilenceMs > 0 {
		detection["silence_duration_ms"] = turn.SilenceMs
	}
	return detection
}

// isSpokenItem reports whether an output item is the model talking, which is
// the only kind truncation refers to. A dialect that omits the type is taken at
// its word rather than losing the only item there is; a named non-message item
// — a function call — is not a truncation target.
func isSpokenItem(itemType string) bool {
	return itemType == "" || itemType == "message"
}

// buildTools renders tool specifications in the flat shape both providers were
// verified to accept.
func (r *Realtime) buildTools(tools []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := json.RawMessage(`{"type":"object","properties":{}}`)
		if len(tool.Parameters) > 0 {
			parameters = tool.Parameters
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  parameters,
		})
	}
	return out
}

//
// Inbound events.
//

func (r *Realtime) readLoop() {
	defer close(r.events)

	for {
		event, raw, err := r.receive()
		if err != nil {
			// There is no reconnect: the provider holds conversation state
			// that cannot be rebuilt, so a lost socket ends the session and
			// the call is routed somewhere a person can take it.
			r.finishStart(err)
			// Nothing will be created on a connection that is gone.
			r.mu.Lock()
			r.stopRequestTimerLocked()
			r.mu.Unlock()
			if !isNormalClosure(err) {
				r.emit(Event{
					Type: EventTypeError, Err: err, IsFatal: true,
					Text: "provider connection lost",
				})
			}
			r.emit(Event{Type: EventTypeClosed})
			return
		}
		if event == nil {
			r.log.Debug("ignored non-text frame", "bytes", len(raw))
			continue
		}
		r.handle(event)
	}
}

func (r *Realtime) handle(event *wireEvent) {
	switch event.Type {
	case "session.created":
		// The provider's defaults, before ours are applied, are not used. What
		// is kept is the session id: it is the provider's own name for this
		// call, and the only way to match a call to the provider's session
		// logs and captures when calls overlap. The event arrives once per
		// connection, so this is one line per call. A provider that sends no
		// id gets a debug line instead, so the absence is visible without
		// an Info line that carries an empty field.
		if event.Session != nil && event.Session.ID != "" {
			r.log.Info("provider session created", "providerSessionId", event.Session.ID)
		} else {
			r.log.Debug("provider session created without an id")
		}
		return

	case "session.updated":
		r.finishStart(nil)
		r.emit(Event{Type: EventTypeSessionReady})

	case "input_audio_buffer.speech_started":
		r.mu.Lock()
		r.isCallerSpeaking = true
		r.mu.Unlock()
		r.emit(Event{Type: EventTypeSpeechStarted})

	case "input_audio_buffer.speech_stopped":
		r.mu.Lock()
		r.isCallerSpeaking = false
		r.mu.Unlock()
		r.emit(Event{Type: EventTypeSpeechStopped})

	case "response.created":
		r.mu.Lock()
		r.responseItemID = ""
		r.callItems, r.finishedCalls = nil, nil
		r.spokenItems, r.unheardItems = nil, nil
		r.mu.Unlock()
		r.hasResponseAudio.Store(false)
		r.isResponseOpen.Store(true)
		isLine, isAskedAgain := r.onResponseCreated()
		r.signal(watchResponseStarted)
		r.emit(Event{Type: EventTypeResponseStarted, IsSpokenLine: isLine,
			IsLineAskedAgain: isAskedAgain})

	case "response.output_item.added":
		// Only the item carrying speech can be truncated. A turn that says
		// something before it calls a tool adds a second item, and letting
		// that one overwrite the id would aim the truncate at the function
		// call: the provider rejects it, and the model is left believing the
		// caller heard a sentence that was cut off three words in.
		if event.Item != nil && event.Item.ID != "" && isSpokenItem(event.Item.Type) {
			r.mu.Lock()
			r.responseItemID = event.Item.ID
			r.spokenItems = append(r.spokenItems, event.Item.ID)
			if r.isAnswerResponse {
				r.answerItemID = event.Item.ID
			}
			r.mu.Unlock()
		}
		// A function call is open until its arguments are done; see
		// removeUnfinishedCalls.
		if event.Item != nil && event.Item.ID != "" && event.Item.Type == "function_call" {
			r.mu.Lock()
			if r.callItems == nil {
				r.callItems = make(map[string]string)
			}
			r.callItems[event.Item.ID] = event.Item.CallID
			r.mu.Unlock()
		}

	// Both the current and the older names for model audio.
	case "response.audio.delta", "response.output_audio.delta":
		audio, err := base64.StdEncoding.AppendDecode(nil, []byte(event.Delta))
		if err != nil {
			r.emit(Event{Type: EventTypeError, Err: fmt.Errorf("undecodable audio: %w", err)})
			return
		}
		r.signal(watchAudioArrived)
		if !r.hasResponseAudio.Load() {
			r.hasResponseAudio.Store(true)
		}
		r.emit(Event{Type: EventTypeAudioDelta, Audio: audio})

	case "response.audio_transcript.delta", "response.output_audio_transcript.delta":
		r.emit(Event{Type: EventTypeOutputTranscript, Text: event.Delta})

	case "response.audio_transcript.done", "response.output_audio_transcript.done":
		r.emit(Event{Type: EventTypeOutputTranscript, Text: event.Transcript, IsFinal: true})

	case "conversation.item.input_audio_transcription.delta":
		r.emit(Event{Type: EventTypeInputTranscript, Text: event.Delta})

	case "conversation.item.input_audio_transcription.updated":
		// A cumulative running text of the same utterance (Grok). Nothing
		// downstream reads partial caller text, and re-emitting the whole
		// string per update would be read as new speech; the final .completed
		// carries the line.
		return

	case "conversation.item.input_audio_transcription.completed":
		// Some vendors repeat .completed with status "in_progress" before the
		// final one. Only the final counts, so a caller utterance is exactly one
		// transcript line. An absent status is final (OpenAI never sends one).
		if event.Status != "" && event.Status != "completed" {
			return
		}
		r.emit(Event{Type: EventTypeInputTranscript, Text: event.Transcript, IsFinal: true})

	case "ping":
		// A keep-alive some vendors send every few seconds. No state, no log.
		return

	case "response.function_call_arguments.done":
		if r.onLineCall(event) {
			return
		}
		r.finishCall(event.ItemID, event.CallID)
		arguments := event.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		r.emit(Event{
			Type: EventTypeToolCall, ToolCallID: event.CallID,
			ToolName: event.Name, ToolArgs: arguments,
		})

	case "conversation.item.created", "conversation.item.added":
		r.noteCreatedItem(event.Item)

	case "conversation.item.deleted":
		r.log.Debug("the provider removed an item from the conversation", "itemId", event.ItemID)

	case "response.done":
		r.handleResponseDone(event)

	case "error":
		r.handleError(event)

	default:
		r.log.Debug("unmapped provider event", "type", event.Type)
	}
}

func (r *Realtime) handleResponseDone(event *wireEvent) {
	r.isResponseOpen.Store(false)
	// A refusal of a request sent in place of a late one arrives while the late
	// turn is still running, so once a turn has ended there is none left to
	// expect: the same words after this are someone else's, and are reported.
	r.mu.Lock()
	r.recoverySentAt, r.recoveryLine = time.Time{}, nil
	r.mu.Unlock()
	cancelledFor := r.takeCancelReason()
	r.signal(watchResponseEnded)
	status := ""
	if event.Response != nil {
		status = event.Response.Status
	}
	// Read before removeUnfinishedCalls forgets that this was a line's turn.
	line, lineCalls := r.takeLineTurn()
	// Before anything asks for a turn, so that turn is made from a
	// conversation without the calls this one left unfinished, nor the words
	// of this one that the caller never heard.
	r.removeUnfinishedCalls(event.Response)
	r.removeUnheardTurn(event.Response)
	r.removeAgainDirection(line)
	r.removeOpeningDirection()
	// The floor is free: a line that was waiting for it goes out before the
	// turn is reported, so the next words are already being made while the
	// consumer catches up.
	isPreemptedForSpeak := r.dispatchPendingSpeak(false)
	// Then a line whose turn called a tool and said nothing is asked for
	// again, unless a newer line has just taken the floor. Whatever turn was
	// owed stays owed past it, as past the line's first request.
	isLineAskedAgain := r.askForLineAgain(line, lineCalls, status)
	// Then the turn a tool result answered mid-response is owed, unless the
	// line just asked for is that turn.
	if !isLineAskedAgain {
		r.releaseOwedTurn()
	}
	// A cancelled response is a turn that was cut short, whether we asked for
	// it or the provider decided on its own when it heard the caller. Either
	// way the consumer needs to know the turn ended early rather than
	// completing, so it is reported as an interruption in its own right.
	//
	// Who cut it short is the one thing that differs, and it has to be said:
	// the consumer flushes what it has queued either way — the caller must not
	// go on hearing an abandoned answer, and how much of it they did hear is
	// counted on that side and trimmed from the provider's history — but a
	// turn this client stopped to make room for a line is not a barge-in, and
	// logging it as one puts an interruption the caller never made into the
	// record of the call.
	if status == statusCancelled {
		by := InterruptReasonSpeech
		if cancelledFor != "" {
			by = cancelledFor
		}
		if isPreemptedForSpeak {
			by = InterruptReasonSystem
		}
		r.emit(Event{Type: EventTypeInterrupted, InterruptedBy: by})
		return
	}
	r.emit(Event{Type: EventTypeResponseDone})
}

// takeCancelReason returns why this client last cancelled, if it did so within
// cancelAnswerWindow, and forgets it: one cancel explains one response's end.
// A response that ended on its own after a cancel lost the race spends the
// reason too, so it cannot label a later cancel the provider decides itself.
func (r *Realtime) takeCancelReason() InterruptReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	reason := r.cancelReason
	r.cancelReason = ""
	if time.Since(r.cancelSentAt) > cancelAnswerWindow {
		return ""
	}
	return reason
}

// statusCancelled is what both providers call a response that was cut short.
const statusCancelled = "cancelled"

// statusCompleted is a response that ran to its end.
const statusCompleted = "completed"

// finishCall records that a function call's arguments arrived, so it is a call
// the consumer has been handed and will answer, never one to take back. The
// event names the item and the call; either may be missing in a dialect, so
// both are kept.
func (r *Realtime) finishCall(itemID, callID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finishedCalls == nil {
		r.finishedCalls = make(map[string]struct{})
	}
	for id, call := range r.callItems {
		if (itemID != "" && id == itemID) || (callID != "" && call == callID) {
			delete(r.callItems, id)
		}
	}
	if itemID != "" {
		r.finishedCalls[itemID] = struct{}{}
	}
	if callID != "" {
		r.finishedCalls[callID] = struct{}{}
	}
}

// onLineCall reports whether a finished function call belongs to a line's
// turn, and if so keeps it from the consumer.
//
// A line is words the flow chose, and the turn asked for to say them must not
// act. qwen ignores every way the request can say so (a response-level
// tool_choice "none" and a response-level empty tools list, measured
// 2026-10-22, qwen-findings W-Q8): asked for a wait line while a slow tool's
// call was unanswered, it said the line and called the same tool again with
// the same arguments. Handed out, that call hit the backend a second time and
// its result made a second answer turn. So the call is not handed out, and it
// is left open: removeUnfinishedCalls takes it out of the conversation when
// the line's turn ends, as it does a call a cancelled turn left half made, so
// that no later turn reads it as a call still waiting for its output. The
// call the line was said over is the earlier turn's and is answered as usual.
//
// The tool is remembered, so that a turn which called it and said nothing can
// be asked for again with the tool named (askForLineAgain).
func (r *Realtime) onLineCall(event *wireEvent) bool {
	r.mu.Lock()
	isLine := r.isLineResponse
	if isLine && event.Name != "" && !slices.Contains(r.lineCalls, event.Name) {
		r.lineCalls = append(r.lineCalls, event.Name)
	}
	r.mu.Unlock()
	if !isLine {
		return false
	}
	r.log.Info("the turn asked for a line called a tool; the call is not handed out and is taken back when the turn ends",
		"tool", event.Name, "callId", event.CallID, "itemId", event.ItemID)
	return true
}

// takeLineTurn returns the line the response now ending was asked for and the
// tools its turn called instead (onLineCall), and forgets both: they belong to
// that response. A response that was not a line's returns a zero request.
func (r *Realtime) takeLineTurn() (line responseRequest, calls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line, calls = r.lineTurn, r.lineCalls
	r.lineTurn, r.lineCalls = responseRequest{}, nil
	return line, calls
}

// askForLineAgain asks once more for a line whose turn called a tool instead
// of saying it, and reports whether it did. The response that turn made has
// just ended (handleResponseDone).
//
// qwen ignores every way a request can bar tools (W-Q8), and the direction
// alone loses where the conversation invites a call: on #351's live call
// 01a17c7d-b4d4-7870-883e-b82a9553f3c6 a non-terminal phase entered through a
// slot tool asked for its line, the phase's instruction said "after the line,
// on any reply, agreement included, call hangup", the caller's last words were
// the agreement that moved the call there, and the line's turn called hangup.
// onLineCall kept the call from the flow, the turn said nothing, and the caller
// sat through nine seconds of silence until they spoke. So the line is asked
// for once more, its direction naming what was called (sayExactlyAfterCall).
// It costs one turn where the first fails and nothing where it does not.
//
// The second direction goes in the conversation as a caller message as well
// as in the request's override, on every profile, and is taken back out when
// its turn ends (removeAgainDirection). Replayed on the call's own session
// (TestLiveQwenLineAfterMove, qwen-findings W-Q10), the override alone lost
// again 9 of 9, the item said the line 9 of 9 and, left in, made the turn
// after it answer the caller with the wrong tool 9 of 9; put in and taken out,
// the line was said 10 of 10 and the caller's answer drew the hangup the
// phase asked for 10 of 10.
//
// Only a turn the caller heard nothing of, that ran to its end, is asked for
// again:
//
//   - a turn whose audio reached the consumer was heard, line or not, and
//     asking again would say it twice. qwen saying the line and then calling
//     the running tool (the wait line in W-Q8) is that turn;
//   - a turn cut short was stopped by someone who wants the floor: the caller
//     talking (the provider's own turn detection answers them), a keypress or
//     a newer line, which pre-empts and has already been asked for
//     (dispatchPendingSpeak);
//   - the caller who has started speaking since the turn ended is answered by
//     the provider's own reply, which a request here would be refused over.
//
// Once only. A line asked for again whose turn calls a tool too is given up,
// with a WARN: the caller hears silence until they speak or the consumer's
// dead-air watch prompts them, which is what every such turn cost before
// #351, and a third ask would be a loop on a model that has made up its mind.
//
// A turn owed to a result or a cue that arrived while the line was on its way
// stays owed past the line asked again, as it does past a line's retry
// (onRequestExpired): the line's turn answers neither, and the owed turn is
// asked for when it ends.
func (r *Realtime) askForLineAgain(line responseRequest, calls []string, status string) bool {
	if line.kind != responseKindLine || len(calls) == 0 {
		return false
	}
	attrs := []any{"tools", calls, "isClosing", line.isClosing}
	switch {
	case r.hasResponseAudio.Load():
		r.log.Debug("the turn asked for a line called a tool and was heard; not asking again", attrs...)
		return false
	case status != "" && status != statusCompleted:
		r.log.Debug("the turn asked for a line called a tool and was cut short; not asking again",
			append(attrs, "status", status)...)
		return false
	case line.isAskedAgain:
		r.log.Warn("the line was asked for again and its turn called a tool instead once more; "+
			"giving it up", attrs...)
		return false
	}

	r.mu.Lock()
	if r.isResponseRequested || r.pendingSpeak != "" || r.isCallerSpeaking {
		r.mu.Unlock()
		r.log.Debug("the turn asked for a line called a tool; the floor has moved on, not asking again", attrs...)
		return false
	}
	again := responseRequest{
		kind:      responseKindLine,
		text:      line.text,
		isClosing: line.isClosing,
		direction: sayExactlyAfterCall(line.text, calls, r.cfg.Language,
			r.profile.NeedsToolFreeLineDirection),
		isDirectionInConversation: true,
		isAskedAgain:              true,
	}
	isOwed := r.isTurnOwed
	r.claimFloorLocked(again)
	if isOwed {
		r.isTurnOwed, r.isOwedPastCreate = true, true
	}
	r.againDirection, r.againItemID = again.direction, ""
	r.mu.Unlock()

	r.log.Info("the turn asked for a line called a tool and said nothing; asking for the line once more", attrs...)
	if err := r.askForLine(again); err != nil {
		r.log.Warn("could not ask again for the line its turn did not say", "error", err)
		return false
	}
	return true
}

// noteCreatedItem learns the provider's name for the caller message that
// carries the direction of a line asked for again, or of the opening line,
// from the event that announces it (conversation.item.created, or .added in
// the GA dialect), so removeAgainDirection or removeOpeningDirection can take
// it out. Any other item is ignored.
func (r *Realtime) noteCreatedItem(item *wireItem) {
	if item == nil || item.ID == "" || item.Role != "user" || len(item.Content) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch text := item.Content[0].Text; {
	case r.againDirection != "" && text == r.againDirection:
		r.againItemID, r.againDirection = item.ID, ""
	case r.openingDirection != "" && text == r.openingDirection:
		r.openingItemID, r.openingDirection = item.ID, ""
	}
}

// removeAgainDirection takes the direction of a line asked for again out of
// the conversation once that line's turn has ended, however it ended, before
// anything asks for the next turn. The item was there to steer that one turn.
// Left in, it steered the turns after it too: asked again with the item kept,
// the line was said 9 of 9, and the caller's answer to it was then met 9 of 9
// with the previous phase's tool instead of the hangup the phase asked for
// (qwen-findings W-Q10). An item the provider never named stays where it is.
func (r *Realtime) removeAgainDirection(line responseRequest) {
	if !line.isAskedAgain {
		return
	}
	r.mu.Lock()
	itemID := r.againItemID
	r.againItemID, r.againDirection = "", ""
	if itemID != "" {
		r.deleteSentAt = time.Now()
	}
	r.mu.Unlock()
	if itemID == "" {
		r.log.Debug("the direction of the line asked again was never named; it stays in the conversation")
		return
	}
	if err := r.sendEvent(map[string]any{
		"type":    "conversation.item.delete",
		"item_id": itemID,
	}); err != nil {
		r.log.Debug("could not take the direction of the line asked again out of the conversation",
			"itemId", itemID, "error", err)
		return
	}
	r.log.Debug("took the direction of the line asked again out of the conversation", "itemId", itemID)
}

// removeOpeningDirection takes the direction of the opening line out of the
// conversation once the response that answers the opening request has ended,
// however it ended (completed, or cancelled by the caller taking the floor),
// before anything asks for the next turn. The item was there to get the first
// turn made. Left in, it stayed the caller's first message for the whole call,
// and a later turn that answered tool results with nothing of its own to say
// sometimes fulfilled it again and spoke the opener word for word: 15 of 385
// probe runs on the old wire, 0 of 240 with the item taken out at this moment
// (one-sided p = 0.0006), and 0 of 17 live calls (qwen-findings W-Q11). The
// opener is still said in full by the turn that was asked for it.
//
// Only a named direction (the opening line) goes: the bare greeting cue of a
// call whose flow names no line is not measured and stays. An opening request
// that is retried keeps the direction until the response that finally answers
// it ends. An item the provider never named stays where it is.
func (r *Realtime) removeOpeningDirection() {
	r.mu.Lock()
	isOpening := r.isOpeningResponse
	r.isOpeningResponse = false
	if !isOpening || (r.openingDirection == "" && r.openingItemID == "") {
		r.mu.Unlock()
		return
	}
	itemID := r.openingItemID
	r.openingItemID, r.openingDirection = "", ""
	if itemID != "" {
		r.deleteSentAt = time.Now()
	}
	r.mu.Unlock()
	if itemID == "" {
		r.log.Debug("the direction of the opening line was never named; it stays in the conversation")
		return
	}
	if err := r.sendEvent(map[string]any{
		"type":    "conversation.item.delete",
		"item_id": itemID,
	}); err != nil {
		r.log.Debug("could not take the direction of the opening line out of the conversation",
			"itemId", itemID, "error", err)
		return
	}
	r.log.Debug("took the direction of the opening line out of the conversation", "itemId", itemID)
}

// removeUnfinishedCalls takes out of the conversation every function call a
// response that did not complete left half made, and every function call a
// line's turn made (onLineCall), however that turn ended.
//
// A response cut off while it is writing a call's arguments (a keypress, a
// barge-in, a preempting line) leaves the call item in the conversation, with
// whatever arguments had arrived, and the provider never sends the event that
// finishes it. qwen's model then reads its own unanswered call as the thing it
// has already done: asked again, it often says "transferring you" and never
// calls the tool, and keeps copying that (qwen-findings W-Q5). Deleting the item
// puts the next turn back on a history without it.
//
// Which calls: those begun in this response (output_item.added) or listed in
// its output, minus any whose arguments completed. A completed call was handed
// to the consumer as a tool call and will get its output, so it stays. Which
// responses: any that did not complete, because cancelled, incomplete and
// failed all stop mid-item and all leave the same remnant. A completed response
// is left alone. The stall path does not come here: the provider has not said
// that response is over and may still finish the call, and deleting an item
// that then gets its arguments and its output would break a call the consumer
// is answering.
//
// A refused delete is not a fault (an item already gone, or a provider without
// the event); see isAnsweredDelete.
func (r *Realtime) removeUnfinishedCalls(response *wireResponse) {
	r.mu.Lock()
	open, finished, isLine := r.callItems, r.finishedCalls, r.isLineResponse
	r.callItems, r.finishedCalls, r.isLineResponse = nil, nil, false
	r.mu.Unlock()

	isCutShort := response != nil && response.Status != "" && response.Status != statusCompleted
	if !isCutShort && !isLine {
		return
	}
	status := ""
	if response != nil {
		status = response.Status
	}
	ids := make(map[string]struct{}, len(open))
	for id := range open {
		ids[id] = struct{}{}
	}
	var output []wireItem
	if response != nil {
		output = response.Output
	}
	for _, item := range output {
		if item.Type != "function_call" || item.ID == "" {
			continue
		}
		_, isItemFinished := finished[item.ID]
		_, isCallFinished := finished[item.CallID]
		if !isItemFinished && !(item.CallID != "" && isCallFinished) {
			ids[item.ID] = struct{}{}
		}
	}
	for id := range ids {
		r.mu.Lock()
		r.deleteSentAt = time.Now()
		r.mu.Unlock()
		if err := r.sendEvent(map[string]any{
			"type":    "conversation.item.delete",
			"item_id": id,
		}); err != nil {
			r.log.Debug("could not take an unfinished function call out of the conversation",
				"itemId", id, "error", err)
			continue
		}
		r.log.Debug("took a function call out of the conversation",
			"itemId", id, "status", status, "isLine", isLine)
	}
}

// removeUnheardTurn takes out of the conversation the words of a response the
// caller heard none of, before anything asks for the next turn.
//
// Two shapes. A response cut short (any status but completed) before a single
// frame of its audio reached the consumer: the provider wrote the words, and
// often all of them, and nothing was played, so the consumer has nothing to
// flush, counts nothing and reports nothing. And an item the consumer did
// report as unheard (Interrupt with 0 ms) while its response was still open.
//
// The first is the #286 live call 01a16256-ddd5-…: a tool's answer was asked
// for as the caller started to speak over the wait line, qwen's own turn
// detection cancelled that response 200 ms later with its whole transcript
// written and no audio sent, and its reply to the caller ("在的，请问还有什么可以
// 帮您？") took the answer as given; the caller never heard it. Replayed live
// (TestLiveQwenSpeechOverTheAnswer), the history without the unheard item is
// answered with the result.
//
// A response some audio of which reached the consumer is the consumer's to
// account for: it alone knows how much was played, and reports it through
// Interrupt.
func (r *Realtime) removeUnheardTurn(response *wireResponse) {
	r.mu.Lock()
	spoken, unheard := r.spokenItems, r.unheardItems
	r.spokenItems, r.unheardItems = nil, nil
	isAnswer, isPreempted := r.isAnswerResponse, r.isPreemptedForSpeak
	r.isAnswerResponse = false
	r.mu.Unlock()

	status := ""
	if response != nil {
		status = response.Status
	}
	// An answer that ran to its end with its audio handed out was delivered;
	// whether it was heard is the consumer's to report (Interrupt).
	if isAnswer && (status == "" || status == statusCompleted) && r.hasResponseAudio.Load() {
		r.mu.Lock()
		r.isResultUnheard, r.isUnheardNoted = false, false
		r.mu.Unlock()
	}
	ids := make([]string, 0, len(spoken)+len(unheard))
	seen := make(map[string]struct{}, cap(ids))
	add := func(id string) {
		if _, ok := seen[id]; ok || id == "" {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range unheard {
		add(id)
	}
	isCutShort := status != "" && status != statusCompleted
	if isCutShort && !r.hasResponseAudio.Load() {
		for _, id := range spoken {
			add(id)
		}
		for _, item := range response.Output {
			if isSpokenItem(item.Type) {
				add(item.ID)
			}
		}
	}
	for _, id := range ids {
		r.mu.Lock()
		if r.responseItemID == id {
			r.responseItemID = ""
		}
		r.mu.Unlock()
		if err := r.deleteUnheardItem(id, status); err != nil {
			r.log.Debug("could not take an unheard turn out of the conversation",
				"itemId", id, "error", err)
		}
	}
	// The turn that answered a tool result went unheard: the caller's next
	// reply must give it. A turn stopped for a line is not: the line is the
	// next turn, and it must say the line alone.
	if isAnswer && len(ids) > 0 && !isPreempted {
		if err := r.noteUnheardResult(); err != nil {
			r.log.Debug("could not tell the model the result went unheard", "error", err)
		}
	}
}

// noteUnheardResult tells the model, as a note in the conversation, that the
// caller has not heard the tool result it answered, so its next reply gives it.
// Once per result: SendToolResult arms it again.
//
// Taking the unheard answer out (removeUnheardTurn) is not enough on its own.
// On #286 live call 01a1627d-7eae-77c2-98f7-e997fca088da the answer was cut
// before its audio and its item deleted, and the reply to the caller's "你在吗"
// was still "在的，请问有什么可以帮您？", the reply qwen gives when the history
// holds the answer as said. Measured on TestLiveQwenUnheardResultNote
// (qwen-findings W-Q9): with the cut answer's words left in the history the
// reply gave the result 1 of 8, with this note after them 9 of 9; with the
// history clean it gave it 12 of 12 without the note and 4 of 4 with it, and a
// follow-up question was answered alike either way. Why the live history was
// not clean is not known; the note is what holds whatever is left in it.
func (r *Realtime) noteUnheardResult() error {
	r.mu.Lock()
	if r.isUnheardNoted {
		r.mu.Unlock()
		return nil
	}
	r.isUnheardNoted = true
	language := r.cfg.Language
	r.mu.Unlock()
	if err := r.sendEvent(userTextItem(unheardResultNote(language))); err != nil {
		return err
	}
	r.log.Info("told the model the caller has not heard the tool's result")
	return nil
}

// unheardResultNote is the note noteUnheardResult puts in the conversation, in
// the session's language.
func unheardResultNote(language string) string {
	if strings.HasPrefix(strings.ToLower(language), "zh") {
		return "（系统提示：刚才的查询结果还没有说给来电者听，来电者一个字也没有听到。" +
			"下一次回复来电者时，先简短回应他，然后把查询结果告诉他。）"
	}
	return "(System note: the caller has not heard the result of the lookup yet, " +
		"not a word of it. In your next reply to the caller, first respond briefly, " +
		"then tell them the result.)"
}

// deleteUnheardItem takes one spoken item the caller heard none of out of the
// conversation. Said at Info: an answer the model wrote and the caller never
// heard is invisible in the call's transcript, which records what the model
// wrote, and the log line is the trace that it was taken back.
func (r *Realtime) deleteUnheardItem(itemID, status string) error {
	r.mu.Lock()
	r.deleteSentAt = time.Now()
	r.mu.Unlock()
	if err := r.sendEvent(map[string]any{
		"type":    "conversation.item.delete",
		"item_id": itemID,
	}); err != nil {
		return err
	}
	r.log.Info("took a turn the caller heard none of out of the conversation",
		"itemId", itemID, "status", status)
	return nil
}

// isAnsweredDelete reports whether an error is the provider refusing an item
// delete this client sent: the item was already gone, or the dialect does not
// have the event. Either way the history is as clean as it can be made, so it
// is not reported.
//
// Matched like isAnsweredCancel: within cancelAnswerWindow of a delete we sent,
// and on words that name a missing item or an unsupported event, because the
// error names neither the frame it refuses nor the item.
func (r *Realtime) isAnsweredDelete(err *wireError) bool {
	r.mu.Lock()
	sentAt := r.deleteSentAt
	r.mu.Unlock()
	if sentAt.IsZero() || time.Since(sentAt) > cancelAnswerWindow {
		return false
	}
	message := strings.ToLower(err.Message)
	if err.Code == "item_not_found" || err.Param == "item_id" {
		return true
	}
	for _, words := range []string{"not found", "does not exist", "no such item", "unknown event", "unsupported", "not supported", "conversation.item.delete"} {
		if strings.Contains(message, words) {
			return true
		}
	}
	return false
}

func (r *Realtime) handleError(event *wireEvent) {
	if event.Error == nil {
		r.emit(Event{Type: EventTypeError, Err: errors.New("provider reported an unspecified error")})
		return
	}
	err := event.Error

	// A rejected configuration takes the whole session with it — persona,
	// tools and all — so it is worth one retry without the optional parts
	// before giving up on the call.
	if err.Param == "session.update" && r.canRetrySession() {
		r.log.Warn("session configuration rejected, retrying without optional fields",
			"code", err.Code, "message", err.Message)
		r.mu.Lock()
		cfg := r.cfg
		r.mu.Unlock()
		if sendErr := r.sendEvent(r.buildSessionUpdate(cfg, true)); sendErr == nil {
			return
		}
	}

	// A cancel that lost the race with the response ending changes nothing:
	// the turn is over either way, and its response.done has already said
	// so. It is not reported, so the WARN count of a healthy call means
	// something.
	if r.isAnsweredCancel(err) {
		r.log.Debug("the provider refused a cancel because the response had already ended",
			"code", err.Code, "message", err.Message)
		return
	}

	if r.isAnsweredDelete(err) {
		r.log.Debug("the provider refused to delete an item",
			"code", err.Code, "message", err.Message)
		return
	}

	// A request sent in place of one the provider was only late to create is
	// refused because that one is now running: the late turn is the turn, and
	// the state belongs to it, so nothing here is cleared or reported. A line
	// that superseded the late request is asked for again, pre-empting it as
	// any line does, because a newer line is the one still true.
	if isRecovery, line := r.isAnsweredRecovery(err); isRecovery {
		r.log.Debug("the provider refused the request sent in place of one it was only late to create",
			"code", err.Code, "message", err.Message, "isLineAskedAgain", line != nil)
		if line != nil {
			r.mu.Lock()
			isNewerLine := r.pendingSpeak != ""
			r.mu.Unlock()
			if !isNewerLine {
				if speakErr := r.SpeakText(line.text, line.isClosing); speakErr != nil {
					r.log.Warn("could not ask again for the line the late turn displaced", "error", speakErr)
				}
			}
		}
		return
	}

	// A request for a turn that the provider refused for any reason but a
	// response already running will never be created, so it must not go on
	// covering the tool results that follow it (SendToolResult).
	//
	// A turn owed past that request (a result or a cue that arrived while a
	// line was on its way) was waiting for the line's turn to end, and there
	// will be none: it is asked for now. Claiming the floor discharges it, so
	// a refusal of that request too cannot loop.
	if !strings.Contains(err.Message, "another response is in progress") {
		r.mu.Lock()
		isSettled := !r.isResponseOpen.Load() && r.isResponseRequested
		if !r.isResponseOpen.Load() {
			r.settleRequestLocked()
		}
		isOwedStranded := isSettled && r.isTurnOwed && r.isOwedPastCreate
		if isOwedStranded {
			r.isOwedPastCreate = false
		}
		r.mu.Unlock()
		if isOwedStranded {
			r.releaseOwedTurn()
		}
	}

	r.finishStart(err)
	r.emit(Event{Type: EventTypeError, Err: err, Text: err.Message})
}

// canRetrySession allows exactly one reduced retry, before the session is up.
func (r *Realtime) canRetrySession() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.ready:
		return false
	default:
	}
	if r.isSessionRetried {
		return false
	}
	r.isSessionRetried = true
	return true
}

// finishStart releases Start, recording the first failure if there was one.
func (r *Realtime) finishStart(err error) {
	r.readyOnce.Do(func() {
		if err != nil {
			r.startErr.set(err)
		}
		close(r.ready)
	})
}

// onResponseStalled closes out a turn the provider walked away from. The
// watchdog calls it on its own goroutine, having already decided that a
// response really is open.
func (r *Realtime) onResponseStalled(hasAudioArrived bool) {
	reason := "the provider never started speaking"
	if hasAudioArrived {
		reason = "the provider stopped partway through speaking"
	}
	r.isResponseOpen.Store(false)
	r.log.Warn("response abandoned", "reason", reason,
		"hasAudioArrived", hasAudioArrived)
	// This path never reaches handleResponseDone, so a line waiting for
	// a turn the provider walked away from would wait for ever.
	r.dispatchPendingSpeak(false)
	// Nor would a tool result's turn that was waiting for it.
	r.releaseOwedTurn()
	// Not fatal: the session is still usable, and the caller has heard
	// whatever did arrive. The flow decides what to say next.
	r.emit(Event{Type: EventTypeError, Text: reason,
		Err: errors.New(reason)})
	r.emit(Event{Type: EventTypeResponseDone})
}

// signal notifies the watchdog without ever blocking the read loop.
func (r *Realtime) signal(s WatchSignal) { r.dog.Signal(s) }

// emit delivers an event, dropping it only once the session is finished.
func (r *Realtime) emit(event Event) {
	select {
	case r.events <- event:
	case <-r.conn.Done():
		// The session is over; nobody is reading any more.
	}
}

func isNormalClosure(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}
