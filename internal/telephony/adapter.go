// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Commander issues commands to FreeSWITCH. esl.Link satisfies it.
type Commander interface {
	API(cmd string) (string, error)
	BgAPI(cmd string) (string, error)
	IsUp() bool
}

// Adapter is the complete vocabulary of commands this application sends to
// FreeSWITCH. Nothing outside this file builds a switch command string, so the
// switch's spelling stays in one reviewable place.
type Adapter struct {
	cmd Commander
	// domain qualifies queue names and endpoint addresses. It must match the
	// switch's own $${domain}, because the Lua handler renders queue names
	// with it and mod_callcenter matches them literally.
	domain string
}

// NewAdapter builds an Adapter.
func NewAdapter(cmd Commander, domain string) *Adapter {
	return &Adapter{cmd: cmd, domain: domain}
}

// IsUp reports whether the switch link is usable.
func (a *Adapter) IsUp() bool { return a.cmd.IsUp() }

// QueueName renders a queue's switch-side name: the name itself.
//
// It used to append "@" and the domain. The switch appends nothing of its own —
// it stores and reports literally what it is told — so that suffix was ours,
// and in a single-tenant product it carried no information. What it did carry
// was the host's IP address, which moves: when this machine went from .176 to
// .55 every tier written under the old name became unreachable by the new one,
// and converge could neither see them nor delete them (C1). A name that cannot
// go stale is a better fix than a delete that copes with stale names.
func (a *Adapter) QueueName(name string) string { return name }

// Endpoint renders a registered extension's dial string.
func (a *Adapter) Endpoint(extensionNumber string) string {
	return "user/" + extensionNumber + "@" + a.domain
}

//
// mod_callcenter: agents and tiers are runtime state, managed by us rather
// than configured in a file, so an agent signing in is a switch command.
//

// AddCallcenterAgent registers an agent. The callback type makes the switch
// originate to the agent's contact when a call is offered.
//
// Registering an agent the switch already knows is the normal case on every
// sign-in after the first, so the switch saying so is success, not a fault.
func (a *Adapter) AddCallcenterAgent(name string) error {
	err := a.exec("callcenter_config agent add %s callback", name)
	if err != nil && strings.Contains(err.Error(), "Agent already exist") {
		return nil
	}
	return err
}

// DeleteCallcenterAgent removes an agent.
func (a *Adapter) DeleteCallcenterAgent(name string) error {
	return a.exec("callcenter_config agent del %s", name)
}

// SetCallcenterAgentContact points an agent at the phone they signed in on.
// Auto-answer is expressed as a channel variable on the originate, which is
// what a remote-controlled browser phone needs.
func (a *Adapter) SetCallcenterAgentContact(name, extensionNumber string, autoAnswer bool) error {
	contact := a.Endpoint(extensionNumber)
	if autoAnswer {
		contact = "{sip_auto_answer=true}" + contact
	}
	return a.exec("callcenter_config agent set contact %s '%s'", name, contact)
}

// SetCallcenterAgentStatus mirrors our presence onto the switch. Our states
// are richer than the switch's three, which is why the mapping lives in the
// agents package rather than here.
func (a *Adapter) SetCallcenterAgentStatus(name, status string) error {
	return a.exec("callcenter_config agent set status %s '%s'", name, status)
}

// SetCallcenterAgentRejectDelay sets how long the switch waits before offering
// a call to an agent who just rejected one.
func (a *Adapter) SetCallcenterAgentRejectDelay(name string, sec int) error {
	return a.exec("callcenter_config agent set reject_delay_time %s %d", name, sec)
}

// SetCallcenterAgentNoAnswerDelay sets the ring-no-answer timeout.
func (a *Adapter) SetCallcenterAgentNoAnswerDelay(name string, sec int) error {
	return a.exec("callcenter_config agent set no_answer_delay_time %s %d", name, sec)
}

// SetCallcenterAgentWrapUp sets the switch's own wrap-up timer. We always set
// it to zero: after-call work is ours, so that the reason an agent is
// unavailable stays visible in our own vocabulary.
func (a *Adapter) SetCallcenterAgentWrapUp(name string, sec int) error {
	return a.exec("callcenter_config agent set wrap_up_time %s %d", name, sec)
}

// AgentNotOnSwitchError reports a tier the switch cannot hold yet because it
// does not know the agent.
//
// Not a failure: mod_callcenter only knows agents who are signed in, so this is
// the ordinary answer for anyone staffed while signed out. Their staffing is
// applied when they sign in and the switch learns who they are. Distinguished
// from a real failure because otherwise every reconnect reports drift it did
// not find and could not have fixed.
//
// Carried as behaviour rather than a sentinel value so a caller in another
// package can recognise it without importing this one.
type AgentNotOnSwitchError struct{ Agent string }

func (e *AgentNotOnSwitchError) Error() string {
	return "the switch does not know agent " + e.Agent + " yet"
}

// AgentNotOnSwitch marks this as deferred rather than failed.
func (e *AgentNotOnSwitchError) AgentNotOnSwitch() bool { return true }

// AddCallcenterTier staffs an agent on a queue.
func (a *Adapter) AddCallcenterTier(queue, agent string, level, position int) error {
	err := a.exec("callcenter_config tier add %s %s %d %d", a.QueueName(queue), agent, level, position)
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "Agent not found"):
		return &AgentNotOnSwitchError{Agent: agent}
	case strings.Contains(err.Error(), "Tier already exist"):
		// Reconciliation is idempotent by design and runs on every
		// registration, so the switch agreeing already is success.
		return nil
	}
	return err
}

// DeleteCallcenterTier unstaffs an agent.
func (a *Adapter) DeleteCallcenterTier(queue, agent string) error {
	return a.exec("callcenter_config tier del %s %s", a.QueueName(queue), agent)
}

// ReloadQueue makes the switch re-read one queue's configuration, which the
// Lua handler renders from the database.
func (a *Adapter) ReloadQueue(name string) error {
	return a.exec("callcenter_config queue reload %s", a.QueueName(name))
}

// ListQueueMembers returns the switch's own view of who is waiting, used to
// reconcile after a reconnect.
func (a *Adapter) ListQueueMembers(name string) (string, error) {
	return a.cmd.API(fmt.Sprintf("callcenter_config queue list members %s", a.QueueName(name)))
}

//
// Call control.
//

// PhoneEvent tells a remote-controlled phone to answer or hold. Browser
// softphones need this rather than uuid_answer or uuid_hold: those report
// success while the browser does nothing.
func (a *Adapter) PhoneEvent(channelID string, event string) error {
	return a.exec("uuid_phone_event %s %s", channelID, event)
}

// Answer instructs the agent's phone to pick up.
func (a *Adapter) Answer(channelID string) error { return a.PhoneEvent(channelID, "talk") }

// Hold and Retrieve drive a remote-controlled phone's hold state.
func (a *Adapter) Hold(channelID string) error     { return a.PhoneEvent(channelID, "hold") }
func (a *Adapter) Retrieve(channelID string) error { return a.PhoneEvent(channelID, "talk") }

// MuteLeg and UnmuteLeg silence one leg's microphone at the switch.
//
// "read" is the direction the switch reads *from* the channel, i.e. what that
// party says; muting it leaves them still hearing the call. Muting in the
// browser instead would only work while the extension has focus and would be
// invisible to the rest of the platform, so the switch owns it.
//
// uuid_audio's stop takes no direction — it tears down the whole audio bug,
// which is exactly the undo of the start above.
func (a *Adapter) MuteLeg(channelID string) error {
	return a.exec("uuid_audio %s start read mute", channelID)
}

func (a *Adapter) UnmuteLeg(channelID string) error {
	return a.exec("uuid_audio %s stop", channelID)
}

// SendDTMF emits tones towards one leg's endpoint.
//
// Note this is uuid_send_dtmf, not uuid_recv_dtmf, and note which channel the
// caller passes: send emits *out* to that endpoint, so the leg to target is
// the far end — the one whose IVR should hear the digits. Aiming it at the
// agent's own leg would only beep in the agent's ear.
func (a *Adapter) SendDTMF(channelID, digits string) error {
	return a.exec("uuid_send_dtmf %s %s", channelID, digits)
}

// Hangup ends one leg with an explicit cause.
func (a *Adapter) Hangup(channelID, cause string) error {
	if cause == "" {
		cause = "NORMAL_CLEARING"
	}
	return a.exec("uuid_kill %s %s", channelID, cause)
}

// TransferToExtension moves a live caller to a dialplan extension. This is how
// a caller reaches a queue: the queue extension runs the queue script.
//
// The default context is aicc's own. Every extension this switch is asked to
// transfer to is one aicc defines, and the stock dialplan defines none of
// them: landing a transfer in `default` would route it by rules written for a
// different product.
func (a *Adapter) TransferToExtension(channelID, extension, context string) error {
	if context == "" {
		context = "aicc"
	}
	return a.exec("uuid_transfer %s %s XML %s", channelID, extension, context)
}

// BridgeToEndpoint connects a live caller to a new leg.
//
// It is deliberately a transfer into an inline bridge rather than an originate
// followed by uuid_bridge: bridging requires one leg to have media up, which
// is not true of a caller hearing ringback, and the failure is silent - the
// call simply sits there. The transfer form also clears the park state and
// lets us name the new leg's uuid before it exists.
//
// The m:^: prefix changes the inline action delimiter: the default is the
// comma, which would split the command inside the {var,var} block and hand
// bridge a truncated argument (found live — the call died with
// DESTINATION_OUT_OF_ORDER before the new leg ever routed).
func (a *Adapter) BridgeToEndpoint(channelID string, newPartyID uuid.UUID, endpoint string, vars map[string]string) error {
	all := map[string]string{"origination_uuid": newPartyID.String()}
	for k, v := range vars {
		all[k] = v
	}
	return a.exec("uuid_transfer %s 'm:^:bridge:{%s}%s' inline", channelID, renderVars(all), endpoint)
}

// Originate creates a new outbound leg parked and waiting, so call control
// stays with us rather than with the dialplan.
func (a *Adapter) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	all := map[string]string{
		"origination_uuid":   partyID.String(),
		"ignore_early_media": "true",
	}
	for k, v := range vars {
		all[k] = v
	}
	return a.cmd.BgAPI(fmt.Sprintf("originate {%s}%s &park()", renderVars(all), endpoint))
}

// StartRecording and StopRecording control a call's recording.
func (a *Adapter) StartRecording(channelID, path string) error {
	return a.exec("uuid_record %s start %s", channelID, path)
}

func (a *Adapter) StopRecording(channelID, path string) error {
	return a.exec("uuid_record %s stop %s", channelID, path)
}

// Eavesdrop lets a supervisor listen to, whisper into, or join a call. No
// conference module is involved, which matters because this build does not
// have one.
func (a *Adapter) Eavesdrop(supervisorPartyID uuid.UUID, supervisorExtension, targetChannelID, mode string) (string, error) {
	vars := map[string]string{
		"origination_uuid": supervisorPartyID.String(),
		"sip_auto_answer":  "true",
	}
	switch mode {
	case "WHISPER":
		vars["eavesdrop_whisper_bleg"] = "true"
	case "BARGE":
		vars["eavesdrop_bridge_aleg"] = "true"
		vars["eavesdrop_bridge_bleg"] = "true"
	}
	return a.cmd.BgAPI(fmt.Sprintf("originate {%s}%s &eavesdrop(%s)",
		renderVars(vars), a.Endpoint(supervisorExtension), targetChannelID))
}

// SetVariable sets one channel variable on a live call.
func (a *Adapter) SetVariable(channelID, name, value string) error {
	return a.exec("uuid_setvar %s %s %s", channelID, name, value)
}

// ShowChannels returns every live channel as JSON, the source of truth when
// reconciling after a restart or a reconnect.
func (a *Adapter) ShowChannels() (string, error) { return a.cmd.API("show channels as json") }

// exec runs a command and turns a switch-level error reply into a Go error.
func (a *Adapter) exec(format string, args ...any) error {
	cmd := fmt.Sprintf(format, args...)
	reply, err := a.cmd.API(cmd)
	slog.Debug("switch command", "command", cmd, "reply", strings.TrimSpace(reply), "error", err)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if strings.HasPrefix(strings.TrimSpace(reply), "-ERR") {
		return fmt.Errorf("%s: %s", cmd, strings.TrimSpace(reply))
	}
	return nil
}

// renderVars renders channel variables in the switch's braces syntax, sorted
// so a command is reproducible and testable.
func renderVars(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// Values are joined bare because this block rides in two different
		// grammars: a raw originate line, where a quoted value would be
		// legal, and the single-quoted inline transfer of BridgeToEndpoint,
		// where an embedded quote ends the outer quoting and the remainder
		// is executed as an inline application (both failure shapes found
		// live). Callers keep values free of spaces, commas and quotes.
		parts = append(parts, k+"="+vars[k])
	}
	return strings.Join(parts, ",")
}

//
// mod_audio_stream: the media tap that feeds live transcription.
//

// StartAudioStream taps a channel and streams it to a websocket.
//
// The rate is an integer and not a word. Only "8k" and "16k" have word forms;
// anything else goes through atoi and fails a modulo check, so "24k" is
// rejected with a bare -ERR whose real reason appears only in the switch log.
// Building the command from an int is what makes that unrepresentable.
//
// stereo, always: left is the tapped channel's read stream and right its write
// stream, which on an agent's own leg is the agent's microphone and the
// customer respectively.
func (a *Adapter) StartAudioStream(channelID, wsURL string, rateHz int, metadata string) error {
	if metadata != "" {
		return a.exec("uuid_audio_stream %s start %s stereo %d %s",
			channelID, wsURL, rateHz, metadata)
	}
	return a.exec("uuid_audio_stream %s start %s stereo %d", channelID, wsURL, rateHz)
}

// StopAudioStream detaches the tap.
//
// Optional in principle — the module tears its own bug down when the channel
// closes — but issued anyway where we end the stream before the call, such as
// an agent leaving a bridge.
func (a *Adapter) StopAudioStream(channelID string) error {
	return a.exec("uuid_audio_stream %s stop", channelID)
}

// PauseAudioStream and ResumeAudioStream bracket the periods that are not the
// conversation. On hold the agent's two channels carry a private side-call and
// music, neither of which belongs in a transcript of this one — and a stereo
// stream whose far side has fallen silent delivers nothing anyway, silently,
// so pausing is what makes that gap deliberate rather than mysterious.
func (a *Adapter) PauseAudioStream(channelID string) error {
	return a.exec("uuid_audio_stream %s pause", channelID)
}

func (a *Adapter) ResumeAudioStream(channelID string) error {
	return a.exec("uuid_audio_stream %s resume", channelID)
}
