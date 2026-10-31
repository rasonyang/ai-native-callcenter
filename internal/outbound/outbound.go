// SPDX-License-Identifier: Apache-2.0

// Package outbound originates calls: an agent's click-to-dial and the AI
// outbound leg. Both originate a parked leg first and act only when a human
// actually answers. Click-to-dial then transfers the answered agent leg into
// the dialplan, which owns all routing (an extension stays internal, a
// carrier number leaves through its gateway); the AI leg is bridged inline
// to the bot gateway, because its target is fixed and carries headers.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/switchsyntax"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// Switch is the slice of the adapter vocabulary this package uses.
type Switch interface {
	Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error)
	BridgeToEndpoint(channelID string, newPartyID uuid.UUID, endpoint string, vars map[string]string) error
	TransferToExtension(channelID, extension, context string) error
	Hangup(channelID, cause string) error
	SetVariable(channelID, name, value string) error
	// ChannelExists asks the switch whether a channel is up: how a command it
	// did not confirm is settled (telephony.EndLeg, bridge.go).
	ChannelExists(channelID string) (bool, error)
	// StartCallRecording starts the call's stereo recording on an answered
	// channel, filed under key (recording.Key).
	StartCallRecording(channelID, key string) error
	Endpoint(extensionNumber string) string
}

// BotSlots is the bot-capacity counter an answered AI outbound leg takes a slot
// from before it is bridged (voice.BotSlots). One number is shared with the
// UAS, which consumes the reservation when the bridge's INVITE arrives.
type BotSlots interface {
	// ReserveWait reserves a slot under key, parking for up to hold until one
	// frees, and gives up early when ctx is done.
	ReserveWait(ctx context.Context, key string, hold time.Duration) (ok bool, waited time.Duration)
	// Release gives back a reservation the INVITE has not consumed.
	Release(key string)
	// Keep holds the reservation under key for d from now; zero is the
	// counter's own expiry. It reports whether the key holds one, and never
	// reserves a key that holds none.
	Keep(key string, d time.Duration) bool
	// Withdraw takes the reservation back and refuses the key to a later
	// INVITE; it reports whether a reservation was still held (false: the
	// INVITE consumed it, or it lapsed).
	Withdraw(key string) bool
}

// DIDSource answers which numbers exist and which flow serves them.
type DIDSource interface {
	DIDs(ctx context.Context) ([]catalog.DID, error)
}

// NumberPlan answers whether a number belongs to the switch itself: a
// provisioned extension or a queue's extension number.
type NumberPlan interface {
	IsInternalNumber(ctx context.Context, number string) (bool, error)
}

// DNCList answers whether a canonical E.164 number is on the do-not-call list.
type DNCList interface {
	Contains(ctx context.Context, number string) (bool, error)
}

// Config shapes how the outside world is dialed.
type Config struct {
	// EndpointFormat renders a destination number into a dial string for the
	// AI outbound leg, e.g. "sofia/gateway/pstn/%s". Only that path needs it:
	// click-to-dial hands the destination to the dialplan instead. There is
	// no default and the config layer guarantees it is a sofia/ dial string
	// with one %s; loopback cannot carry this path (C47). Empty refuses the
	// call with ErrNoOutboundEndpoint.
	EndpointFormat string
	// SIPHeaders are stamped on the AI outbound customer leg as sip_h_<Name>
	// and nowhere else: not click-to-dial, not the bridge to the bot. Already
	// validated by the config layer.
	SIPHeaders []config.SIPHeader
	// BotGateway is the gateway name the switch bridges AI legs through.
	BotGateway string
	// CallerID presented on click-to-dial customer legs; empty leaves the
	// carrier default.
	CallerID string
	// RatePerSec caps originates per second; the switch's own cap rejects
	// while this one delays. Default 100.
	RatePerSec int
	// AnswerWindow is how long an originated leg may ring before the armed
	// continuation is abandoned. Default config.OutboundAnswerWindow (60s).
	AnswerWindow time.Duration
	// RingTimeout is the default originate_timeout of an originated leg: how
	// long the switch rings it. Must be strictly below AnswerWindow (the
	// config layer validates it; a request is checked again at call time).
	// Default config.DefaultOutboundRingTimeout (45s).
	RingTimeout time.Duration
	// Slots is the bot-capacity counter. Nil bridges without reserving, which
	// is only right for a deployment that never fills its bot.
	Slots BotSlots
	// OverflowHold is how long an answered leg waits for a slot before it is
	// hung up as OverflowCause. Zero waits not at all.
	OverflowHold time.Duration
	// DefaultCountryCode (digits, no "+") completes a national destination
	// before the do-not-call check. Empty: a national number cannot be
	// completed and is not checked.
	DefaultCountryCode string
}

func (c Config) withDefaults() Config {
	// No default, deliberately. It used to be loopback/%s/aicc/XML, which
	// routed the call through the dialplan and looked like it worked — and on
	// an AI call it did not: origination_uuid pins the loopback's own A leg,
	// the customer is reached on the B leg bridged out beyond it, and the A
	// leg is gone by the time the bot has anything to say about the caller.
	// Every stamp and every transfer then came back "No such channel!", so an
	// AI outbound call could never be handed to a person (C47).
	//
	// Which endpoint reaches a carrier is a fact about the deployment and
	// there is no value that is right for two of them. Rather than ship one
	// that is wrong quietly, an unset endpoint refuses the call and says so.
	if c.BotGateway == "" {
		c.BotGateway = "aicc_bot"
	}
	if c.RatePerSec <= 0 {
		c.RatePerSec = 100
	}
	if c.AnswerWindow <= 0 {
		c.AnswerWindow = config.OutboundAnswerWindow
	}
	if c.RingTimeout <= 0 {
		c.RingTimeout = config.DefaultOutboundRingTimeout
	}
	return c
}

// Service originates and follows up outbound legs.
type Service struct {
	cfg     Config
	sw      Switch
	dids    DIDSource
	numbers NumberPlan
	dnc     DNCList
	hasCDR  func(ctx context.Context, callID uuid.UUID) (bool, error)
	isLive  func(callID uuid.UUID) bool
	limiter *Limiter
	log     *slog.Logger
	// abandonBot hands a call whose bot session died to the telephony CDR path.
	abandonBot func(uuid.UUID)
	// callData holds what a placed call was asked to carry until the call
	// itself exists to hold it.
	callData *CallData
	// lines is the DIDs' outbound line account (lines.go).
	lines *Lines

	mu      sync.Mutex
	pending map[string]*pendingLeg // by channel id
	// ready is the admission gate (adopt.go): nil is open. isReady records
	// that it was opened. answered and reaped are short memories that keep a
	// recovery pass from deciding twice about one channel.
	ready   chan struct{}
	isReady bool
	// readyWait overrides reconcileWait; tests shorten it.
	readyWait time.Duration
	answered  map[string]time.Time
	reaped    map[string]time.Time
	now       func() time.Time // the clock; replaced in tests
	// onAnswered, when set, hears every answered AI outbound leg.
	onAnswered func(callID uuid.UUID)
	// callStart says when the registry's call began, the instant the
	// recording's key date must come from (the CDR ingest files by it too).
	callStart func(callID uuid.UUID) (time.Time, bool)

	// Originates awaiting their BACKGROUND_JOB result (jobs.go), guarded by mu.
	jobs      map[string]*trackedJob // by Job-UUID
	legJobs   map[string]*trackedJob // by leg channel id, from before the originate
	early     map[string]earlyResult
	jobMargin time.Duration
	failures  FailureLedger
	// placing is the callIds with a placement in progress (claimCall), and
	// lateLegs the legs of unconfirmed placements given up on
	// (unconfirmed.go); both guarded by mu.
	placing  map[uuid.UUID]struct{}
	lateLegs map[string]lateLeg
	// timers, when set, replaces time.AfterFunc for the placement windows;
	// tests only.
	timers func(d time.Duration, f func()) stopper
	// beforeHold, when set, runs when an originate has returned unconfirmed
	// and before the placement is held, with the leg's id: a test dispatches
	// a job result in that gap. Tests only.
	beforeHold func(legID string)

	// holds are the answered AI legs that took, or are waiting for, a bot slot,
	// by channel id; guarded by mu. A hangup cancels the wait and releases the
	// reservation.
	holds map[string]*slotHold
	// bridges are the bridges to the bot the switch has not confirmed, by
	// customer leg (bridge.go); guarded by mu.
	bridges map[string]*pendingBridge
}

// slotHold is one answered leg's claim on a bot slot.
type slotHold struct {
	key    string
	cancel context.CancelFunc
	since  time.Time
}

// OverflowCause is the hangupCause of an answered AI outbound call that no bot
// slot could take. It is this platform's own word, not the switch's: the leg
// is stamped with it (telephony.VarHangupCause) before it is hung up.
const OverflowCause = "OVERFLOW"

// LateAnswerCause is the hangup cause stamped on a leg that was answered after
// its window closed. It reaches the CDR's hangupCause (a free string) as the
// leg's release cause, so the ledger tells a late answer from a normal ending.
const LateAnswerCause = "ALLOTTED_TIMEOUT"

// ringTimeout resolves a request's ring timeout against the service default
// and refuses one that would outlast the answer window.
func (s *Service) ringTimeout(requested time.Duration) (time.Duration, error) {
	d := requested
	if d == 0 {
		d = s.cfg.RingTimeout
	}
	if d <= 0 || d >= s.cfg.AnswerWindow {
		return 0, ErrBadRingTimeout
	}
	return d, nil
}

// ringTimeoutVar renders originate_timeout in whole seconds, rounded up.
func ringTimeoutVar(d time.Duration) string {
	return strconv.Itoa(int((d + time.Second - 1) / time.Second))
}

// continuation is what an originated leg does once somebody has answered it.
// It has two parts because two different moments matter. The switch's
// originate owns the leg until it writes its +OK (the BACKGROUND_JOB result),
// and a transfer that lands earlier is overwritten by the originate's own
// &park() continuation, leaving the person on a silent leg (#333). So the
// answer itself only starts the clocks (onAnswer), and the move that talks to
// the switch (proceed) waits for the +OK as well.
type continuation struct {
	// onAnswer, when set, runs on the event goroutine at the in-time
	// CHANNEL_ANSWER, once; it must not block.
	onAnswer func()
	// proceed runs, once, off the event goroutine, when the leg was answered
	// in time and the originate has let go of it.
	proceed func()
}

// pendingLeg is a continuation armed on an originated channel. It fires when
// both signals are in, in either order: the leg's CHANNEL_ANSWER (in time)
// and its originate job's +OK. They come from different switch threads, so
// whichever is second takes the entry out of Service.pending under the lock,
// which is what makes the continuation run once.
type pendingLeg struct {
	cont continuation
	// expiresAt ends the wait for an answer. An answer inside it is in time
	// by definition: it no longer applies once answeredAt is set.
	expiresAt time.Time
	// answeredAt is when the in-time answer was seen; zero until then.
	answeredAt time.Time
	// isReleased is set when the originate's +OK was seen.
	isReleased bool
}

// New builds the service. hasCDR and isLive make retries idempotent: a call
// the ledger already closed, or one still running, is never dialed again.
func New(cfg Config, sw Switch, dids DIDSource, numbers NumberPlan,
	hasCDR func(context.Context, uuid.UUID) (bool, error),
	isLive func(uuid.UUID) bool, log *slog.Logger) *Service {

	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:      cfg,
		sw:       sw,
		dids:     dids,
		numbers:  numbers,
		hasCDR:   hasCDR,
		isLive:   isLive,
		limiter:  NewLimiter(cfg.RatePerSec),
		callData: NewCallData(),
		lines:    NewLines(),
		log:      log,
		pending:  map[string]*pendingLeg{},
		holds:    map[string]*slotHold{},
		now:      time.Now,
	}
}

// WithDNC installs the do-not-call list. Without one nothing is checked.
func (s *Service) WithDNC(dnc DNCList) *Service {
	s.dnc = dnc
	return s
}

// checkDNC is the one choke point for the do-not-call list: Dial (click-to-dial)
// and DialAI both call it before any state is created or any command is sent.
//
// The list holds canonical E.164, so the destination is canonicalised first,
// with the deployment's default country code (AICC_DEFAULT_COUNTRY_CODE, which
// defaults to 1, so a national number is always completed). A destination
// written with "+" or "00" is compared as written. A number that cannot be
// canonicalised at all is not on the list by construction; that is logged at
// debug. A failing lookup refuses the call: dialling a number we could not
// prove is allowed is the worse error.
func (s *Service) checkDNC(ctx context.Context, destination string) error {
	if s.dnc == nil {
		return nil
	}
	number, err := phonenum.Canonical(destination, s.cfg.DefaultCountryCode)
	if err != nil {
		s.log.DebugContext(ctx, "do-not-call check skipped: destination cannot be canonicalised",
			"to", destination, "reason", phonenum.ReasonOf(err),
			"hasDefaultCountryCode", s.cfg.DefaultCountryCode != "")
		return nil
	}
	isListed, err := s.dnc.Contains(ctx, number)
	if err != nil {
		return fmt.Errorf("outbound: do-not-call lookup: %w", err)
	}
	if isListed {
		return ErrNumberOnDNC
	}
	return nil
}

// Errors the API layer translates.
var (
	ErrBadNumber = fmt.Errorf("outbound: not a dialable number")
	// ErrNoOutboundEndpoint means nobody has said how this deployment reaches
	// a carrier. There is no default worth guessing: the one that used to be
	// here routed through the dialplan by loopback and left every AI outbound
	// call unable to reach a person (C47).
	ErrNoOutboundEndpoint = fmt.Errorf("outbound: AICC_OUTBOUND_ENDPOINT is not set")
	// ErrNoDefaultOutbound means no number is marked as the one calls go out
	// from. Refused rather than defaulted: the alternative is every call
	// presenting whatever the gateway carries, which nobody chose and nothing
	// records.
	ErrNoDefaultOutbound = fmt.Errorf("outbound: no number is marked as the default outbound one")
	ErrUnknownDID        = fmt.Errorf("outbound: no such DID")
	ErrFlowless          = fmt.Errorf("outbound: the DID has no published flow")
	// ErrDIDNotOutbound means the DID named as the caller id is inbound-only
	// (allowOutbound false). Refused before anything is recorded or sent: a
	// number the operator never opened for outbound is not one to present.
	ErrDIDNotOutbound = fmt.Errorf("outbound: the DID does not allow outbound calls")
	ErrAlreadyPlaced  = fmt.Errorf("outbound: this call was already placed")
	// ErrNumberOnDNC means the destination is on the do-not-call list. The
	// call is refused before anything is recorded or sent: the CDR is the
	// ledger of calls placed, and this one was not.
	ErrNumberOnDNC = fmt.Errorf("outbound: the number is on the do-not-call list")
	// ErrBadRingTimeout means a ring timeout that would let the leg ring past
	// the answer window.
	ErrBadRingTimeout = fmt.Errorf("outbound: ring timeout must be positive and strictly below the answer window")
	// ErrDIDChannelsExhausted means the DID the call goes out from is at its
	// outbound line limit (dids.max_outbound_channels). Nothing was sent and
	// nothing is recorded: the CDR is the ledger of calls placed.
	ErrDIDChannelsExhausted = fmt.Errorf("outbound: the DID is at its outbound line limit")
	// ErrBadLanguage means the language the call would carry holds something
	// the switch reads as syntax, so the bridge to the bot would be refused.
	// Refused before anything is sent: refusing at the bridge would leave a
	// customer who has answered on a silent line. The API refuses such a
	// language in a request, so this is a number's language written before it
	// did.
	ErrBadLanguage = fmt.Errorf("outbound: the call's language cannot be sent to the switch")
)

// pinCodecs adds the G.711 pin on legs that leave through sofia. Loopback
// legs run L16 internally and refuse the pin outright — a pinned loopback
// dies with DESTINATION_OUT_OF_ORDER before it ever routes (found live).
// One codec, not a list: a comma inside a {var=v,var=v} block is a list
// separator, and the parse error cancels the whole originate (found live).
func pinCodecs(vars map[string]string, endpoint string) {
	if strings.HasPrefix(endpoint, "sofia/") {
		vars["absolute_codec_string"] = "PCMU"
	}
}

// callTypeFor classifies a dialled destination for the ledger. The callee
// decides (owner rule): a number internal to the switch, meaning a provisioned
// extension (registered or not) or a queue's extension number, is INTERNAL;
// anything else, a platform DID or an external number, is OUTBOUND. The ledger
// needs the answer before the second leg exists, so it is looked up here.
func (s *Service) callTypeFor(ctx context.Context, destination string) (string, error) {
	if s.numbers == nil {
		return "OUTBOUND", nil
	}
	internal, err := s.numbers.IsInternalNumber(ctx, destination)
	if err != nil {
		return "", err
	}
	if internal {
		return "INTERNAL", nil
	}
	return "OUTBOUND", nil
}

// isDialable keeps dial strings boring: one optional leading "+", then ASCII
// digits, 3 to 20 of them. Everything else stays out of command lines by
// construction. Separators, a second "+" and rewriting a national number are
// the import normaliser's job (internal/phonenum), not the dial path's: a
// destination is dialled, and recorded in the CDR, exactly as sent, so a single
// call may carry a national number or canonical E.164 with its "+".
func isDialable(number string) bool {
	return isDigits(strings.TrimPrefix(number, "+"))
}

// isExtension is the strict form for an agent's phone: digits only, no "+".
func isExtension(number string) bool {
	return isDigits(number)
}

func isDigits(number string) bool {
	if len(number) < 3 || len(number) > 20 {
		return false
	}
	for _, ch := range number {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// AgentDialRequest is a click-to-dial: which phone to raise, and what to dial
// from it once somebody picks up.
type AgentDialRequest struct {
	// CallID makes retries idempotent; zero mints a fresh identity.
	CallID uuid.UUID
	// AgentExtension is the phone raised first.
	AgentExtension string
	// To is the number dialled once that phone answers.
	To string
	// CallcenterName is the switch's own name for the agent at that phone, so
	// its queues can be told they are busy. Empty when nobody is signed in
	// there — a call placed for an agent who never logged into this
	// application still goes out, it simply has no queue membership to guard.
	CallcenterName string
	// UserData is business data to carry into the call and its ledger row.
	UserData map[string]string
	// RingTimeout overrides the service's ring timeout for this attempt; zero
	// uses the default. Must be strictly below the answer window.
	RingTimeout time.Duration
}

// Dial is click-to-dial: ring the agent first, and only dial out once the
// agent leg is up. The answered leg is then transferred into the dialplan at
// the destination, which routes it the same way a phone dialling that number
// would be routed — no second copy of the routing rules here, and no loopback
// legs to trace.
//
// The agent's phone auto-answers, whoever asked for the call. That is
// obviously right when the agent clicked, and it is a deliberate choice when a
// system placed the call for them (owner directive 2026-08-26): this is
// preview dialing, where the phone going live is the notification. An agent
// whose phone should ring instead would need a second mode, and there is no
// call for one yet.
func (s *Service) Dial(ctx context.Context, req AgentDialRequest) (uuid.UUID, error) {
	agentExtension, destination := req.AgentExtension, req.To
	userData, callcenterName := req.UserData, req.CallcenterName
	if !isDialable(destination) || !isExtension(agentExtension) {
		return uuid.Nil, ErrBadNumber
	}
	ring, err := s.ringTimeout(req.RingTimeout)
	if err != nil {
		return uuid.Nil, err
	}
	// Before anything that consults what is live: until the first reconcile
	// the registry does not know the calls that outlived a restart.
	if err := s.awaitReconciled(ctx); err != nil {
		return uuid.Nil, err
	}

	// The same idempotency record DialAI uses, and for a sharper reason here:
	// the caller is a system that retries on a timeout, and a retried
	// click-to-dial raises the agent's phone a second time while they are
	// still talking on the first one.
	callID, err := s.checkPlaced(ctx, req.CallID)
	if errors.Is(err, ErrAlreadyPlaced) {
		return callID, err
	}
	if err != nil {
		return uuid.Nil, err
	}
	// After the idempotency check: a call already placed is reported as
	// placed, even if its number joined the list since.
	if err := s.checkDNC(ctx, destination); err != nil {
		return uuid.Nil, err
	}
	if err := s.claimCall(ctx, callID, req.CallID == uuid.Nil); err != nil {
		return callID, err
	}
	// Held until the placement is tracked (expectLeg), which then holds it.
	isClaimHeld := true
	defer func() {
		if isClaimHeld {
			s.unclaim(callID)
		}
	}()

	if err := s.limiter.Take(ctx); err != nil {
		return uuid.Nil, err
	}

	// A store failure refuses the dial, like the default-outbound lookup
	// below: guessing a type would stamp a wrong one on a ledger row that
	// never changes. Nothing is held yet, so there is nothing to drop.
	callType, err := s.callTypeFor(ctx, destination)
	if err != nil {
		return uuid.Nil, err
	}

	agentLeg := uuid.New()

	// Before the originate, for the same reason as an AI call: the agent's
	// leg can reach the registry while this function is still running.
	if s.callData != nil {
		s.callData.Put(callID, userData)
	}

	vars := map[string]string{
		"aicc_call_id": callID.String(),
		// What this leg is for, so a process that did not place it (after a
		// restart, say) can tell (telephony.AdoptLiveChannels).
		telephony.VarPlacedBy: telephony.PlacedByDial,
		// The identity crosses the bridge with the leg. The trunk leg the
		// dialplan raises next inherits aicc_call_id, so it is adopted into
		// this call the moment it exists — not merged later at the bridge,
		// which a failed originate never reaches. Without this, a dial whose
		// agent leg died before bridging left the trunk leg on a provisional
		// call of its own, and the ledger showed two OUTBOUND rows for one
		// click (walkthrough, 2026-08-28). One name only: inside an originate
		// {…} block a comma separates variables, so a two-name export_vars
		// would be cut in half.
		"export_vars":     "aicc_call_id",
		"sip_auto_answer": "true",
		// sip_auto_answer is the agent's phone picking up its own call, and
		// nobody else's. mod_sofia turns it into sip_h_Call-Info=…;answer-after=0
		// on this leg, and copies every sip_h_* of a leg onto each leg it dials
		// from it: the trunk leg, then whatever that leg is transferred to. An
		// extension it reached answered with no ring (#266). Gone once the
		// agent answered, which is before the dialplan bridges anywhere: the
		// answer runs this on the leg's own thread. Not
		// sip_copy_custom_headers=false, which would stop every other header
		// that should cross.
		"execute_on_answer":            "unset sip_h_Call-Info",
		"origination_caller_id_number": destination,
		// No space in the name: it travels inside an originate {…} block,
		// where a space ends the block and kills the call before it routes.
		"origination_caller_id_name": "Dial-" + destination,
		// Who placed it: an originated leg carries no directory lookup, so
		// without this the agent is missing from their own call's record.
		"aicc_extension": agentExtension,
		// The number this call is *for*. The switch cannot be asked: a
		// click-to-dial raises the agent's leg at `user/1008@domain`, the
		// directory resolves that to the contact the browser registered
		// under, and the leg is created with Caller-Destination-Number
		// = "6p2g7hjk" — the registration token, not a number anyone dialled
		// (measured live, artifacts/C61). PARTY_DIALING is right to refuse a
		// token, and was therefore announcing an agent dialling nobody. The
		// destination was never in doubt here — it is the request's own
		// argument — so it travels with the leg instead of being guessed off
		// it. A leg an agent dials from their own phone carries the real
		// number in the usual place and needs none of this.
		"aicc_destination": destination,
		// What this call is, decided by the callee where the destination is
		// still known: to the switch every originated leg is simply outbound.
		"aicc_call_type": callType,
		// The agent leg speaks telephone audio, not what a browser would
		// prefer: the leg the dialplan raises next inherits this one's codec,
		// and a G.711-only phone answers an inherited opus offer with
		// INCOMPATIBLE_DESTINATION (found live). One codec, no comma — a
		// comma inside the block is a variable separator.
		"absolute_codec_string": "PCMU",
		// Explicit and below the answer window: the switch's own default is
		// about the window, which leaves an answer on either side of it.
		"originate_timeout": ringTimeoutVar(ring),
	}
	// Tell mod_callcenter this agent is on a call, so its queues stop offering
	// them one while it lasts.
	//
	// The module only knows about calls it placed: for anything else
	// agents.state stays Waiting and a queue rings a phone that is already
	// talking. The phone then says no — 486, which costs a busy delay, or 480
	// on its slot race, which the module counts as a call the agent failed to
	// answer. external_calls_count is the field it keeps for exactly this and
	// skip-agents-with-external-calls reads it by default; nothing was filling
	// it in. callcenter_track owns the increment, the decrement on hangup and
	// the state hook behind it — we only have to name the agent.
	//
	// On ring, not on answer: a phone that is ringing is already engaged, and
	// the window this closes is a queue offering a second call into it — which
	// is exactly when the phone says 486, or 480 on its slot race. Waiting for
	// the answer leaves that window open for the whole of the ringing.
	//
	// A leg that rings out and is never answered costs nothing: the decrement
	// hangs off CS_HANGUP, so an abandoned ring puts the count back like any
	// other ending. execute_on_media would not do — with early media ignored
	// on this path it fires at answer anyway (switch_channel.c:3900), which is
	// the same moment under a different name.
	//
	// The value has a space in it and is escaped on the way into the block; an
	// agent the module does not know costs a warning in its log and nothing
	// else.
	if callcenterName != "" {
		vars["execute_on_ring"] = "callcenter_track " + callcenterName
	}
	// Tell mod_callcenter this agent is on a call, so its queues stop offering
	// them one.
	//
	// A call the agent placed themselves is invisible to the module: it only
	// tracks what it dispatched, so agents.state stays Waiting and the queue
	// happily rings a phone that is already talking. The phone then answers
	// 486, which is correct and costs the agent a busy delay — and 480 on the
	// slot race, which mod_callcenter counts as a call they failed to answer.
	// external_calls_count is the field the module keeps for exactly this, and
	// skip-agents-with-external-calls consults it by default; nothing was
	// filling it in.
	//
	// callcenter_track takes the increment, the decrement on hangup and the
	// state hook that guarantees it — we only have to say the name. On answer
	// rather than on origination, because a leg that never answered is not a
	// call the agent is on. An unknown name logs a warning inside the module
	// and changes nothing, so the dial goes out either way.

	// Presented onward when the dialplan bridges out; the agent's own display
	// above is the origination_* pair.
	//
	// It comes from the number marked as the deployment's default outbound one,
	// and its absence is a refusal rather than a fallback (D9). Left unset, the
	// trunk presents whatever the gateway happens to be configured with — a
	// number the operator never chose, on every call, discoverable only by
	// asking somebody who was rung what they saw.
	clid, err := s.defaultOutboundNumber(ctx)
	if err != nil {
		if s.callData != nil {
			s.callData.Drop(callID)
		}
		return uuid.Nil, err
	}
	vars["effective_caller_id_number"] = clid
	// The line the call will take on the number it goes out from. Reserved from
	// now, when the agent's leg is about to ring, a few seconds before the
	// carrier line is taken: conservative, and the agent's phone answers by
	// itself. The trunk leg inherits aicc_call_id, so it is not counted again.
	if !s.takeLine(callID, clid, s.limitOf(ctx, clid)) {
		if s.callData != nil {
			s.callData.Drop(callID)
		}
		return uuid.Nil, ErrDIDChannelsExhausted
	}
	// Registered before the originate: the leg's CHANNEL_CREATE can be
	// dispatched before the bgapi reply is (jobs.go).
	job := &trackedJob{
		callID: callID, legID: agentLeg.String(), placedAt: time.Now().UTC(),
		callType: callType, from: agentExtension, to: destination,
		legKind: "AGENT", legLabel: agentExtension,
	}
	s.expectLeg(job)
	isClaimHeld = false
	jobID, err := s.sw.Originate(agentLeg, s.sw.Endpoint(agentExtension), vars)
	if isUnconfirmed(err) {
		// The agent's phone may still ring and answer by itself: arm what its
		// answer does, keep everything held, and let the switch decide.
		s.holdUnconfirmed(job, s.transferOnAnswer(callID, agentLeg.String(), destination))
		s.log.Warn("click-to-dial unconfirmed: the switch did not confirm the originate; tracking it",
			"callId", callID, "channelId", agentLeg, "agentExtension", agentExtension,
			"destination", destination, "error", err)
		return callID, unconfirmedError(err)
	}
	if err != nil {
		// Nothing was sent. The line and the business data go before the
		// claim does (abandonLeg): a retry with this callId may claim it the
		// moment it is free, and must not have its own released under it.
		s.lines.Release(callID)
		if s.callData != nil {
			s.callData.Drop(callID)
		}
		s.abandonLeg(job)
		return uuid.Nil, err
	}

	s.arm(agentLeg.String(), s.transferOnAnswer(callID, agentLeg.String(), destination))

	// After arming: a failure already on record is applied at once, and it
	// must find the continuation there to drop.
	s.attachJob(job, jobID)

	s.log.Info("click-to-dial placed", "callId", callID,
		"agentExtension", agentExtension, "destination", destination)
	return callID, nil
}

// takeLine reserves a line on did for the call and sweeps, now and then,
// reservations whose release was never signalled.
func (s *Service) takeLine(callID uuid.UUID, did string, limit *int) bool {
	s.lines.Sweep(s.cfg.AnswerWindow+2*jobMargin, s.isLiveCall)
	return s.lines.Reserve(callID, did, limit)
}

func (s *Service) isLiveCall(callID uuid.UUID) bool { return s.isLive != nil && s.isLive(callID) }

// limitOf is the outbound line limit of the number, nil when it has none or is
// not a DID of the catalog (a caller id set by configuration may not be). A
// failing lookup counts as no limit: the call is not refused for want of a
// figure it could not read.
func (s *Service) limitOf(ctx context.Context, number string) *int {
	dids, err := s.dids.DIDs(ctx)
	if err != nil {
		s.log.Warn("outbound line limit unreadable; not applied", "did", number, "error", err)
		return nil
	}
	for _, d := range dids {
		if d.Number == number {
			return d.MaxOutboundChannels
		}
	}
	return nil
}

// CallEnded gives back the line a call placed here held: its last leg hung up.
// Wire it to the registry's call-finished and call-retired hooks; it is safe to
// call more than once and for calls that held none.
func (s *Service) CallEnded(callID uuid.UUID) { s.lines.Release(callID) }

// SetLinesFreedObserver installs a function told whenever outbound lines are
// given back (a call ended, an originate failed). It must not block. Set before
// the service is used.
func (s *Service) SetLinesFreedObserver(f func()) { s.lines.SetOnFree(f) }

// LinesInUse is the outbound lines held on a DID now.
func (s *Service) LinesInUse(did string) int { return s.lines.InUse(did) }

// DIDLimits lists the DIDs that have an outbound line limit, with the limit.
func (s *Service) DIDLimits(ctx context.Context) (map[string]int, error) {
	dids, err := s.dids.DIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, d := range dids {
		if d.MaxOutboundChannels != nil {
			out[d.Number] = *d.MaxOutboundChannels
		}
	}
	return out, nil
}

// Data exposes what placed calls were asked to carry, for the parts that build
// a call's identity from what the switch tells them and have no other way to
// learn it.
func (s *Service) Data() *CallData { return s.callData }

// AIDialRequest asks for an AI outbound call.
type AIDialRequest struct {
	// CallID makes retries idempotent; zero mints a fresh identity.
	CallID uuid.UUID
	// To is the customer's number.
	To string
	// DIDNumber names the flow (and the caller id) for the conversation.
	DIDNumber string
	// Language overrides the DID's default when set.
	Language string
	// UserData is business data to carry to whoever ends up on the call, and
	// into its ledger row. It is held here rather than sent to the switch.
	UserData map[string]string
	// RingTimeout overrides the service's ring timeout for this attempt (a
	// campaign's per-attempt value); zero uses the default. Must be strictly
	// below the answer window.
	RingTimeout time.Duration
	// LegID pins the customer leg's origination uuid; zero mints one. A
	// campaign attempt mints its leg id with its call id, before the originate,
	// so the row and the channel are the same thing.
	LegID uuid.UUID
	// CampaignID and AttemptID mark the call as a campaign attempt: they ride
	// the leg as channel variables (so a recovery after a restart can arm the
	// bridge with them again) and the bridge to the bot as X-AICC-Campaign-ID
	// and X-AICC-Attempt-ID, which is how the orchestrator knows to withhold
	// a way to a person (design 10 section 10). Both zero for a single call.
	CampaignID uuid.UUID
	AttemptID  uuid.UUID
}

// Placement is what an accepted AI outbound request came to.
type Placement struct {
	CallID uuid.UUID
	// JobID is the Job-UUID the switch's bgapi returned for the originate; the
	// key its BACKGROUND_JOB result will carry. Empty when the switch gave none.
	JobID string
}

// DialAI is the inbound AI path reversed: originate towards the customer,
// park; when they answer, transfer their leg into a bridge to the bot
// gateway with the same correlation headers an inbound call carries.
func (s *Service) DialAI(ctx context.Context, req AIDialRequest) (uuid.UUID, error) {
	p, err := s.PlaceAI(ctx, req)
	return p.CallID, err
}

// CanPlaceAI reports whether this deployment can place an AI outbound call at
// all: it has an outbound endpoint to render a number into. PlaceAI refuses with
// ErrNoOutboundEndpoint exactly when this is false, and it is the one answer
// callers (campaign start and resume) ask instead of keeping a copy of the
// setting.
func (s *Service) CanPlaceAI() bool {
	return s.cfg.EndpointFormat != ""
}

// PlaceAI is DialAI that also says which originate job it started. On an error
// the CallID is set only for ErrAlreadyPlaced and ErrPlacementUnconfirmed.
func (s *Service) PlaceAI(ctx context.Context, req AIDialRequest) (Placement, error) {
	if !s.CanPlaceAI() {
		return Placement{}, ErrNoOutboundEndpoint
	}
	if !isDialable(req.To) {
		return Placement{}, ErrBadNumber
	}
	ring, err := s.ringTimeout(req.RingTimeout)
	if err != nil {
		return Placement{}, err
	}

	if err := s.awaitReconciled(ctx); err != nil {
		return Placement{}, err
	}

	did, err := s.findDID(ctx, req.DIDNumber)
	if err != nil {
		return Placement{}, err
	}
	// Here and not in findDID: recovery (adopt.go) rebuilds the bridge for a
	// leg that was already placed and must still find an inbound-only number
	// whose flag was cleared after the call went out.
	if !did.AllowOutbound {
		return Placement{}, ErrDIDNotOutbound
	}
	language := req.Language
	if language == "" {
		language = did.Language
	}
	// Only what the switch would read as syntax, not the whole tag rule: a
	// number stored as "EN" before languages were checked still places its
	// calls, as it always did.
	if bad := switchsyntax.Unsafe(language); bad != "" {
		return Placement{}, fmt.Errorf("%w: %q holds %q", ErrBadLanguage, language, bad)
	}

	callID, err := s.checkPlaced(ctx, req.CallID)
	if errors.Is(err, ErrAlreadyPlaced) {
		return Placement{CallID: callID}, err
	}
	if err != nil {
		return Placement{}, err
	}
	// After the idempotency check: a call already placed is reported as
	// placed, even if its number joined the list since.
	if err := s.checkDNC(ctx, req.To); err != nil {
		return Placement{}, err
	}
	if err := s.claimCall(ctx, callID, req.CallID == uuid.Nil); err != nil {
		return Placement{CallID: callID}, err
	}
	// Held until the placement is tracked (expectLeg), which then holds it.
	isClaimHeld := true
	defer func() {
		if isClaimHeld {
			s.unclaim(callID)
		}
	}()

	if err := s.limiter.Take(ctx); err != nil {
		return Placement{}, err
	}

	// A line on the DID, taken before anything is sent so that originates fired
	// in the same instant cannot all pass the limit. Given back on every way
	// out below that sends nothing, and when the call ends (CallEnded).
	if !s.takeLine(callID, did.Number, did.MaxOutboundChannels) {
		return Placement{}, ErrDIDChannelsExhausted
	}

	// Before the originate, because the call can exist before this returns:
	// the switch raises the channel and the registry mints the call from it
	// while we are still here. Dropped again on any failure below, so a call
	// that never happened does not leave its business data waiting.
	if s.callData != nil {
		s.callData.Put(callID, req.UserData)
	}

	customerLeg := req.LegID
	if customerLeg == uuid.Nil {
		customerLeg = uuid.New()
	}
	vars := map[string]string{
		"aicc_call_id":  callID.String(),
		"aicc_language": language,
		// What this leg is for and whom it dialled: enough, with the DID and
		// the language above, for a process that did not place it to bridge
		// the bot when the customer answers (adopt.go).
		telephony.VarPlacedBy: telephony.PlacedByDialAI,
		telephony.VarPlacedTo: req.To,
		// The number this call goes out from, on the leg from the moment it
		// exists rather than only on the bridge to the bot. A call nobody
		// answers never reaches that bridge, so its row had neither the number
		// dialled nor the number it was dialled from — the ledger could not
		// tell an outbound campaign ringing out from one that never ran
		// (C53). It is also what lets the row take the direction a platform
		// -placed call has: from the DID, to the customer.
		//
		// Not enough to make this a bot leg: that asks whether the leg was
		// dialled *at* the DID, and this one is dialled at the customer.
		"aicc_did":                     did.Number,
		"origination_caller_id_number": did.Number,
		// The flow the call is for, on the leg from its first moment, so the
		// row of an answered call whose bridge to the bot never ran still names
		// the conversation it was placed for — as the row of one whose
		// originate never made a channel already does (placementFailureCDR).
		// The bot stamps the same flow on the channel when it starts the
		// conversation; a flow being known is not a bot having been there,
		// which is why the journey asks whether the bot joined and not for
		// this (#313).
		"aicc_flow_id": did.FlowID.String(),
		// Explicit and below the answer window; see Dial.
		"originate_timeout": ringTimeoutVar(ring),
	}
	if req.CampaignID != uuid.Nil {
		vars[telephony.VarCampaignID] = req.CampaignID.String()
		vars[telephony.VarAttemptID] = req.AttemptID.String()
	}
	endpoint := fmt.Sprintf(s.cfg.EndpointFormat, req.To)
	pinCodecs(vars, endpoint)
	for _, h := range s.cfg.SIPHeaders {
		vars["sip_h_"+h.Name] = h.Value
	}
	if len(s.cfg.SIPHeaders) > 0 {
		// mod_sofia copies every sip_h_ variable of a channel onto each sofia
		// leg bridged from it, so without this the headers would also ride
		// the bridge to the bot and any later transfer to an agent.
		vars["sip_copy_custom_headers"] = "false"
	}
	job := &trackedJob{
		callID: callID, legID: customerLeg.String(), placedAt: time.Now().UTC(),
		callType: "OUTBOUND", from: did.Number, to: req.To, did: did.Number,
		language: language, flowID: did.FlowID,
		legKind: "TRUNK", legLabel: req.To,
	}
	s.expectLeg(job)
	isClaimHeld = false
	jobID, err := s.sw.Originate(customerLeg, endpoint, vars)
	if isUnconfirmed(err) {
		// The customer may still be called: arm the bridge to the bot, keep
		// the line and the business data, and let the switch decide.
		s.holdUnconfirmed(job, s.bridgeToBot(callID, customerLeg.String(), did.Number, language, req.To, did.IsRecordingEnabled,
			campaignRef{campaignID: req.CampaignID, attemptID: req.AttemptID}))
		s.log.Warn("ai outbound unconfirmed: the switch did not confirm the originate; tracking it",
			"callId", callID, "channelId", customerLeg, "to", req.To, "did", did.Number, "error", err)
		return Placement{CallID: callID}, unconfirmedError(err)
	}
	if err != nil {
		// Nothing was sent. The line and the business data go before the
		// claim does (abandonLeg): a retry with this callId may claim it the
		// moment it is free, and must not have its own released under it.
		s.lines.Release(callID)
		if s.callData != nil {
			s.callData.Drop(callID)
		}
		s.abandonLeg(job)
		return Placement{}, err
	}

	s.arm(customerLeg.String(), s.bridgeToBot(callID, customerLeg.String(), did.Number, language, req.To, did.IsRecordingEnabled,
		campaignRef{campaignID: req.CampaignID, attemptID: req.AttemptID}))

	s.attachJob(job, jobID) // after arming, for the same reason as click-to-dial

	s.log.Info("ai outbound placed", "callId", callID,
		"to", req.To, "did", did.Number, "language", language)
	return Placement{CallID: callID, JobID: jobID}, nil
}

// campaignRef names the campaign attempt a call belongs to; zero for a single
// call.
type campaignRef struct{ campaignID, attemptID uuid.UUID }

// SetAnswerObserver installs a function told, once per call, that the
// customer answered, at the answer itself: before the bridge to the bot, which
// also waits for the originate's result, and before any bot slot is taken. It
// runs on the event goroutine and must not block. Set before the service is
// used.
func (s *Service) SetAnswerObserver(f func(callID uuid.UUID)) { s.onAnswered = f }

// SetCallStart installs the lookup of a call's start time (the registry's
// CallTimeline). The recording of an AI outbound call is filed under the UTC
// date of that instant, the same one the CDR ingest computes; without a
// lookup, or for a call it does not know, the answer time is used. Set before
// the service is used.
func (s *Service) SetCallStart(f func(callID uuid.UUID) (time.Time, bool)) { s.callStart = f }

// startRecording records the answered customer leg of an AI call, before the
// bridge to the bot, so one stereo file (customer left, then bot and agent on
// the right) follows the call through any transfer. A failure is logged and
// never stops the call: a call without a recording beats a dropped call, and
// has_recording stays false because only a stored object sets it.
func (s *Service) startRecording(callID uuid.UUID, customerLeg string) {
	at, ok := time.Time{}, false
	if s.callStart != nil {
		at, ok = s.callStart(callID)
	}
	if !ok || at.IsZero() {
		at = s.now()
	}
	key := recording.Key(at, callID.String())
	if err := s.sw.StartCallRecording(customerLeg, key); err != nil {
		s.log.Warn("ai outbound recording not started; the call goes on unrecorded",
			"callId", callID, "key", key, "error", err)
	}
}

// bridgeToBot is what an answered AI outbound leg does: transfer into a bridge
// to the bot gateway with the correlation headers an inbound call carries.
// Placing a call and adopting one that outlived a restart arm the same thing.
func (s *Service) bridgeToBot(callID uuid.UUID, customerLeg, didNumber, language, to string, record bool, ref campaignRef) continuation {
	return continuation{onAnswer: func() {
		// The first stage of the time to the first word starts here.
		obs.OutboundAnswered(callID, time.Now())
		if s.onAnswered != nil {
			s.onAnswered(callID)
		}
	}, proceed: func() {
		key := callID.String()
		if s.cfg.Slots != nil && !s.takeSlot(callID, customerLeg, to) {
			return
		}
		if record {
			s.startRecording(callID, customerLeg)
		}
		botLeg := uuid.New()
		bridgeVars := map[string]string{
			"aicc_call_id":                    callID.String(),
			"aicc_did":                        didNumber,
			"aicc_language":                   language,
			"sip_h_X-AICC-Call-ID":            callID.String(),
			"sip_h_X-AICC-Channel-ID":         customerLeg,
			"sip_h_X-AICC-DID":                didNumber,
			"sip_h_X-AICC-Language":           language,
			"sip_h_X-AICC-ANI":                to,
			"sip_h_X-AICC-Call-Type":          "OUTBOUND",
			"sip_h_X-AICC-Placed-To-Customer": "true",
		}
		if ref.campaignID != uuid.Nil {
			bridgeVars["sip_h_X-AICC-Campaign-ID"] = ref.campaignID.String()
			bridgeVars["sip_h_X-AICC-Attempt-ID"] = ref.attemptID.String()
		}
		target := "sofia/gateway/" + s.cfg.BotGateway + "/" + didNumber
		pinCodecs(bridgeVars, target)
		// The slot is held over the command and the window after it before
		// the command is sent: its INVITE can come later than the default
		// expiry, and a reservation that lapsed under it would send that
		// INVITE in unreserved (bridge.go).
		isReserved := s.keepForBridge(key)
		err := s.sw.BridgeToEndpoint(customerLeg, botLeg, target, bridgeVars)
		switch {
		case err == nil:
			// Confirmed: the INVITE follows at once, and the reservation
			// lapses on its usual short expiry if it never does.
			if isReserved {
				s.cfg.Slots.Keep(key, 0)
			}
		case isUnconfirmed(err):
			s.log.Warn("ai outbound bridge to bot sent and not confirmed; holding the slot while the switch decides",
				"callId", callID, "to", to, "botLeg", botLeg, "isReserved", isReserved,
				"windowSec", int(bridgeUnconfirmedFor.Seconds()), "error", err)
			s.awaitBridge(callID, customerLeg, botLeg.String(), key, to, isReserved)
		default:
			s.log.Error("ai outbound bridge to bot failed",
				"callId", callID, "to", to, "error", err)
			s.dropHold(customerLeg, key)
		}
	}}
}

// takeSlot reserves a bot slot for an answered leg, parking it for up to the
// overflow hold when none is free. It reports whether the bridge may go ahead.
// When it may not, the leg has already been dealt with: hung up as OVERFLOW,
// or gone because the customer hung up first.
func (s *Service) takeSlot(callID uuid.UUID, customerLeg, to string) bool {
	key := callID.String()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	now := s.now()
	for id, h := range s.holds { // a hangup that raced the answer left these
		if now.Sub(h.since) > time.Minute {
			delete(s.holds, id)
		}
	}
	s.holds[customerLeg] = &slotHold{key: key, cancel: cancel, since: now}
	s.mu.Unlock()

	ok, waited := s.cfg.Slots.ReserveWait(ctx, key, s.cfg.OverflowHold)
	isGone := ctx.Err() != nil
	switch {
	case isGone:
		// The customer hung up while parked (or between reserving and now);
		// the hangup handler has released or will release the reservation.
		s.cfg.Slots.Release(key)
		if waited > 0 {
			obs.RecordBotHold(obs.BotHoldAbandoned, waited.Milliseconds())
		}
		s.log.Info("customer hung up while waiting for a bot slot",
			"callId", callID, "waitedMs", waited.Milliseconds())
		s.dropHold(customerLeg, key)
		return false
	case ok:
		if waited > 0 {
			obs.RecordBotHold(obs.BotHoldReserved, waited.Milliseconds())
		}
		s.log.Info("bot slot reserved for the answered leg",
			"callId", callID, "waitedMs", waited.Milliseconds())
		return true
	}

	obs.RecordBotHold(obs.BotHoldOverflow, waited.Milliseconds())
	s.log.Warn("no bot slot freed within the overflow hold; hanging up the answered leg",
		"callId", callID, "to", to, "heldMs", waited.Milliseconds(), "cause", OverflowCause)
	s.dropHold(customerLeg, key)
	// The stamp first: uuid_kill takes the switch's causes only, so the
	// platform's own word rides a channel variable the normalizer reads.
	s.endLeg(telephony.LegEnding{
		ChannelID:     customerLeg,
		Cause:         "NORMAL_CLEARING",
		PlatformCause: OverflowCause,
		What:          "an overflowed leg",
		Attrs:         []any{"callId", callID},
	})
	return false
}

// dropHold forgets a leg's slot claim and cancels any wait for it.
func (s *Service) dropHold(customerLeg, key string) {
	s.mu.Lock()
	h := s.holds[customerLeg]
	delete(s.holds, customerLeg)
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
	// Whatever was reserved and not consumed goes back; a no-op when nothing
	// was or the INVITE took it.
	if s.cfg.Slots != nil {
		s.cfg.Slots.Release(key)
	}
}

// transferOnAnswer is what an answered click-to-dial agent leg does: go on to
// the destination through the dialplan. It has nothing to do at the answer
// itself, only once the originate has let go of the leg.
func (s *Service) transferOnAnswer(callID uuid.UUID, agentLeg, destination string) continuation {
	return continuation{proceed: func() {
		err := s.sw.TransferToExtension(agentLeg, destination, "aicc")
		switch {
		case err == nil:
		case isUnconfirmed(err):
			// Not a failure: a stalled switch runs it when it recovers, and
			// the agent's phone then dials on. Sent once, never again: the
			// agent can always hang their own phone up.
			s.log.Warn("click-to-dial transfer sent and not confirmed; the switch may still run it",
				"callId", callID, "channelId", agentLeg, "destination", destination, "error", err)
		default:
			s.log.Error("click-to-dial transfer failed",
				"callId", callID, "destination", destination, "error", err)
		}
	}}
}

// defaultOutboundNumber is the number a call this platform places comes from.
//
// The database allows at most one (uq_dids_default_outbound), so this is a
// lookup and not a choice. Refused rather than defaulted: a caller id nobody
// chose is worse than a call that does not go out, because the second one says
// so and the first one does not.
func (s *Service) defaultOutboundNumber(ctx context.Context) (string, error) {
	// Configuration still wins where a deployment has set it: the env var
	// predates the flag and removing it silently would change what a working
	// deployment presents.
	if s.cfg.CallerID != "" {
		return s.cfg.CallerID, nil
	}
	dids, err := s.dids.DIDs(ctx)
	if err != nil {
		return "", err
	}
	for _, did := range dids {
		if did.IsDefaultOutbound && did.IsEnabled {
			return did.Number, nil
		}
	}
	return "", ErrNoDefaultOutbound
}

func (s *Service) findDID(ctx context.Context, number string) (catalog.DID, error) {
	dids, err := s.dids.DIDs(ctx)
	if err != nil {
		return catalog.DID{}, err
	}
	for _, did := range dids {
		if did.Number == number && did.IsEnabled {
			if did.FlowID == nil {
				return catalog.DID{}, ErrFlowless
			}
			return did, nil
		}
	}
	return catalog.DID{}, ErrUnknownDID
}

// arm registers what to do once a channel has answered and its originate has
// let go of it.
func (s *Service) arm(channelID string, cont continuation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[channelID] = &pendingLeg{
		cont:      cont,
		expiresAt: s.now().Add(s.cfg.AnswerWindow),
	}
}

// armUntil is arm for a leg whose window began earlier than now.
func (s *Service) armUntil(channelID string, expiresAt time.Time, cont continuation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[channelID] = &pendingLeg{cont: cont, expiresAt: expiresAt}
}

// releaseLeg records the originate's +OK for a leg: the switch has let go of
// it. If the leg was already answered in time its continuation runs now;
// otherwise the answer will run it. No entry (never armed, already run,
// hung up) is nothing to do.
func (s *Service) releaseLeg(legID string) {
	s.mu.Lock()
	leg := s.pending[legID]
	if leg == nil {
		s.mu.Unlock()
		return
	}
	leg.isReleased = true
	if leg.answeredAt.IsZero() {
		s.mu.Unlock()
		return
	}
	delete(s.pending, legID)
	s.mu.Unlock()
	// The continuation talks to the switch; the dispatch loop must not wait
	// on that.
	go leg.cont.proceed()
}

// HandleSwitchEvent follows originated legs: an answer in time starts the
// armed continuation's clocks and, once the originate's +OK is in as well,
// runs it; a hangup (busy, declined, expired) discards it. Fed from the same
// dispatch loop as the coordinator.
func (s *Service) HandleSwitchEvent(ev telephony.SwitchEvent) {
	if ev.Kind == telephony.KindBackgroundJob {
		s.handleJob(ev)
		return
	}
	if ev.ChannelID == "" {
		return
	}
	s.settleBridge(ev)
	switch ev.Kind {
	case telephony.KindChannelCreate:
		s.noteChannel(ev.ChannelID)
		s.observeLine(ev)
	case telephony.KindChannelAnswer:
		s.mu.Lock()
		leg := s.pending[ev.ChannelID]
		if leg == nil {
			s.mu.Unlock()
			return
		}
		if !leg.answeredAt.IsZero() {
			s.mu.Unlock() // a repeated answer: the first one counts
			return
		}
		now := s.now()
		expired := now.After(leg.expiresAt)
		var fire bool
		if expired {
			delete(s.pending, ev.ChannelID)
		} else {
			leg.answeredAt = now
			if leg.isReleased {
				delete(s.pending, ev.ChannelID)
				fire = true
			}
		}
		s.noteAnswered(ev.ChannelID)
		s.mu.Unlock()
		if expired {
			// Never leave an answered leg parked: the person on it would
			// hear nothing. Hang it up with a distinct cause, which is what
			// the ledger's hangupCause then reads. For click-to-dial this is
			// the agent's auto-answered phone.
			s.log.Warn("answer arrived after the window; hanging up, not bridging",
				"channelId", ev.ChannelID, "cause", LateAnswerCause)
			go s.endLeg(telephony.LegEnding{
				ChannelID: ev.ChannelID,
				Cause:     LateAnswerCause,
				What:      "a late-answered leg",
			})
			return
		}
		if leg.cont.onAnswer != nil {
			leg.cont.onAnswer()
		}
		if fire {
			// The originate had already let go; the dispatch loop must not
			// wait on the switch.
			go leg.cont.proceed()
		}
	case telephony.KindChannelHangup:
		s.lines.Unobserve(ev.ChannelID)
		s.mu.Lock()
		delete(s.pending, ev.ChannelID)
		h := s.holds[ev.ChannelID]
		delete(s.holds, ev.ChannelID)
		s.mu.Unlock()
		// The customer is gone: stop waiting for a slot, and give back one
		// that was reserved for a bridge that no longer has anyone to carry.
		// A reservation the INVITE already consumed is untouched.
		if h != nil {
			h.cancel()
			s.cfg.Slots.Release(h.key)
		}
	}
}

// PendingCount reports how many originated legs still await their
// continuation: an answer, or the originate's result after one.
func (s *Service) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Expired entries are only garbage; sweeping here keeps the map honest
	// without a background goroutine. An entry answered in time is past its
	// window by definition and waits for the +OK instead; a hangup removes
	// it, and one whose +OK never comes goes once the job margin has passed
	// since the answer (the job's own backstop gives up at the same point).
	now := s.now()
	for id, leg := range s.pending {
		if leg.answeredAt.IsZero() {
			if now.After(leg.expiresAt) {
				delete(s.pending, id)
			}
		} else if now.Sub(leg.answeredAt) > s.margin() {
			delete(s.pending, id)
		}
	}
	return len(s.pending)
}
