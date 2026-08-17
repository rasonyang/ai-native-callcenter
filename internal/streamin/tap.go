// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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

// Tap starts and stops the media tap. It implements telephony.Tapper, which is
// how the coordinator drives transcription without knowing anything about it.
type Tap struct {
	srv       *Server
	sw        SwitchTap
	publicURL string
	rateHz    int
	ttl       time.Duration
	log       Logger

	mu     sync.Mutex
	active map[string]bool
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
	return &Tap{
		srv: srv, sw: sw, publicURL: publicURL, rateHz: rateHz,
		ttl: ttl, log: log, active: map[string]bool{},
	}
}

// Attach taps one agent leg.
func (t *Tap) Attach(callID uuid.UUID, agentID, partyID *uuid.UUID, channelID string) {
	if t == nil || t.sw == nil || !t.sw.IsUp() || t.publicURL == "" {
		return
	}
	t.mu.Lock()
	if t.active[channelID] {
		t.mu.Unlock()
		return // one bug per channel; the module enforces it too
	}
	t.active[channelID] = true
	t.mu.Unlock()

	claim := Claim{
		CallID:  callID,
		Channel: channelID,
		Expires: time.Now().Add(t.ttl),
	}
	if agentID != nil {
		claim.AgentID = *agentID
	}
	if partyID != nil {
		claim.PartyID = *partyID
	}

	dialURL, err := withToken(t.publicURL, t.srv.Token(claim))
	if err != nil {
		t.forget(channelID)
		t.log.Error("transcription tap has an unusable stream url",
			"url", t.publicURL, "error", err)
		return
	}

	if err := t.sw.StartAudioStream(channelID, dialURL, t.rateHz, ""); err != nil {
		t.forget(channelID)
		t.log.Warn("transcription tap could not attach",
			"callId", callID, "channelId", channelID, "error", err)
		return
	}
	t.log.Info("transcription tap attached",
		"callId", callID, "channelId", channelID, "rateHz", t.rateHz)
}

// Detach stops a tap. Harmless when there is none: a channel that closes takes
// its own bug with it, and this is the case where we end the stream first.
func (t *Tap) Detach(channelID string) {
	if t == nil || !t.was(channelID) {
		return
	}
	t.forget(channelID)
	if t.sw == nil || !t.sw.IsUp() {
		return
	}
	if err := t.sw.StopAudioStream(channelID); err != nil {
		// Expected whenever the channel ended first, which is most of the
		// time, so this is not a warning.
		t.log.Debug("transcription tap was already gone",
			"channelId", channelID, "error", err)
	}
}

func (t *Tap) Pause(channelID string) {
	if t == nil || !t.was(channelID) || t.sw == nil || !t.sw.IsUp() {
		return
	}
	if err := t.sw.PauseAudioStream(channelID); err != nil {
		t.log.Debug("transcription tap could not pause", "channelId", channelID, "error", err)
	}
}

func (t *Tap) Resume(channelID string) {
	if t == nil || !t.was(channelID) || t.sw == nil || !t.sw.IsUp() {
		return
	}
	if err := t.sw.ResumeAudioStream(channelID); err != nil {
		t.log.Debug("transcription tap could not resume", "channelId", channelID, "error", err)
	}
}

func (t *Tap) was(channelID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[channelID]
}

func (t *Tap) forget(channelID string) {
	t.mu.Lock()
	delete(t.active, channelID)
	t.mu.Unlock()
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
