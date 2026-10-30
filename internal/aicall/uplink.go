// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

const (
	// uplinkSlot is the interval the provider's input timeline advances by
	// per frame. Providers run turn detection and echo cancellation that
	// assume one frame every slot.
	uplinkSlot = frameDurationMs * time.Millisecond

	// uplinkMaxBacklog is the jitter-buffer depth of the uplink clock: 100 ms.
	// A frame that finds the queue this deep pushes the oldest out. A frame
	// that arrives after its slot was filled with silence is late, and this
	// bound drops it rather than letting it push later speech back.
	uplinkMaxBacklog = 5
)

// uplinkStats is what one call's uplink clock did; read at teardown.
type uplinkStats struct {
	// Received is every frame the clock was given.
	Received int64
	// Sent is every slot the provider was fed, real frames and fills.
	Sent int64
	// Filled is the slots that carried silence because nothing was queued.
	Filled int64
	// DroppedBacklog is frames pushed out by the depth bound.
	DroppedBacklog int64
	// DroppedHushed is frames discarded because a transfer had hushed the call.
	DroppedHushed int64
	// MaxBacklog is the deepest the queue got.
	MaxBacklog int
}

// uplinkClock is the receive clock of an AI leg's uplink: the provider gets
// exactly one frame per uplinkSlot of elapsed wall time. The count follows
// elapsed time, not ticks, so a goroutine woken late sends the frames it owes
// and the timeline cannot compress. It owns no goroutine and reads no clock:
// the pump (and the tests) hand it the time.
//
// Nothing here looks at RTP sequence numbers or timestamps. The bridge writes
// nothing for a comfort-noise frame, so they do not locate the skipped slots;
// the bound is on queue depth.
type uplinkClock struct {
	start     time.Time
	slotsDone int64

	queue [uplinkMaxBacklog][]byte
	head  int
	count int

	stats uplinkStats
}

func newUplinkClock(start time.Time) *uplinkClock { return &uplinkClock{start: start} }

// push queues one frame, taking ownership. When the queue is full the oldest
// frame is released to the pool and counted.
func (c *uplinkClock) push(frame []byte) {
	c.stats.Received++
	if c.count == uplinkMaxBacklog {
		media.PutBytes(c.queue[c.head])
		c.queue[c.head] = nil
		c.head = (c.head + 1) % uplinkMaxBacklog
		c.count--
		c.stats.DroppedBacklog++
	}
	c.queue[(c.head+c.count)%uplinkMaxBacklog] = frame
	c.count++
	if c.count > c.stats.MaxBacklog {
		c.stats.MaxBacklog = c.count
	}
}

// owed reports how many slots have elapsed since the last call, and counts
// them as accounted for.
func (c *uplinkClock) owed(now time.Time) int {
	slots := int64(now.Sub(c.start) / uplinkSlot)
	n := slots - c.slotsDone
	if n <= 0 {
		return 0
	}
	c.slotsDone = slots
	return int(n)
}

// pop takes the oldest queued frame for a slot; nil when none is queued.
// The caller returns a non-nil frame to the pool.
func (c *uplinkClock) pop() []byte {
	if c.count == 0 {
		return nil
	}
	frame := c.queue[c.head]
	c.queue[c.head] = nil
	c.head = (c.head + 1) % uplinkMaxBacklog
	c.count--
	return frame
}

// queued is how many frames are waiting.
func (c *uplinkClock) queued() int { return c.count }

// release returns every queued frame to the pool.
func (c *uplinkClock) release() {
	for frame := c.pop(); frame != nil; frame = c.pop() {
		media.PutBytes(frame)
	}
}

// uplinkSilenceFrame is one frame of silence in the provider's input format,
// built once per call: the law's own silence byte on the G.711 passthrough
// path, PCM16 zeros after conversion. The slice is shared and read-only; the
// providers copy or encode what SendAudio hands them.
func uplinkSilenceFrame(input media.AudioFormat) []byte {
	switch input.Encoding {
	case media.EncodingMuLaw:
		return media.SilenceFrame(media.LawMu)
	case media.EncodingALaw:
		return media.SilenceFrame(media.LawAlaw)
	}
	// 20 ms of 16-bit samples.
	return make([]byte, input.RateHz/50*2)
}
