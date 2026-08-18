// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// SwitchTap is the slice of the switch adapter this package needs. Command
// strings are built in the adapter and nowhere else, so this is an interface
// over four of them rather than a place that composes a fifth.
type SwitchTap interface {
	StartAudioStream(channelID, wsURL string, rateHz int, metadata string) error
	StopAudioStream(channelID string) error
	PauseAudioStream(channelID string) error
	ResumeAudioStream(channelID string) error
	IsUp() bool
}

// tapKey identifies one attachment rather than one channel: the call it
// belongs to, the channel it runs on, and which attempt it is.
//
// A bare channel id is not an identity. It is the *place* a tap lives, and the
// same place is occupied by different taps over a call's life — a leg whose
// call id changes under it when two calls turn out to be one conversation, a
// re-attach after a stream that never connected. Keyed by channel alone, a
// late continuation from the first attachment retires the second, and the
// stop command lands on a stream somebody else owns.
//
// callID + channel is the same composite the ingest already keys its sessions
// and expectations on (Server.sessions, Server.pending); epoch distinguishes
// two attachments of the same pair, which is the case those two never see
// because the switch dials back only once per attach.
type tapKey struct {
	callID  uuid.UUID
	channel string
	epoch   uint64
}

// ErrSwitchDown reports that the tap could not be commanded because the ESL
// link is not up. Distinct from telephony.ErrNoTap: there is a tap, and we
// could not reach it.
var ErrSwitchDown = errors.New("streamin: the switch link is down")

// Tap starts and stops the media tap. It implements telephony.Tapper, which is
// how the coordinator drives transcription without knowing anything about it.
type Tap struct {
	srv       *Server
	sw        SwitchTap
	publicURL string
	rateHz    int
	ttl       time.Duration
	log       Logger

	mu    sync.Mutex
	epoch uint64
	// active holds every live attachment, so a call can retire all of its own
	// without knowing which channels they were on.
	active map[tapKey]struct{}
	// current is the channel's live attachment, which is what a switch event
	// naming only a channel resolves through.
	current map[string]tapKey
}

// NewTap builds the coordinator's handle on the ingest.
//
// publicURL is what the *switch* dials, which is not necessarily what we bind:
// in a container the two differ, exactly as the ESL address already does. The
// scheme comes from configuration and is never composed here, so a deployment
// that needs wss:// is a config change rather than a patch.
func NewTap(srv *Server, sw SwitchTap, publicURL string, rateHz int, ttl time.Duration, log Logger) *Tap {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	t := &Tap{
		srv: srv, sw: sw, publicURL: publicURL, rateHz: rateHz,
		ttl: ttl, log: log,
		active:  map[tapKey]struct{}{},
		current: map[string]tapKey{},
	}
	if srv != nil {
		srv.AttachStreamEndHandler(t.streamEnded)
	}
	return t
}

// streamEnded drops an attachment the module has already finished with.
//
// It stops tracking; it does not command anything. The stream is over either
// way — what this prevents is the *next* Detach, on the hangup that follows,
// sending a stop for a channel FreeSWITCH has already destroyed. The module
// logs that at ERR, once for every tapped call, and a log where teardown is
// always an error is a log where a real error is invisible.
//
// Keyed on call and channel together, so a stream ending cannot retire an
// attachment that belongs to a later call on the same channel.
func (t *Tap) streamEnded(callID uuid.UUID, channelID string) {
	if t == nil {
		return
	}
	key, ok := t.currentKey(channelID)
	if !ok || key.callID != callID {
		return
	}
	t.forget(key)
}

// Attach taps one agent leg.
//
// A channel already tapped for this call is left alone: one bug per channel,
// which the module enforces too. A channel tapped for a *different* call has
// had its conversation renamed under it — two calls turned out to be one — and
// the old stream is reporting into a transcript that is no longer this
// conversation's, so it is stopped before the new one starts.
func (t *Tap) Attach(callID uuid.UUID, agentID, partyID *uuid.UUID, channelID, language string) {
	if t == nil || t.sw == nil || !t.sw.IsUp() || t.publicURL == "" {
		return
	}
	// The channel's attachment is replaced under one lock acquisition, so two
	// attaches racing for the same channel have exactly one winner in the
	// index. The loser's stream is still stopped below — it is just no longer
	// the thing the index points at, which is why forget refuses to clear an
	// index entry that has moved on.
	t.mu.Lock()
	prev, replaced := t.current[channelID]
	if replaced && prev.callID == callID {
		t.mu.Unlock()
		return
	}
	t.epoch++
	key := tapKey{callID: callID, channel: channelID, epoch: t.epoch}
	t.active[key] = struct{}{}
	t.current[channelID] = key
	t.mu.Unlock()

	if replaced {
		t.log.Info("transcription tap follows the call its leg was folded into",
			"channelId", channelID, "from", prev.callID, "to", callID)
		_ = t.detach(prev)
	}

	claim := Claim{
		CallID:   callID,
		Channel:  channelID,
		Language: language,
		Expires:  time.Now().Add(t.ttl),
	}
	if agentID != nil {
		claim.AgentID = *agentID
	}
	if partyID != nil {
		claim.PartyID = *partyID
	}

	dialURL, err := withToken(t.publicURL, t.srv.Token(claim))
	if err != nil {
		t.forget(key)
		t.log.Error("transcription tap has an unusable stream url",
			"url", t.publicURL, "error", err)
		return
	}

	if err := t.sw.StartAudioStream(channelID, dialURL, t.rateHz, ""); err != nil {
		t.forget(key)
		t.log.Warn("transcription tap could not attach",
			"callId", callID, "channelId", channelID, "error", err)
		return
	}
	// +OK means the switch accepted the command, not that the socket came up:
	// the connect is asynchronous, and an attach to a port with nothing
	// listening succeeds just as loudly. So the ingest is told to expect this
	// stream, and to say so if it never arrives.
	t.srv.Expect(claim)
	t.log.Info("transcription tap attached",
		"callId", callID, "channelId", channelID, "rateHz", t.rateHz)
}

// Detach stops the tap on a channel the switch has just told us about.
// Harmless when there is none: a hangup arrives for every leg of every call
// and most of them were never tapped.
func (t *Tap) Detach(channelID string) {
	if t == nil {
		return
	}
	key, ok := t.currentKey(channelID)
	if !ok {
		return
	}
	_ = t.detach(key)
}

// DetachCall stops every tap this call has, whatever the switch did or did not
// say about the individual legs.
//
// This is the convergence guarantee. Detach is driven by CHANNEL_UNBRIDGE and
// CHANNEL_HANGUP, and neither is owed to us: a leg transferred away, an ESL
// link that reconnected across the hangup, an event this application filtered
// before the tap ever saw it. Any of those leaves the module pumping audio at
// an ingest whose transcript actor has been closed, for the life of the
// process. So the call's own termination path drives this unconditionally, and
// it is idempotent because it will usually run second.
func (t *Tap) DetachCall(callID uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	keys := make([]tapKey, 0, 2)
	for key := range t.active {
		if key.callID == callID {
			keys = append(keys, key)
		}
	}
	t.mu.Unlock()
	for _, key := range keys {
		_ = t.detach(key)
	}
}

// Pause and Resume bracket hold, which is a private side-call and music,
// neither of which is this conversation.
//
// They report rather than shrug. A hold on a channel with no live tap is
// ordinary — the caller's own leg is never tapped — but it is the *caller's*
// business to decide that, and silently doing nothing makes a tap that died
// under us indistinguishable from one that was never there.
func (t *Tap) Pause(channelID string) error {
	return t.command(channelID, SwitchTap.PauseAudioStream)
}

func (t *Tap) Resume(channelID string) error {
	return t.command(channelID, SwitchTap.ResumeAudioStream)
}

// command is written over a method expression rather than a bound method so
// that a nil Tap or a nil switch is a returned error and not a panic in the
// argument list.
func (t *Tap) command(channelID string, send func(SwitchTap, string) error) error {
	if t == nil {
		return telephony.ErrNoTap
	}
	key, ok := t.currentKey(channelID)
	if !ok {
		return telephony.ErrNoTap
	}
	if t.sw == nil || !t.sw.IsUp() {
		return ErrSwitchDown
	}
	return send(t.sw, key.channel)
}

// detach stops one attachment, and only if it is still the live one.
func (t *Tap) detach(key tapKey) error {
	if !t.forget(key) {
		return telephony.ErrNoTap
	}
	if t.sw == nil || !t.sw.IsUp() {
		return ErrSwitchDown
	}
	if err := t.sw.StopAudioStream(key.channel); err != nil {
		// Expected whenever the channel ended first, which is most of the
		// time, so this is not a warning.
		t.log.Debug("transcription tap was already gone",
			"channelId", key.channel, "error", err)
	}
	return nil
}

func (t *Tap) currentKey(channelID string) (tapKey, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key, ok := t.current[channelID]
	return key, ok
}

// forget drops one attachment and reports whether it was still live. The
// channel index is cleared only when it still points at this attachment: a
// continuation from an attachment that has already been replaced must not
// evict its successor.
func (t *Tap) forget(key tapKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, live := t.active[key]; !live {
		return false
	}
	delete(t.active, key)
	if cur, ok := t.current[key.channel]; ok && cur == key {
		delete(t.current, key.channel)
	}
	return true
}

// withToken appends the credential to the stream URL, preserving whatever the
// deployment already put there.
func withToken(base, token string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", &url.Error{Op: "parse", URL: base,
			Err: errScheme}
	}
	q := u.Query()
	q.Set("t", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var errScheme = &schemeError{}

type schemeError struct{}

func (*schemeError) Error() string {
	return "stream url must be ws:// or wss://"
}

// StreamPath is the path the ingest serves, so a deployment that configures a
// bare host still reaches it.
const StreamPath = "/stream"

// NormalizePublicURL fills in the path when a deployment gives only a host.
func NormalizePublicURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.TrimSuffix(u.Path, "/") == "" {
		u.Path = StreamPath
	}
	return u.String()
}
