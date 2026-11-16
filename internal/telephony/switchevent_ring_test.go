// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// The fixtures in testdata/events are the dialled leg's own events, captured
// from a real switch (1.11.3) in the e2e stack with a callee simulator ending
// calls in the ways a carrier does (#576):
//
//	A  180 at 2 s, then the far end's 408 at 25 s
//	D  183 with SDP at 2 s, then 408 at 10 s
//	E  180 at 2 s, then this platform's own CANCEL at the ring timeout
//	F  180 at 2 s, 200 at 5 s, then BYE
//
// loadEvent reads one the way the client does: percent-encoded values.
func loadEvent(t *testing.T, name string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if decoded, err := url.QueryUnescape(v); err == nil {
			v = decoded
		}
		headers[k] = v
	}
	return headers
}

func normalized(t *testing.T, headers map[string]string) SwitchEvent {
	t.Helper()
	got, ok := Normalize(esl.NewEvent(headers, ""))
	if !ok {
		t.Fatalf("%s was not normalized", headers["Event-Name"])
	}
	return got
}

func TestNormalizeReadsRingProgressAndTheFarEndsStatusFromTheHangup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fixture    string
		wantRing   time.Time // zero: the leg never rang
		wantStatus int       // 0: the switch reported none
	}{
		{"180 then the far end's 408", "A_channel_hangup_complete.txt", time.UnixMicro(1794820125494569).UTC(), 408},
		{"183 with SDP sets only the media time", "D_channel_hangup_complete.txt", time.UnixMicro(1794820414834617).UTC(), 408},
		{"our own cancel leaves no status", "E_channel_hangup_complete.txt", time.UnixMicro(1794820511337144).UTC(), 0},
		{"an answered call ends 200", "F_channel_hangup_complete.txt", time.UnixMicro(1794820061595604).UTC(), 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalized(t, loadEvent(t, tt.fixture))
			if got.Kind != KindChannelHangup {
				t.Fatalf("kind = %v, want a hangup", got.Kind)
			}
			if !got.RingStartedAt.Equal(tt.wantRing) {
				t.Errorf("RingStartedAt = %v, want %v", got.RingStartedAt, tt.wantRing)
			}
			if got.SIPStatus != tt.wantStatus {
				t.Errorf("SIPStatus = %d, want %d", got.SIPStatus, tt.wantStatus)
			}
		})
	}
}

func TestNormalizeTakesTheEarlierOfTheTwoProgressTimes(t *testing.T) {
	t.Parallel()
	h := loadEvent(t, "A_channel_hangup_complete.txt")

	// Neither set: the far end ended the call without a 180 or 183. Unset
	// times are written as 0, never left out.
	h["Caller-Channel-Progress-Time"], h["Caller-Channel-Progress-Media-Time"] = "0", "0"
	if got := normalized(t, h); !got.RingStartedAt.IsZero() || got.SIPStatus != 408 {
		t.Errorf("no progress: ring %v, status %d; want zero, 408", got.RingStartedAt, got.SIPStatus)
	}

	// Both set: the first of them.
	h["Caller-Channel-Progress-Time"], h["Caller-Channel-Progress-Media-Time"] = "2000000", "1000000"
	if got := normalized(t, h); !got.RingStartedAt.Equal(time.UnixMicro(1000000).UTC()) {
		t.Errorf("both set: ring %v, want the earlier", got.RingStartedAt)
	}

	// Neither header at all.
	delete(h, "Caller-Channel-Progress-Time")
	delete(h, "Caller-Channel-Progress-Media-Time")
	if got := normalized(t, h); !got.RingStartedAt.IsZero() {
		t.Errorf("no headers: ring %v, want zero", got.RingStartedAt)
	}
}

// The ring start rides the answer too, so a leg that is still up when the
// application restarts and replays its state from the switch has it (the
// replayed event carries the dump's headers).
func TestNormalizeReadsTheRingStartOnAnAnswer(t *testing.T) {
	t.Parallel()
	got := normalized(t, loadEvent(t, "F_channel_answer.txt"))
	if got.Kind != KindChannelAnswer {
		t.Fatalf("kind = %v, want an answer", got.Kind)
	}
	if want := time.UnixMicro(1794820061595604).UTC(); !got.RingStartedAt.Equal(want) {
		t.Errorf("RingStartedAt = %v, want %v", got.RingStartedAt, want)
	}
	if got.SIPStatus != 0 {
		t.Errorf("SIPStatus = %d on an answer, want 0 (only a hangup reports it)", got.SIPStatus)
	}
}
