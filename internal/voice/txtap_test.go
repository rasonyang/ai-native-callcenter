// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// The tap sees what each send tick put on the wire: the queued audio when
// there was some, nil when the tick sent padding.
func TestTxTapSeesTheAudioAndThePaddingTheSendLoopSends(t *testing.T) {
	t.Parallel()
	session, arrivals, _ := startPacer(t, 0, nil)

	type tick struct {
		frame []byte
		at    time.Time
	}
	var mu sync.Mutex
	var ticks []tick
	session.SetTxTap(func(frame []byte, at time.Time) {
		mu.Lock()
		defer mu.Unlock()
		// The loop reuses its buffers: a tap that keeps a frame must copy it.
		ticks = append(ticks, tick{append([]byte(nil), frame...), at})
		if frame == nil {
			ticks[len(ticks)-1].frame = nil
		}
	})
	audio := bytes.Repeat([]byte{0x42}, media.FrameSamples)
	for range 8 {
		session.Send(audio)
	}
	// Let the 8 frames play and some padding follow.
	for range 40 {
		nextArrival(t, arrivals)
	}
	session.SetTxTap(nil)

	mu.Lock()
	defer mu.Unlock()
	var real, padding int
	for i, tk := range ticks {
		switch {
		case tk.frame == nil:
			padding++
		case bytes.Equal(tk.frame, audio):
			real++
		default:
			t.Fatalf("tick %d: tapped a frame that is not the audio sent", i)
		}
		if i > 0 && !tk.at.After(ticks[i-1].at) {
			t.Fatalf("tick %d is not later than the one before", i)
		}
	}
	if real != 8 || padding == 0 {
		t.Errorf("tap saw %d audio frames and %d padding ticks, want 8 and some", real, padding)
	}
}
