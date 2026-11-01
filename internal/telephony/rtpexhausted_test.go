// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

const legUUID = "019ff973-6b32-7557-b7c4-11b3fdb692f0"

// logFrame is a forwarded log line as the client delivers it.
func logFrame(level, file, text string) *esl.Event {
	return esl.NewLogEvent(map[string]string{
		"Content-Type": "log/data", "Log-Level": level, "Text-Channel": "0",
		"Log-File": file, "Log-Func": "switch_core_media_choose_port", "Log-Line": "1234",
		"User-Data": legUUID,
	}, text)
}

// critLine is a log line's body as mod_event_socket sends it: the whole
// formatted line (switch_log.c), date, idle CPU, level and file:line first.
func critLine(level, file, msg string) string {
	return "2026-11-01 01:03:16.790420 97.83% [" + level + "] " + file + ":8191 " + msg + "\n"
}

func TestTheCritNoRTPPortsLineBecomesAnEvent(t *testing.T) {
	t.Parallel()
	for _, media := range []string{"audio", "video", "text"} {
		got, ok := Normalize(logFrame("2", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "No "+media+" RTP ports available!")))
		if !ok {
			t.Fatalf("%s: not recognised", media)
		}
		if got.Kind != KindSwitchRTPPortsExhausted || got.ChannelID != legUUID || got.Media != media {
			t.Errorf("%s: got kind %q channel %q media %q", media, got.Kind, got.ChannelID, got.Media)
		}
	}
	// The file may be a path, and a more severe level still counts.
	if _, ok := Normalize(logFrame("1", "/usr/src/freeswitch/src/switch_core_media.c", critLine("ALERT", "switch_core_media.c", "No audio RTP ports available!"))); !ok {
		t.Error("a path and an ALERT level were not recognised")
	}
}

func TestEveryOtherLogLineIsIgnored(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]*esl.Event{
		"another CRIT line":       logFrame("2", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "Cannot allocate codec")),
		"another media":           logFrame("2", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "No fax RTP ports available!")),
		"a longer text":           logFrame("2", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "No audio RTP ports available! really")),
		"the same text at ERR":    logFrame("3", "switch_core_media.c", critLine("ERR", "switch_core_media.c", "No audio RTP ports available!")),
		"the same text at INFO":   logFrame("6", "switch_core_media.c", critLine("INFO", "switch_core_media.c", "No audio RTP ports available!")),
		"the same text elsewhere": logFrame("2", "mod_sofia.c", critLine("CRIT", "mod_sofia.c", "No audio RTP ports available!")),
		"a word glued to No":      logFrame("2", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "XNo audio RTP ports available!")),
		"no level":                logFrame("", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "No audio RTP ports available!")),
	} {
		if got, ok := Normalize(ev); ok {
			t.Errorf("%s: became %q", name, got.Kind)
		}
	}
	// A plain event with the same words in its body is not a log line.
	if _, ok := Normalize(esl.NewEvent(map[string]string{"Log-Level": "2", "Log-File": "switch_core_media.c"},
		"No audio RTP ports available!")); ok {
		t.Error("an event was read as a log line")
	}
}

// capture swaps the default logger for one that writes to a buffer.
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Not parallel: it replaces the process's default logger.
func TestRTPExhaustionIsLoggedAtErrorWithTheCallWhenKnown(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	out := &capture{}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})
	callID := uuid.New()
	if _, err := registry.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindChannel(legUUID, callID); err != nil {
		t.Fatal(err)
	}

	ev, ok := Normalize(logFrame("2", "switch_core_media.c", critLine("CRIT", "switch_core_media.c", "No audio RTP ports available!")))
	if !ok {
		t.Fatal("the CRIT line was not recognised")
	}
	c.ReportRTPPortsExhausted(context.Background(), ev)
	line := out.String()
	for _, want := range []string{"level=ERROR", `msg="switch out of RTP ports"`, "legId=" + legUUID,
		"callId=" + callID.String(), "media=audio", "FS_RTP_START_PORT/FS_RTP_END_PORT", "RTP_START/RTP_END"} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}

	// A leg no call owns yet is still reported, without a call.
	out.mu.Lock()
	out.buf.Reset()
	out.mu.Unlock()
	ev.ChannelID = uuid.NewString()
	c.ReportRTPPortsExhausted(context.Background(), ev)
	line = out.String()
	if !strings.Contains(line, `msg="switch out of RTP ports"`) || strings.Contains(line, "callId=") {
		t.Errorf("log %q for an unbound leg", line)
	}
}
