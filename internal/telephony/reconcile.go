// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// Channels that predate this process.
//
// Calls do not end when the application restarts, and an ESL link that drops
// and comes back has missed whatever happened in between. Live events say
// nothing about either, so on every connect the switch is asked what it holds
// and the registry adopts it (docs/design/01-telephony.md section 6).
//
// `show channels as json` lists channels but carries no channel variables, and
// the variables are the point: aicc_call_id says whose channel it is, and the
// rest say what it was for. So each listed channel is read once with
// `uuid_dump <uuid> json`, which returns the same headers and variable_*
// entries a channel event carries, and the dump is fed through the ordinary
// normalizer. One path from the switch's words to domain events, not two.

// The channel variables the originator stamps on a leg it parks, so that a
// process that did not place it can still tell what the leg is for.
const (
	// VarPlacedBy says which operation originated the leg.
	VarPlacedBy = "aicc_placed_by"
	// VarPlacedTo carries the number an AI outbound leg dialled, exactly as
	// the request gave it. The destination number the switch reports back may
	// have been rewritten by the dial string; this is what the bot is told.
	VarPlacedTo = "aicc_placed_to"

	// PlacedByDialAI marks the customer leg of an AI outbound call.
	PlacedByDialAI = "DIAL_AI"
	// PlacedByDial marks the agent leg of a click-to-dial.
	PlacedByDial = "DIAL"
)

// LiveChannel is what the switch holds for one channel of ours, in domain
// words. The channel variable names stop at this boundary.
type LiveChannel struct {
	ChannelID string
	// CallID is the identity the dialplan or the originator minted; never
	// Nil in what AdoptLiveChannels returns.
	CallID    uuid.UUID
	CreatedAt time.Time
	// IsInbound is the switch's direction: the leg arrived rather than being
	// created by an originate.
	IsInbound  bool
	IsAnswered bool
	// IsParked reports a channel sitting in park, which is where an
	// originated leg waits for a decision from this application.
	IsParked bool
	// PeerChannelID is the leg this one is bridged to, empty when unbridged.
	PeerChannelID string
	// IsBotLeg marks the leg the switch dialled towards the bot gateway.
	IsBotLeg bool
	// PeerIsBotLeg reports that the leg this one is bridged to is the bot
	// gateway's.
	PeerIsBotLeg bool
	// Age is how long the channel has existed, and BridgeAge how long it has
	// been bridged (or, failing the bridge time, how long its peer has been
	// answered). Both are measured on the switch's clock: the dump's own
	// timestamp minus the channel's own, never this machine's now. The Is...
	// flags say whether the switch gave enough to compute them; when it did
	// not, nothing may be decided from the figure.
	Age              time.Duration
	IsAgeKnown       bool
	BridgeAge        time.Duration
	IsBridgeAgeKnown bool
	// BridgedAt is when the bridge to the peer started, zero when unknown.
	BridgedAt time.Time

	Language string
	DID      string
	// PlacedBy is PlacedByDialAI or PlacedByDial for a leg this application
	// originated and stamped; empty for everything else, including legs
	// originated by a build that did not stamp them.
	PlacedBy string
	// PlacedTo is the number an AI outbound leg dialled; Destination the
	// number a click-to-dial agent leg is to be sent on to.
	PlacedTo    string
	Destination string
}

// ErrChannelGone reports that the switch no longer has the channel asked about.
var ErrChannelGone = errors.New("telephony: no such channel")

// DumpChannel returns one channel's headers and variables as JSON.
func (a *Adapter) DumpChannel(channelID string) (string, error) {
	out, err := a.cmd.API("uuid_dump " + channelID + " json")
	if err != nil {
		return "", fmt.Errorf("uuid_dump %s: %w", channelID, err)
	}
	if reply := strings.TrimSpace(out); strings.HasPrefix(reply, "-ERR") {
		if strings.Contains(reply, "No such channel") {
			return "", ErrChannelGone
		}
		return "", fmt.Errorf("uuid_dump %s: %s", channelID, reply)
	}
	return out, nil
}

// listedChannel is one row of `show channels as json` that survived
// validation.
type listedChannel struct {
	id           uuid.UUID
	callState    string
	application  string
	createdEpoch int64
}

// knownCallStates are the states `show channels` reports in its callstate
// column. A row naming any other is a row this code does not understand, and
// acting on a channel it does not understand is worse than leaving it alone.
var knownCallStates = []string{
	"DOWN", "DIALING", "RINGING", "EARLY", "ACTIVE", "HELD", "UNHELD", "RING_WAIT", "HANGUP",
}

// parseChannelList reads the listing, tolerating damage. A row that cannot be
// used is skipped with a warning; a payload cut off mid-way yields the rows
// before the cut. Nothing here aborts the pass.
//
// listed holds the uuid of every row the switch showed, usable or not, and
// isComplete says the whole payload was read: only a complete listing can
// prove that a channel is gone.
func parseChannelList(payload string, log *slog.Logger) (rows []listedChannel, listed map[string]bool, isComplete bool) {
	dec := json.NewDecoder(strings.NewReader(payload))
	listed = map[string]bool{}

	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		log.Warn("channel listing is not a JSON object; nothing adopted", "error", err)
		return nil, listed, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			log.Warn("channel listing is cut off", "error", err)
			return rows, listed, false
		}
		key, _ := keyTok.(string)
		if key != "rows" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				log.Warn("channel listing is cut off", "error", err)
				return rows, listed, false
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			log.Warn("channel listing has no row array", "error", err)
			return rows, listed, false
		}
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				log.Warn("channel listing is cut off", "error", err, "rowsRead", len(rows))
				return rows, listed, false
			}
			var named struct {
				UUID string `json:"uuid"`
			}
			if json.Unmarshal(raw, &named) == nil {
				if id, err := uuid.Parse(named.UUID); err == nil {
					listed[id.String()] = true
				}
			}
			if row, ok := parseChannelRow(raw, log); ok {
				rows = append(rows, row)
			}
		}
		if _, err := dec.Token(); err != nil { // the closing bracket
			log.Warn("channel listing is cut off", "error", err)
			return rows, listed, false
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		log.Warn("channel listing is cut off", "error", err)
		return rows, listed, false
	}
	return rows, listed, true
}

func parseChannelRow(raw json.RawMessage, log *slog.Logger) (listedChannel, bool) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		log.Warn("skipping a channel row that is not an object", "error", err)
		return listedChannel{}, false
	}
	text := func(name string) string {
		switch v := fields[name].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
		return ""
	}
	id, err := uuid.Parse(text("uuid"))
	if err != nil {
		log.Warn("skipping a channel row with no usable uuid", "uuid", text("uuid"))
		return listedChannel{}, false
	}
	state := strings.ToUpper(text("callstate"))
	if !slices.Contains(knownCallStates, state) {
		log.Warn("skipping a channel row in a state this code does not know",
			"channelId", id, "callstate", text("callstate"))
		return listedChannel{}, false
	}
	epoch, _ := strconv.ParseInt(text("created_epoch"), 10, 64)
	return listedChannel{
		id: id, callState: state, application: text("application"), createdEpoch: epoch,
	}, true
}

// dumped is a channel read in full.
type dumped struct {
	row     listedChannel
	headers map[string]string
}

func (d dumped) get(name string) string {
	if v, ok := d.headers[name]; ok {
		return v
	}
	return ""
}

func (d dumped) variable(name string) string { return d.get("variable_" + name) }

func (d dumped) micros(name string) time.Time {
	us, err := strconv.ParseInt(d.get(name), 10, 64)
	if err != nil || us <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

func (d dumped) createdAt() time.Time {
	if t := d.micros("Caller-Channel-Created-Time"); !t.IsZero() {
		return t
	}
	if d.row.createdEpoch > 0 {
		return time.Unix(d.row.createdEpoch, 0).UTC()
	}
	return time.Time{}
}

// switchNow is the switch's clock when the dump was made.
func (d dumped) switchNow() time.Time { return d.micros("Event-Date-Timestamp") }

// bridgedTo is the leg this one is really bridged to. A pointer to a peer is
// believed only when that peer is listed and points back: a stale pointer on a
// leg whose bridge has since ended must not read as a bridge.
func (d dumped) bridgedTo(byID map[string]dumped) string {
	id := d.peer()
	if id == "" {
		return ""
	}
	other, ok := byID[id]
	if !ok || other.peer() != d.get("Unique-ID") {
		return ""
	}
	return id
}

func (d dumped) peer() string {
	for _, v := range []string{
		d.get("Other-Leg-Unique-ID"), d.variable("signal_bond"), d.variable("bridge_uuid"),
	} {
		if v != "" && v != d.get("Unique-ID") {
			return v
		}
	}
	return ""
}

func (d dumped) isAnswered() bool {
	if state := d.get("Answer-State"); state != "" {
		return strings.EqualFold(state, "answered")
	}
	switch d.row.callState {
	case "ACTIVE", "HELD", "UNHELD":
		return true
	}
	return false
}

func (d dumped) isParked() bool {
	return d.row.application == "park" || d.variable("current_application") == "park"
}

// event builds the channel event a live switch would have sent at this moment
// in the channel's life.
func (d dumped) event(name string, at time.Time, extra map[string]string) *esl.Event {
	h := make(map[string]string, len(d.headers)+len(extra)+2)
	for k, v := range d.headers {
		h[k] = v
	}
	h["Event-Name"] = name
	if !at.IsZero() {
		h["Event-Date-Timestamp"] = strconv.FormatInt(at.UnixMicro(), 10)
	}
	for k, v := range extra {
		h[k] = v
	}
	return esl.NewEvent(h, "")
}

// AdoptLiveChannels asks the switch what it holds and brings the registry up
// to date with it, returning the channels of ours it found.
//
// A channel the registry already owns is left exactly as it is: the live
// process holds it, and replaying its history would publish states that did
// not change. A channel it does not own is adopted with the facts the switch
// reports (creation, answer and bridge times included), so a call that
// outlived a restart keeps its true timings, and is announced for the first
// time rather than again. Channels that carry no aicc_call_id and no pointer
// to a call of ours belong to somebody else and are not touched; neither are
// supervisors' monitoring legs or scripted-test scaffolding.
//
// An error means the listing could not be had at all. Individual channels
// that cannot be read are logged and skipped.
func (c *Coordinator) AdoptLiveChannels(ctx context.Context) ([]LiveChannel, error) {
	// Anything the registry learned of after this instant may be a channel the
	// listing could not have shown.
	snapshotStart := time.Now()
	payload, err := c.adapter.ShowChannels()
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	if reply := strings.TrimSpace(payload); strings.HasPrefix(reply, "-ERR") {
		return nil, fmt.Errorf("list channels: %s", reply)
	}
	log := slog.Default()
	rows, listed, isComplete := parseChannelList(payload, log)

	var all []dumped
	for _, row := range rows {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// DOWN is not dead: an originated leg sits in DOWN from creation
		// until the far end first answers with progress, and that is the
		// state a leg placed a moment before a reconnect is listed in
		// (live 2026-10-12: a ringing AI customer leg was skipped, unlogged).
		// Only a channel already hanging up is left out.
		if row.callState == "HANGUP" {
			continue
		}
		out, err := c.adapter.DumpChannel(row.id.String())
		if errors.Is(err, ErrChannelGone) {
			log.Info("skipping a listed channel that is gone by the time it is read",
				"channelId", row.id, "callstate", row.callState)
			continue
		}
		if err != nil {
			log.Warn("skipping a channel that cannot be read", "channelId", row.id, "error", err)
			continue
		}
		var headers map[string]string
		if err := decodeStringMap(out, &headers); err != nil {
			log.Warn("skipping a channel whose dump is unreadable", "channelId", row.id, "error", err)
			continue
		}
		if got := headers["Unique-ID"]; got != "" && got != row.id.String() {
			log.Warn("skipping a channel whose dump is of another channel",
				"channelId", row.id, "dumpedId", got)
			continue
		}
		headers["Unique-ID"] = row.id.String()
		all = append(all, dumped{row: row, headers: headers})
	}

	byID := make(map[string]dumped, len(all))
	for _, d := range all {
		byID[d.get("Unique-ID")] = d
	}

	var (
		found   []LiveChannel
		adopted []dumped
	)
	for _, d := range all {
		id := d.get("Unique-ID")
		raw := d.variable("aicc_call_id")
		callID, parseErr := uuid.Parse(raw)
		hasPointer := d.variable("cc_member_session_uuid") != "" || d.variable("aicc_parent_channel") != ""
		switch {
		case d.variable(ObserverVar) != "":
			continue
		case d.variable("aicc_harness") == "true":
			continue
		case raw != "" && parseErr != nil:
			log.Warn("skipping a channel whose call id is not a uuid", "channelId", id, "aiccCallId", raw)
			continue
		case raw == "" && !hasPointer:
			continue
		}
		if !c.registry.Owns(id) {
			adopted = append(adopted, d)
		}
		if raw == "" {
			continue // adopted by its pointer, but nothing to decide about it
		}
		found = append(found, liveChannelOf(d, callID, byID))
	}

	// Creation, then answer, for every channel; a bridge needs both of its
	// legs on the books, so those come last.
	for _, d := range adopted {
		c.replay(ctx, d.event("CHANNEL_CREATE", d.createdAt(), nil))
		if d.isAnswered() {
			at := d.micros("Caller-Channel-Answered-Time")
			if at.IsZero() {
				at = d.createdAt()
			}
			c.replay(ctx, d.event("CHANNEL_ANSWER", at, nil))
		}
	}
	bridged := map[string]bool{}
	for _, d := range adopted {
		id, peerID := d.get("Unique-ID"), d.bridgedTo(byID)
		if peerID == "" || bridged[id] || bridged[peerID] {
			continue
		}
		if !c.registry.Owns(peerID) {
			continue
		}
		bridged[id], bridged[peerID] = true, true
		at := d.micros("Caller-Channel-Bridged-Time")
		c.replay(ctx, d.event("CHANNEL_BRIDGE", at, map[string]string{
			"Bridge-A-Unique-ID": id, "Bridge-B-Unique-ID": peerID,
		}))
	}

	if isComplete {
		c.closeVanished(ctx, listed, snapshotStart)
	} else {
		log.Warn("channel listing was incomplete; no vanished channel is closed on its evidence")
	}

	log.Info("live channels adopted",
		"listed", len(rows), "read", len(all), "ours", len(found), "adopted", len(adopted))
	return found, nil
}

// VanishedCause is the release cause recorded on a leg whose hangup this
// process never saw: the switch no longer has the channel, and nothing says how
// it ended. A word of our own rather than a Q.850 name, because no switch said
// it, and the ledger should show that the ending is an inference.
const VanishedCause = "CHANNEL_VANISHED"

// closeVanished ends, through the ordinary hangup path, every channel the
// registry holds that a complete listing does not show. A hangup lost while
// the link was down otherwise leaves the call open for ever: its agent
// stays on a call and no CDR is written.
//
// A channel the registry learned of after the snapshot began is exempt: it
// may have been created too late for the listing to show it.
func (c *Coordinator) closeVanished(ctx context.Context, listed map[string]bool, snapshotStart time.Time) {
	for id, boundAt := range c.registry.BoundChannels() {
		if listed[id] || !boundAt.Before(snapshotStart) {
			continue
		}
		slog.Warn("closing a channel the switch no longer has", "channelId", id, "cause", VanishedCause)
		c.replay(ctx, esl.NewEvent(map[string]string{
			"Event-Name": "CHANNEL_HANGUP_COMPLETE", "Unique-ID": id, "Hangup-Cause": VanishedCause,
			"Event-Date-Timestamp": strconv.FormatInt(time.Now().UnixMicro(), 10),
		}, ""))
	}
}

// AbandonBot says that the bot session behind a call will never write its
// row, because the process that ran it is gone and recovery is ending the
// call. From here the call's CDR is written by the ordinary retirement path,
// which would otherwise leave a bot call to its session. It is a no-op for a
// call the registry does not hold.
func (c *Coordinator) AbandonBot(callID uuid.UUID) {
	_ = c.registry.Do(callID, func(call *Call) { call.IsBotAbandoned = true })
}

func (c *Coordinator) replay(ctx context.Context, raw *esl.Event) {
	if ev, ok := Normalize(raw); ok {
		c.Handle(ctx, ev)
	}
}

func liveChannelOf(d dumped, callID uuid.UUID, byID map[string]dumped) LiveChannel {
	ch := LiveChannel{
		ChannelID:     d.get("Unique-ID"),
		CallID:        callID,
		CreatedAt:     d.createdAt(),
		IsInbound:     strings.EqualFold(d.get("Call-Direction"), "inbound"),
		IsAnswered:    d.isAnswered(),
		IsParked:      d.isParked(),
		PeerChannelID: d.bridgedTo(byID),
		BridgedAt:     d.micros("Caller-Channel-Bridged-Time"),
		Language:      d.variable("aicc_language"),
		DID:           d.variable("aicc_did"),
		PlacedBy:      d.variable(VarPlacedBy),
		PlacedTo:      d.variable(VarPlacedTo),
		Destination:   d.variable("aicc_destination"),
	}
	now := d.switchNow()
	if created := d.createdAt(); !now.IsZero() && !created.IsZero() && !now.Before(created) {
		ch.Age, ch.IsAgeKnown = now.Sub(created), true
	}
	if ev, ok := Normalize(d.event("CHANNEL_CREATE", time.Time{}, nil)); ok {
		ch.IsBotLeg = isBotLeg(ev)
	}
	if ch.PeerChannelID == "" {
		return ch
	}
	peer := byID[ch.PeerChannelID]
	if ev, ok := Normalize(peer.event("CHANNEL_CREATE", time.Time{}, nil)); ok {
		ch.PeerIsBotLeg = isBotLeg(ev)
	}
	// When the bridge began: its own stamp on either leg, else when the peer
	// answered, which is the earliest the bridge could have been made.
	since := d.micros("Caller-Channel-Bridged-Time")
	if since.IsZero() {
		since = peer.micros("Caller-Channel-Bridged-Time")
	}
	if since.IsZero() {
		since = peer.micros("Caller-Channel-Answered-Time")
	}
	if !now.IsZero() && !since.IsZero() && !now.Before(since) {
		ch.BridgeAge, ch.IsBridgeAgeKnown = now.Sub(since), true
	}
	return ch
}

// decodeStringMap reads a flat JSON object into strings, tolerating numbers.
func decodeStringMap(payload string, out *map[string]string) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	var generic map[string]any
	if err := dec.Decode(&generic); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(generic) == 0 {
		return errors.New("empty dump")
	}
	m := make(map[string]string, len(generic))
	for k, v := range generic {
		switch t := v.(type) {
		case string:
			m[k] = t
		case float64:
			m[k] = strconv.FormatInt(int64(t), 10)
		case bool:
			m[k] = strconv.FormatBool(t)
		}
	}
	*out = m
	return nil
}
