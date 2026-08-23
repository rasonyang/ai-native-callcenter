// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// callDataTTL is how long business data waits for its call to appear.
//
// A placed call reaches the switch, comes back as a channel, and only then
// exists here — a second or two normally. Anything still waiting minutes later
// belongs to a call that never happened: the originate failed after the entry
// was made, or the switch took the request and nothing came of it. Holding
// those forever would make this a slow leak of exactly the data least worth
// leaking.
const callDataTTL = 10 * time.Minute

// callDataMax bounds the store against a caller placing many calls that never
// materialise. Oldest entries go first; losing business data for a call that
// does not exist costs nothing.
const callDataMax = 4096

// CallData carries the business data a request attached to a call, from the
// moment the call is placed to the moment something can hold it.
//
// It exists because that data deliberately does not travel through the switch.
// Business data on a channel variable is business data in the switch's logs,
// its database and its event stream, and the agent's screen is reached through
// this application either way. So the request leaves it here, and the two
// things that build a call's identity — the call registry, and the bot's own
// session — read it by call id.
//
// Read, not taken: both of those read it, and for an AI call that the bot
// finishes alone the bot's read is the one that reaches the ledger.
type CallData struct {
	mu      sync.Mutex
	entries map[uuid.UUID]callDataEntry
	now     func() time.Time
}

type callDataEntry struct {
	data      map[string]any
	expiresAt time.Time
}

// NewCallData builds an empty store.
func NewCallData() *CallData {
	return &CallData{entries: map[uuid.UUID]callDataEntry{}, now: time.Now}
}

// Put records the data a call was asked to carry. An empty map records
// nothing: no business data and none given are the same thing.
func (d *CallData) Put(callID uuid.UUID, data map[string]string) {
	if len(data) == 0 {
		return
	}
	copied := make(map[string]any, len(data))
	for k, v := range data {
		copied[k] = v
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked()
	d.entries[callID] = callDataEntry{data: copied, expiresAt: d.now().Add(callDataTTL)}
}

// CallData returns what the call was asked to carry, or nil.
func (d *CallData) CallData(callID uuid.UUID) map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[callID]
	if !ok || d.now().After(entry.expiresAt) {
		return nil
	}
	out := make(map[string]any, len(entry.data))
	for k, v := range entry.data {
		out[k] = v
	}
	return out
}

// Drop forgets a call's data. Called when the call could not be placed: the
// entry would otherwise wait out its life for something that will never ask.
func (d *CallData) Drop(callID uuid.UUID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, callID)
}

// sweepLocked removes what has expired, and then the oldest if the store is
// still over its bound.
func (d *CallData) sweepLocked() {
	now := d.now()
	for id, entry := range d.entries {
		if now.After(entry.expiresAt) {
			delete(d.entries, id)
		}
	}
	for len(d.entries) >= callDataMax {
		var oldest uuid.UUID
		var at time.Time
		for id, entry := range d.entries {
			if at.IsZero() || entry.expiresAt.Before(at) {
				oldest, at = id, entry.expiresAt
			}
		}
		delete(d.entries, oldest)
	}
}
