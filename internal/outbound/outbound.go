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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// Switch is the slice of the adapter vocabulary this package uses.
type Switch interface {
	Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error)
	BridgeToEndpoint(channelID string, newPartyID uuid.UUID, endpoint string, vars map[string]string) error
	TransferToExtension(channelID, extension, context string) error
	Endpoint(extensionNumber string) string
}

// DIDSource answers which numbers exist and which flow serves them.
type DIDSource interface {
	DIDs(ctx context.Context) ([]catalog.DID, error)
}

// Config shapes how the outside world is dialed.
type Config struct {
	// EndpointFormat renders a destination number into a dial string for the
	// AI outbound leg, e.g. "sofia/gateway/pstn/%s". Only that path needs it:
	// click-to-dial hands the destination to the dialplan instead. The
	// default loops back into the local dialplan — pinned to XML, because a
	// loopback leg inherits the a-leg's dialplan and would otherwise read the
	// number as an application name — which is what a dev box without a trunk
	// can reach.
	EndpointFormat string
	// BotGateway is the gateway name the switch bridges AI legs through.
	BotGateway string
	// CallerID presented on click-to-dial customer legs; empty leaves the
	// carrier default.
	CallerID string
	// RatePerSec caps originates per second; the switch's own cap rejects
	// while this one delays. Default 100.
	RatePerSec int
	// AnswerWindow is how long an originated leg may ring before the armed
	// continuation is abandoned. Default 60s.
	AnswerWindow time.Duration
}

func (c Config) withDefaults() Config {
	if c.EndpointFormat == "" {
		c.EndpointFormat = "loopback/%s/default/XML"
	}
	if c.BotGateway == "" {
		c.BotGateway = "aicc_bot"
	}
	if c.RatePerSec <= 0 {
		c.RatePerSec = 100
	}
	if c.AnswerWindow <= 0 {
		c.AnswerWindow = 60 * time.Second
	}
	return c
}

// Service originates and follows up outbound legs.
type Service struct {
	cfg     Config
	sw      Switch
	dids    DIDSource
	hasCDR  func(ctx context.Context, callID uuid.UUID) (bool, error)
	isLive  func(callID uuid.UUID) bool
	limiter *Limiter
	log     *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingLeg // by channel id
}

// pendingLeg is a continuation armed on an originated channel: what to do
// the moment somebody answers it.
type pendingLeg struct {
	onAnswer  func()
	expiresAt time.Time
}

// New builds the service. hasCDR and isLive make retries idempotent: a call
// the ledger already closed, or one still running, is never dialed again.
func New(cfg Config, sw Switch, dids DIDSource,
	hasCDR func(context.Context, uuid.UUID) (bool, error),
	isLive func(uuid.UUID) bool, log *slog.Logger) *Service {

	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:     cfg,
		sw:      sw,
		dids:    dids,
		hasCDR:  hasCDR,
		isLive:  isLive,
		limiter: NewLimiter(cfg.RatePerSec),
		log:     log,
		pending: map[string]*pendingLeg{},
	}
}

// Errors the API layer translates.
var (
	ErrBadNumber     = fmt.Errorf("outbound: not a dialable number")
	ErrUnknownDID    = fmt.Errorf("outbound: no such DID")
	ErrFlowless      = fmt.Errorf("outbound: the DID has no published flow")
	ErrAlreadyPlaced = fmt.Errorf("outbound: this call was already placed")
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

// isDialable keeps dial strings boring: digits only, sane length. Everything
// else stays out of command lines by construction.
func isDialable(number string) bool {
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

// Dial is click-to-dial: ring the agent first, and only dial out once the
// agent leg is up. The agent clicked, so their leg auto-answers; the answered
// leg is then transferred into the dialplan at the destination, which routes
// it the same way a phone dialling that number would be routed — no second
// copy of the routing rules here, and no loopback legs to trace.
func (s *Service) Dial(ctx context.Context, agentExtension, destination string) (uuid.UUID, error) {
	if !isDialable(destination) || !isDialable(agentExtension) {
		return uuid.Nil, ErrBadNumber
	}
	if err := s.limiter.Take(ctx); err != nil {
		return uuid.Nil, err
	}

	callID := uuid.Must(uuid.NewV7())
	agentLeg := uuid.New()

	vars := map[string]string{
		"aicc_call_id":                 callID.String(),
		"sip_auto_answer":              "true",
		"origination_caller_id_number": destination,
		// No space in the name: it travels inside an originate {…} block,
		// where a space ends the block and kills the call before it routes.
		"origination_caller_id_name": "Dial-" + destination,
	}
	if s.cfg.CallerID != "" {
		// Presented onward when the dialplan bridges out; the agent's own
		// display above is the origination_* pair.
		vars["effective_caller_id_number"] = s.cfg.CallerID
	}
	if _, err := s.sw.Originate(agentLeg, s.sw.Endpoint(agentExtension), vars); err != nil {
		return uuid.Nil, err
	}

	s.arm(agentLeg.String(), func() {
		if err := s.sw.TransferToExtension(agentLeg.String(), destination, "default"); err != nil {
			s.log.Error("click-to-dial transfer failed",
				"callId", callID, "destination", destination, "error", err)
		}
	})

	s.log.Info("click-to-dial placed", "callId", callID,
		"agentExtension", agentExtension, "destination", destination)
	return callID, nil
}

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
}

// DialAI is the inbound AI path reversed: originate towards the customer,
// park; when they answer, transfer their leg into a bridge to the bot
// gateway with the same correlation headers an inbound call carries.
func (s *Service) DialAI(ctx context.Context, req AIDialRequest) (uuid.UUID, error) {
	if !isDialable(req.To) {
		return uuid.Nil, ErrBadNumber
	}

	did, err := s.findDID(ctx, req.DIDNumber)
	if err != nil {
		return uuid.Nil, err
	}
	language := req.Language
	if language == "" {
		language = did.Language
	}

	callID := req.CallID
	if callID == uuid.Nil {
		callID = uuid.Must(uuid.NewV7())
	} else {
		// The ledger is the idempotency record: a finished call stays
		// finished, a running call keeps running, and neither redials.
		if done, err := s.hasCDR(ctx, callID); err != nil {
			return uuid.Nil, err
		} else if done {
			return callID, ErrAlreadyPlaced
		}
		if s.isLive != nil && s.isLive(callID) {
			return callID, ErrAlreadyPlaced
		}
	}

	if err := s.limiter.Take(ctx); err != nil {
		return uuid.Nil, err
	}

	customerLeg := uuid.New()
	vars := map[string]string{
		"aicc_call_id":                 callID.String(),
		"aicc_language":                language,
		"origination_caller_id_number": did.Number,
	}
	endpoint := fmt.Sprintf(s.cfg.EndpointFormat, req.To)
	pinCodecs(vars, endpoint)
	if _, err := s.sw.Originate(customerLeg, endpoint, vars); err != nil {
		return uuid.Nil, err
	}

	s.arm(customerLeg.String(), func() {
		botLeg := uuid.New()
		bridgeVars := map[string]string{
			"aicc_call_id":            callID.String(),
			"aicc_did":                did.Number,
			"aicc_language":           language,
			"absolute_codec_string":   "PCMU,PCMA",
			"sip_h_X-AICC-Call-ID":    callID.String(),
			"sip_h_X-AICC-Channel-ID": customerLeg.String(),
			"sip_h_X-AICC-DID":        did.Number,
			"sip_h_X-AICC-Language":   language,
			"sip_h_X-AICC-ANI":        req.To,
			"sip_h_X-AICC-Call-Type":  "OUTBOUND",
		}
		target := "sofia/gateway/" + s.cfg.BotGateway + "/" + did.Number
		pinCodecs(bridgeVars, target)
		if err := s.sw.BridgeToEndpoint(customerLeg.String(), botLeg, target, bridgeVars); err != nil {
			s.log.Error("ai outbound bridge to bot failed",
				"callId", callID, "to", req.To, "error", err)
		}
	})

	s.log.Info("ai outbound placed", "callId", callID,
		"to", req.To, "did", did.Number, "language", language)
	return callID, nil
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

// arm registers what to do when a channel answers.
func (s *Service) arm(channelID string, onAnswer func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[channelID] = &pendingLeg{
		onAnswer:  onAnswer,
		expiresAt: time.Now().Add(s.cfg.AnswerWindow),
	}
}

// HandleSwitchEvent follows originated legs: an answer fires the armed
// continuation, a hangup (busy, declined, expired) discards it. Fed from the
// same dispatch loop as the coordinator.
func (s *Service) HandleSwitchEvent(ev telephony.SwitchEvent) {
	if ev.ChannelID == "" {
		return
	}
	switch ev.Kind {
	case telephony.KindChannelAnswer:
		s.mu.Lock()
		leg := s.pending[ev.ChannelID]
		delete(s.pending, ev.ChannelID)
		expired := leg != nil && time.Now().After(leg.expiresAt)
		s.mu.Unlock()
		if leg == nil {
			return
		}
		if expired {
			s.log.Warn("answer arrived after the window; not bridging", "channelId", ev.ChannelID)
			return
		}
		// The continuation talks to the switch; the dispatch loop must not
		// wait on that.
		go leg.onAnswer()
	case telephony.KindChannelHangup:
		s.mu.Lock()
		delete(s.pending, ev.ChannelID)
		s.mu.Unlock()
	}
}

// PendingCount reports how many originated legs still await an answer.
func (s *Service) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Expired entries are only garbage; sweeping here keeps the map honest
	// without a background goroutine.
	now := time.Now()
	for id, leg := range s.pending {
		if now.After(leg.expiresAt) {
			delete(s.pending, id)
		}
	}
	return len(s.pending)
}
