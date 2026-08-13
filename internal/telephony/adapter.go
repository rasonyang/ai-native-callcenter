// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
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

// QueueName renders a queue's switch-side name. The Lua configuration handler
// uses the same form, so the two never disagree.
func (a *Adapter) QueueName(name string) string { return name + "@" + a.domain }

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
func (a *Adapter) AddCallcenterAgent(name string) error {
	return a.exec("callcenter_config agent add %s callback", name)
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

// AddCallcenterTier staffs an agent on a queue.
func (a *Adapter) AddCallcenterTier(queue, agent string, level, position int) error {
	return a.exec("callcenter_config tier add %s %s %d %d", a.QueueName(queue), agent, level, position)
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

// Hangup ends one leg with an explicit cause.
func (a *Adapter) Hangup(channelID, cause string) error {
	if cause == "" {
		cause = "NORMAL_CLEARING"
	}
	return a.exec("uuid_kill %s %s", channelID, cause)
}

// TransferToExtension moves a live caller to a dialplan extension. This is how
// a caller reaches a queue: the queue extension runs the queue script.
func (a *Adapter) TransferToExtension(channelID, extension, context string) error {
	if context == "" {
		context = "default"
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
func (a *Adapter) BridgeToEndpoint(channelID string, newPartyID uuid.UUID, endpoint string, vars map[string]string) error {
	all := map[string]string{"origination_uuid": newPartyID.String()}
	for k, v := range vars {
		all[k] = v
	}
	return a.exec("uuid_transfer %s 'bridge:{%s}%s' inline", channelID, renderVars(all), endpoint)
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
		parts = append(parts, k+"="+vars[k])
	}
	return strings.Join(parts, ",")
}
