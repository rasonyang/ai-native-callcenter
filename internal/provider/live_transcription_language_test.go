// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures what the transcription language the product sends does to the
// caller transcript on qwen (#602, qwen-findings W-Q19): the session
// transcribes the caller unprompted, and on production zh calls a lone "嗯"
// came back as Korean "음." Each session is a raw WebSocket whose opening
// session.update is built by the product's own code, QwenProfile through
// buildSessionUpdate with a zh call, sent right after the dial and before
// session.created as the product does. It costs money, so it is skipped unless
// AICC_LIVE_PROVIDER_TEST and ALIYUN_API_KEY are set. It fails on a harness
// error, never on the vendor's choice.
//
// Arms (AICC_PROBE_ARMS, comma-separated, one session each, default
// product,none,product,none):
//
//	product  the profile as shipped (AcceptsTranscriptionLanguage on)
//	none     the same with the trait cleared: no transcription field
//
// The session.created / session.updated echoes of input_audio_transcription
// are logged, and after the last clip an instructions-only session.update (the
// shape Realtime.UpdateInstructions sends) is made and its echo logged, to see
// that the language outlives it.
//
// The caller (testdata/q_tl_<clip>.pcm, 16 kHz mono PCM16 little-endian) was
// made on 2026-11-18 with `say -v Tingting` (zh_en 嗯, zh_wei 喂), `say -v
// Yuna` (ko_eum 음) and `say -v Kyoko` (ja_un うん) to AIFF, `afconvert -f WAVE
// -d LEI16@16000 -c 1`, and everything before the WAVE data chunk removed (the
// header is longer than 44 bytes: afconvert adds a FLLR padding chunk). The ko
// and ja clips discriminate: a honoured zh hint should write them in Chinese
// characters, and the ja clip shows the hint is a bias, not a script filter.
//
// Each session streams 20 ms frames at real time: 1 s of silence, then each
// clip followed by 4 s of silence, waiting for the clip's transcription
// (or 16 s) before the next. The model's spoken replies are ignored. Raw
// events go to AICC_PROBE_DIR (default $TMPDIR/probe602).
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -v -run TestLiveQwenTranscriptionLanguage ./internal/provider/
func TestLiveQwenTranscriptionLanguage(t *testing.T) {
	profile := liveProfile(QwenProfile())
	requireLive(t, profile)
	dir := os.Getenv("AICC_PROBE_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "probe602")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	arms := strings.Split(envOr("AICC_PROBE_ARMS", "product,none,product,none"), ",")
	clips := []string{"zh_en", "zh_wei", "ko_eum", "ja_un"}
	pcm := map[string][]byte{}
	for _, c := range clips {
		pcm[c] = readMidClip(t, profile.LinearInput, "q_tl_"+c)
	}

	type row struct {
		label      string
		created    string
		updated    string
		errs       []string
		transcript map[string]string
	}
	var rows []row
	for i, arm := range arms {
		arm = strings.TrimSpace(arm)
		label := fmt.Sprintf("%s#%d", arm, i+1)
		r := probeTranscription(t, profile, arm, filepath.Join(dir, fmt.Sprintf("%02d_%s.jsonl", i+1, arm)), clips, pcm)
		rows = append(rows, row{label, r.created, r.updated, r.errs, r.transcript})
		t.Logf("[%s] mid-call (instructions only) session.updated input_audio_transcription: %s", label, r.midUpdated)
		t.Logf("[%s] session.created input_audio_transcription: %s", label, r.created)
		t.Logf("[%s] session.updated input_audio_transcription: %s", label, r.updated)
		for _, e := range r.errs {
			t.Logf("[%s] ERROR %s", label, e)
		}
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "session\t%s\n", strings.Join(clips, "\t"))
	for _, r := range rows {
		cells := make([]string, len(clips))
		for i, c := range clips {
			cells[i] = fmt.Sprintf("%q", r.transcript[c])
		}
		fmt.Fprintf(w, "%s\t%s\n", r.label, strings.Join(cells, "\t"))
	}
	w.Flush()
	t.Logf("RESULT %s %s\n%s", profile.Name, profile.Model, b.String())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type transcriptionProbe struct {
	created, updated, midUpdated string
	errs                         []string
	transcript                   map[string]string
}

func probeTranscription(t *testing.T, profile Profile, arm, logPath string, clips []string, pcm map[string][]byte) transcriptionProbe {
	t.Helper()
	out := transcriptionProbe{transcript: map[string]string{}}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+os.Getenv(profile.APIKeyEnv))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, profile.endpointURL(), headers)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	events := make(chan map[string]any, 4096)
	go func() {
		defer close(events)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ev map[string]any
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			typ, _ := ev["type"].(string)
			if !strings.Contains(typ, "audio.delta") && !strings.HasSuffix(typ, "audio_transcript.delta") {
				line := string(data)
				if !(strings.HasPrefix(typ, "session.") || typ == "error" || strings.Contains(typ, "input_audio_transcription")) {
					line = `{"type":"` + typ + `"}`
				}
				fmt.Fprintln(logFile, line)
			}
			events <- ev
		}
	}()

	transcription := func(ev map[string]any) string {
		s, _ := ev["session"].(map[string]any)
		v, ok := s["input_audio_transcription"]
		if !ok {
			return "(absent)"
		}
		b, _ := json.Marshal(v)
		return string(b)
	}

	// As the product does: the session.update goes out right after the dial,
	// before session.created has arrived.
	update := buildProbeUpdate(t, profile, arm)
	fmt.Fprintf(logFile, "SENT %s\n", update)
	if err := conn.WriteMessage(websocket.TextMessage, update); err != nil {
		t.Fatal(err)
	}

	// session.created
	select {
	case ev, ok := <-events:
		if !ok || ev["type"] != "session.created" {
			t.Fatalf("first event is not session.created: %v", ev)
		}
		out.created = transcription(ev)
	case <-time.After(15 * time.Second):
		t.Fatal("no session.created")
	}

	// Pace the uplink and consume events on one loop.
	const frameBytes = 640
	silence := make([]byte, frameBytes)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var queue [][]byte
	push := func(b []byte) {
		for len(b) >= frameBytes {
			queue = append(queue, b[:frameBytes])
			b = b[frameBytes:]
		}
	}
	silenceFrames := func(d time.Duration) {
		for range int(d / (20 * time.Millisecond)) {
			queue = append(queue, silence)
		}
	}
	push(nil)
	silenceFrames(time.Second)
	next := 0
	waiting := ""
	var deadline time.Time
	var tail int
	feed := func() {
		if len(queue) == 0 {
			queue = append(queue, silence)
		}
		frame := queue[0]
		queue = queue[1:]
		msg, _ := json.Marshal(map[string]any{"type": "input_audio_buffer.append", "audio": b64(frame)})
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	started := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("probe timed out")
		case ev, ok := <-events:
			if !ok {
				t.Fatal("connection closed")
			}
			typ, _ := ev["type"].(string)
			b, _ := json.Marshal(ev)
			switch {
			case typ == "session.updated":
				if out.updated == "" {
					out.updated = transcription(ev)
					started = true
				} else {
					out.midUpdated = transcription(ev)
					fmt.Fprintf(logFile, "MID-CALL ECHO %s\n", b)
					return out
				}
			case typ == "error":
				out.errs = append(out.errs, string(b))
			case typ == "conversation.item.input_audio_transcription.completed":
				if waiting != "" {
					text, _ := ev["transcript"].(string)
					out.transcript[waiting] = text
					waiting = ""
					deadline = time.Time{}
					silenceFrames(500 * time.Millisecond)
				}
			case typ == "conversation.item.input_audio_transcription.failed":
				out.errs = append(out.errs, string(b))
				if waiting != "" {
					out.transcript[waiting] = "(failed)"
					waiting = ""
				}
			case strings.HasSuffix(typ, "input_audio_transcription.delta") || strings.HasSuffix(typ, "input_audio_transcription.text"):
				t.Logf("[%s] %s: %s", arm, typ, b)
			}
		case <-ticker.C:
			if !started {
				continue
			}
			if waiting == "" && len(queue) == 0 && next < len(clips) {
				waiting = clips[next]
				push(pcm[waiting])
				silenceFrames(4 * time.Second)
				next++
				deadline = time.Now().Add(16 * time.Second)
			} else if waiting == "" && len(queue) == 0 && next >= len(clips) {
				tail++
				if tail == 1 {
					// Mid-call update, shaped as Realtime.UpdateInstructions sends it.
					mid, _ := json.Marshal(map[string]any{"type": "session.update",
						"session": map[string]any{"instructions": "你是客服，请用中文简短回答。（更新）"}})
					fmt.Fprintf(logFile, "SENT %s\n", mid)
					if err := conn.WriteMessage(websocket.TextMessage, mid); err != nil {
						t.Fatal(err)
					}
				}
				if tail > 500 {
					return out
				}
			}
			if waiting != "" && time.Now().After(deadline) {
				out.transcript[waiting] = "(timeout)"
				waiting = ""
			}
			feed()
		}
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// buildProbeUpdate is the opening session.update the product would send for a
// zh call, from the product's own code. Arm "none" clears the trait.
func buildProbeUpdate(t *testing.T, profile Profile, arm string) []byte {
	t.Helper()
	switch arm {
	case "product":
	case "none":
		profile.AcceptsTranscriptionLanguage = false
	default:
		t.Fatalf("unknown arm %q", arm)
	}
	input, output := profile.FormatsFor(media.LawMu)
	client := &Realtime{profile: profile}
	update, err := json.Marshal(client.buildSessionUpdate(SessionConfig{
		Instructions: "你是客服，请用中文简短回答。",
		Language:     "zh-CN",
		InputFormat:  input,
		OutputFormat: output,
		Turn:         DefaultTurnDetection(),
	}, false))
	if err != nil {
		t.Fatal(err)
	}
	return update
}
