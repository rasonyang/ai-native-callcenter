// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QueueMember is one caller mod_callcenter is holding for a queue right now.
//
// Runtime state, like Tier: the switch owns who is queued and reports it, and
// there is no way to learn it from events alone once an event has been missed.
type QueueMember struct {
	Queue string
	// ChannelID is the caller's own channel — the switch's session_uuid, which
	// is what every leg raised on their behalf points back to and the only
	// identifier that survives the delivery.
	ChannelID string
	Number    string
	JoinedAt  time.Time
	// StartedAt is when the call itself began, which the switch reports
	// alongside the join. A call adopted after a restart needs its real start:
	// stamping it with the moment we noticed would make every recovered call's
	// duration begin at the recovery.
	StartedAt time.Time
	// State is the switch's word for what is happening to this member:
	// Waiting, Trying, Answered and so on. Kept verbatim; the decision about
	// which states count as waiting belongs to the reader.
	State string
}

// IsWaiting reports whether this member still counts as queued.
//
// Waiting and Trying both do: a caller an agent is being rung for has not
// reached anybody yet, and dropping them at the first offer would empty the
// list every time the queue tried somebody. Answered and the rest do not — the
// switch's own count agrees, and so does the live path, which drops a member
// on queue-bridge-start.
func (m QueueMember) IsWaiting() bool {
	switch strings.ToLower(m.State) {
	case "waiting", "trying":
		return true
	}
	return false
}

// ChannelVariable reads one variable off a live channel.
//
// Needed to adopt a call this process never saw start: the identity the
// dialplan minted for it lives on the channel and nowhere else, and recovering
// that identity rather than minting a new one is what keeps the recording, the
// transcript and the CDR talking about the same call.
//
// An unset variable is not an error — the switch answers with its own word for
// nothing, and so does this: the empty string. Anything unrecognised reads as
// empty too, so a misread degrades to "we could not adopt this call" rather
// than to a call adopted with a wrong identity.
func (a *Adapter) ChannelVariable(channelID, name string) (string, error) {
	out, err := a.cmd.API(fmt.Sprintf("uuid_getvar %s %s", channelID, name))
	if err != nil {
		return "", fmt.Errorf("read %s of %s: %w", name, channelID, err)
	}
	return parseChannelVariable(out), nil
}

// parseChannelVariable reads what uuid_getvar answers. The switch says _undef_
// for a variable that is not set and -ERR for a channel that is gone; neither
// is a value.
func parseChannelVariable(out string) string {
	out = strings.TrimSpace(out)
	if out == "" || out == "_undef_" || strings.HasPrefix(out, "-ERR") {
		return ""
	}
	return out
}

// QueueMembers asks the switch who is queued for one queue.
//
// The events that announce a join can be missed — they are missed for the
// whole of a restart, which is the case that matters, because a caller
// transferred to a queue while this process was down is announced to nobody
// and would wait unseen until an agent happened to answer them.
func (a *Adapter) QueueMembers(queue string) ([]QueueMember, error) {
	out, err := a.ListQueueMembers(queue)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", queue, err)
	}
	return parseQueueMembers(out), nil
}

// Column positions in mod_callcenter's member listing. Named rather than
// inlined because five of the seventeen are read and the header is the only
// thing that says which.
const (
	memberColQueue       = 0
	memberColSessionUUID = 3
	memberColCIDNumber   = 4
	memberColSystemEpoch = 6
	memberColJoinedEpoch = 7
	memberColState       = 15
	memberColCount       = 17
)

// parseQueueMembers reads mod_callcenter's pipe-separated listing, in the same
// spirit as parseTiers: the header line and the trailing +OK are not members,
// and a row that does not carry the full column count is skipped rather than
// guessed at.
//
// joined_epoch is the switch's own answer to "since when", and it is the whole
// point of reading this: restoring a caller with time.Now() would reset their
// wait to zero, so a call that had been queued for two minutes would report as
// new and the queue's service level would improve for having lost track of
// them. A member with no usable join time is dropped, because a waiting time
// invented here is worse than a caller the list rebuilds on the next event.
func parseQueueMembers(out string) []QueueMember {
	var members []QueueMember
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+OK") || strings.HasPrefix(line, "queue|") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < memberColCount {
			continue
		}
		joined, err := strconv.ParseInt(parts[memberColJoinedEpoch], 10, 64)
		if err != nil || joined <= 0 {
			continue
		}
		channelID := strings.TrimSpace(parts[memberColSessionUUID])
		if channelID == "" {
			continue
		}
		member := QueueMember{
			Queue:     parts[memberColQueue],
			ChannelID: channelID,
			Number:    strings.TrimSpace(parts[memberColCIDNumber]),
			JoinedAt:  time.Unix(joined, 0).UTC(),
			State:     strings.TrimSpace(parts[memberColState]),
		}
		// The call's own start, where the switch reports one. Unlike the join
		// time this is not worth dropping a member over: a caller in a queue
		// is worth knowing about even if their call has to be dated from the
		// moment they joined it.
		if started, err := strconv.ParseInt(parts[memberColSystemEpoch], 10, 64); err == nil && started > 0 {
			member.StartedAt = time.Unix(started, 0).UTC()
		}
		members = append(members, member)
	}
	return members
}
