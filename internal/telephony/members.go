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
		members = append(members, QueueMember{
			Queue:     parts[memberColQueue],
			ChannelID: channelID,
			Number:    strings.TrimSpace(parts[memberColCIDNumber]),
			JoinedAt:  time.Unix(joined, 0).UTC(),
			State:     strings.TrimSpace(parts[memberColState]),
		})
	}
	return members
}
