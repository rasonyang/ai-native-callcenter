// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real provider, what the model does with a
// caller's first words at an inbound greeting when they say the call is a
// mistake (#554, qwen-findings W-Q18). On owner-placed qwen flash calls to
// novanet_support a wrong number was met with "No problem. What can I help
// you with today?" (or "没关系，请问有什么可以帮您？") in five of eight first
// sayings: every miss a response with words and no tool call, every hit a bare
// hangup. It costs money, so it is skipped unless AICC_LIVE_PROVIDER_TEST and
// the profile's key are set. It is a measurement: it fails on a harness error
// (no session, no greeting, a fatal provider error), never on what the model
// chose.
//
// testdata/wrong_number.json is novanet_support's entry session as
// flow.Runtime renders it (standing instructions, opening line, tools, with the
// demo's two queues), written by flow.TestRenderWrongNumberSession;
// testdata/wrong_number_before.json is the same flow and built-ins as they
// stood before #554, the text of the owner's calls, for an A/B:
//
//	AICC_WRONG_OUT=../provider/testdata/wrong_number.json \
//	go test -count=1 -run TestRenderWrongNumberSession ./internal/flow/
//
// A variant wording is a scratch copy of the flow rendered the same way
// (AICC_WRONG_FLOW=<copy> AICC_WRONG_OUT=<file>) and passed here as
// AICC_LIVE_WRONG_FIXTURE; a variant of the built-ins' text needs the change
// in internal/flow before rendering. A language the fixture lacks is skipped.
//
// The caller (testdata/q_wn_<clip>_<language>.pcm, 16 kHz mono PCM16
// little-endian) was made on 2026-11-13 with macOS `say -v Samantha` (en) or
// `say -v Tingting` (zh) to AIFF, `afconvert -f WAVE -d LEI16@16000 -c 1`, and
// the WAVE header removed. The clips are the issue's live set, wrongNumberClips
// below; "ask" is the question about the number that must not end the call.
//
// One session per run, driven as a call: the opening line is asked for and its
// audio is let play out on the wall clock (from its first audio, its length and
// 700 ms more), room tone streaming throughout; then the clip, at real time,
// with room tone after it. The first response that starts after the clip was
// queued is the one measured: a bare hangup (its reason recorded), words and a
// hangup, words only (the transcript is logged), another tool, or none within
// 15 s of the clip's end. Its tool calls are not answered; the run ends 1.5 s
// after that response, for its transcript.
//
//	AICC_LIVE_PROVIDER_TEST=1 AICC_LIVE_RUNS=10 go test -count=1 -run 'TestLiveQwenWrongNumber/zh' -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 3) is how many sessions each clip opens.
// AICC_LIVE_WRONG_CLIPS (default all) is a comma-separated list of clips
// (wrong, mistake, dialled, someone, ask); `-run 'TestLiveQwenWrongNumber/zh/ask'`
// selects one as well. AICC_LIVE_WRONG_FIXTURE names another fixture.
// AICC_PROVIDER=qwen with AICC_PROVIDER_MODEL measures another qwen model
// (liveProfile). Each language ends with a per-clip table (RESULT).
func TestLiveQwenWrongNumber(t *testing.T) {
	profile := liveProfile(QwenProfile())
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	path := "testdata/wrong_number.json"
	if other := os.Getenv("AICC_LIVE_WRONG_FIXTURE"); other != "" {
		path = other
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]wrongNumberSession
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	selected := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("AICC_LIVE_WRONG_CLIPS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			selected[name] = true
		}
	}
	for _, language := range []string{"zh", "en"} {
		t.Run(language, func(t *testing.T) {
			fx, ok := fixtures[language]
			if !ok {
				t.Skipf("%s has no %s session", path, language)
			}
			input, _ := profile.FormatsFor(media.LawMu)
			var rows []wrongNumberRow
			for _, clip := range wrongNumberClips {
				if len(selected) > 0 && !selected[clip.name] {
					continue
				}
				t.Run(clip.name, func(t *testing.T) {
					audio := readMidClip(t, input, "q_wn_"+clip.name+"_"+language)
					row := wrongNumberRow{clip: clip.name, text: clip.text[language], isEnding: clip.isEnding,
						counts: map[string]int{}}
					for run := 1; run <= runs; run++ {
						r := runWrongNumber(t, profile, language, fx, audio)
						row.add(r)
						t.Logf("[%s/%s #%d] class=%s reason=%q text=%q tools=%v audio=%dms speechSeen=%v "+
							"turnsBeforeCaller=%d greeting=%dms session=%dms",
							language, clip.name, run, r.class, r.reason, r.text, r.tools, r.audioMs,
							r.isSpeechSeen, r.early, r.greetingMs, r.sessionMs)
					}
					rows = append(rows, row)
				})
			}
			t.Logf("[%s] RESULT %s, %s, fixture %s (flow %s), %d run(s) per clip\n%s",
				language, profile.Name, profile.Model, path, fx.Flow, runs, wrongNumberTable(rows))
		})
	}
}

// wrongNumberClips are the caller's first words, the live set of #554.
// isEnding says the call should end on them; the "ask" clips ask whether this
// is the right number, a question to answer, and must not end the call.
var wrongNumberClips = []struct {
	name     string
	text     map[string]string
	isEnding bool
}{
	{"wrong", map[string]string{"en": "Wrong number, sorry.", "zh": "打错了，不好意思。"}, true},
	{"mistake", map[string]string{"en": "Sorry, I called by mistake.", "zh": "不好意思，拨错电话了。"}, true},
	{"dialled", map[string]string{"en": "Oh, I dialled the wrong number.", "zh": "拨错号码了。"}, true},
	{"someone", map[string]string{"en": "I was trying to reach someone else.", "zh": "我找的不是你们。"}, true},
	{"ask", map[string]string{"en": "Is this the right number for billing?", "zh": "我是不是打错了？你们是 NovaNet 吗？"}, false},
}

// wrongNumberSession is one language of testdata/wrong_number.json.
type wrongNumberSession struct {
	Flow         string     `json:"flow"`
	Instructions string     `json:"instructions"`
	Opening      string     `json:"opening"`
	Tools        []ToolSpec `json:"tools"`
	Temperature  *float64   `json:"temperature,omitempty"`
}

// The classes of the measured response.
const (
	wrongClassHangup      = "hangup"       // hangup and no words
	wrongClassWordsHangup = "words+hangup" // words and a hangup in one response
	wrongClassWords       = "words"        // words, no tool call
	wrongClassOther       = "other"        // another tool, no hangup
	wrongClassNone        = "none"         // no response, or one with nothing in it
)

// wrongNumberRun is what one session did.
type wrongNumberRun struct {
	class  string
	reason string   // hangup's reason argument, if it called one
	text   string   // the measured response's transcript
	tools  []string // its calls, name(arguments)
	// audioMs is the measured response's audio; isSpeechSeen says the provider
	// reported the caller's speech before it; early counts turns that started
	// after the greeting and before the clip was queued.
	audioMs      int
	isSpeechSeen bool
	early        int
	greetingMs   int // the opening line's audio
	sessionMs    int // from opening the session to closing it
}

func runWrongNumber(t *testing.T, profile Profile, language string, fx wrongNumberSession,
	clip []byte) (r wrongNumberRun) {
	t.Helper()
	input, output := profile.FormatsFor(media.LawMu)
	opened := time.Now()
	tap := &liveLogTap{}
	c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
		Instructions: fx.Instructions,
		OpeningText:  fx.Opening,
		Language:     language,
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
		Tools:        fx.Tools,
		Temperature:  fx.Temperature,
	})
	defer func() {
		c.close(t)
		r.sessionMs = int(time.Since(opened) / time.Millisecond)
	}()

	outMs, inMs := wrongBytesPerMs(output), wrongBytesPerMs(input)
	var (
		firstAudio     time.Time
		greetingBytes  int
		isGreetingDone bool
		isQueued       bool // the clip is queued
		isResponding   bool // the measured response has started
		isAnswered     bool // it has ended
		isLater        bool // a turn after it has started
		speakAt        <-chan time.Time
		quietAt        <-chan time.Time
		graceAt        <-chan time.Time
		deadline       = time.After(40 * time.Second)
	)
	for {
		select {
		case <-deadline:
			if !isQueued {
				t.Fatalf("harness: the clip was not played within 40 s of opening (greeting ended: %v)", isGreetingDone)
			}
			// The model's doing, not the harness's: measured as it stands.
			t.Logf("the measured response did not end within 40 s of the clip")
			tap.print(t)
			r.classify()
			return r
		case <-speakAt:
			speakAt = nil
			isQueued = true
			c.say(clip)
			// say puts 200 ms of quiet before the clip.
			clipMs := 200 + len(clip)/inMs
			quietAt = time.After(time.Duration(clipMs)*time.Millisecond + 15*time.Second)
			deadline = time.After(time.Duration(clipMs)*time.Millisecond + 40*time.Second)
		case <-quietAt:
			r.classify()
			return r
		case <-graceAt:
			tap.print(t)
			r.classify()
			return r
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatalf("harness: the session ended")
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				switch {
				case !isGreetingDone:
				case !isQueued:
					r.early++
				case !isResponding:
					isResponding, quietAt = true, nil
				default:
					isLater = true
				}
			case EventTypeAudioDelta:
				switch {
				case !isGreetingDone:
					if firstAudio.IsZero() {
						firstAudio = time.Now()
					}
					greetingBytes += len(ev.Audio)
				case isResponding && !isAnswered:
					r.audioMs += len(ev.Audio) / outMs
				}
			case EventTypeOutputTranscript:
				if ev.IsFinal && isResponding && !isLater {
					r.text += ev.Text
				}
			case EventTypeSpeechStarted:
				if isQueued && !isResponding {
					r.isSpeechSeen = true
				}
			case EventTypeToolCall:
				if isResponding && !isLater {
					r.tools = append(r.tools, ev.ToolName+"("+ev.ToolArgs+")")
					if ev.ToolName == liveHangupTool && r.reason == "" {
						var args struct {
							Reason string `json:"reason"`
						}
						_ = json.Unmarshal([]byte(ev.ToolArgs), &args)
						r.reason = args.Reason
					}
				} else {
					t.Logf("a call outside the measured response: %s %s", ev.ToolName, ev.ToolArgs)
				}
			case EventTypeResponseDone, EventTypeInterrupted:
				switch {
				case !isGreetingDone:
					isGreetingDone = true
					if greetingBytes == 0 {
						t.Fatalf("harness: the greeting carried no audio")
					}
					r.greetingMs = greetingBytes / outMs
					// Let the greeting play out, then a short pause.
					wait := time.Until(firstAudio.Add(time.Duration(r.greetingMs+700) * time.Millisecond))
					speakAt = time.After(max(0, wait))
				case isResponding && !isAnswered:
					isAnswered = true
					graceAt = time.After(1500 * time.Millisecond)
				}
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
				if ev.IsFatal {
					t.Fatalf("harness: fatal provider error")
				}
			}
		}
	}
}

// classify names what the measured response did. Words are judged by its
// transcript.
func (r *wrongNumberRun) classify() {
	hasWords := midNormalize(r.text) != ""
	isHangup := false
	for _, call := range r.tools {
		isHangup = isHangup || strings.HasPrefix(call, liveHangupTool+"(")
	}
	switch {
	case isHangup && !hasWords:
		r.class = wrongClassHangup
	case isHangup:
		r.class = wrongClassWordsHangup
	case len(r.tools) > 0:
		r.class = wrongClassOther
	case hasWords:
		r.class = wrongClassWords
	default:
		r.class = wrongClassNone
	}
}

// wrongBytesPerMs is how many bytes of format carry one millisecond.
func wrongBytesPerMs(format media.AudioFormat) int {
	if format.Encoding == media.EncodingMuLaw || format.Encoding == media.EncodingALaw {
		return format.RateHz / 1000
	}
	return format.RateHz * 2 / 1000
}

// wrongNumberRow is one clip's runs.
type wrongNumberRow struct {
	clip, text string
	isEnding   bool
	runs       int
	counts     map[string]int // by class; hangup is split by reason
	asWanted   int
}

func (row *wrongNumberRow) add(r wrongNumberRun) {
	row.runs++
	key := r.class
	if r.class == wrongClassHangup {
		key = wrongClassHangup + "/" + r.reason
	}
	row.counts[key]++
	isHangup := r.class == wrongClassHangup || r.class == wrongClassWordsHangup
	switch {
	case row.isEnding && r.class == wrongClassHangup && r.reason == "CALLER_ASKED_TO_END":
		row.asWanted++
	case !row.isEnding && !isHangup && r.class != wrongClassNone:
		row.asWanted++
	}
}

// wrongNumberTable is the per-clip summary. "as wanted" is a bare hangup with
// CALLER_ASKED_TO_END for a clip that should end the call, and an answer that
// calls no hangup for one that must not.
func wrongNumberTable(rows []wrongNumberRow) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "clip\twant\tn\thangup ASKED_TO_END\thangup other reason\twords+hangup\twords\tother\tnone\tas wanted\tsaid")
	for _, row := range rows {
		want := "end"
		if !row.isEnding {
			want = "MUST NOT END"
		}
		other := 0
		for key, n := range row.counts {
			if strings.HasPrefix(key, wrongClassHangup+"/") && key != wrongClassHangup+"/CALLER_ASKED_TO_END" {
				other += n
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d/%d\t%s\n", row.clip, want, row.runs,
			row.counts[wrongClassHangup+"/CALLER_ASKED_TO_END"], other, row.counts[wrongClassWordsHangup],
			row.counts[wrongClassWords], row.counts[wrongClassOther], row.counts[wrongClassNone],
			row.asWanted, row.runs, row.text)
	}
	_ = w.Flush()
	return b.String()
}
