// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real provider, what a turn does after the caller
// cut the opening line short (#360): on qwen the model recorded the stance and
// hung up in one response, and the turn answering both results said the whole
// opening line again before the closing line. It costs money, so it is
// skipped unless AICC_LIVE_PROVIDER_TEST and the profile's key are set.
//
// testdata/cut_opener.json is the campaign_lead_followup flow as flow.Runtime
// renders it for a campaign call (the session's instructions, opening line and
// tools, the answers to record_lead and hangup, the closing line riding in
// hangup's answer as answerToolCall does on a Realtime profile), written by
// flow.TestRenderCutOpenerSession. testdata/q_stop_*.pcm ("Stop calling.",
// "别再打了。") and q_driving_*.pcm ("Sorry, I'm driving.", "我在开车。") are the
// caller (macOS say, voices Samantha and Tingting, 16 kHz mono PCM16).
//
// One session per run, driven as the orchestrator and aicall.Session drive a
// call: the opening turn is consumed; at the cell's offset the caller speaks;
// qwen's own turn detection takes the turn, and the first speech_started gets
// Interrupt(SPEECH, playedMs) with the time played so far, as the barge-in
// does; record_lead and hangup are answered as the orchestrator answers them
// (hangup moves to the closing phase: instructions re-pinned, the closing line
// in the result).
//
// The client's frames pass through a local proxy (wireProxy) that logs the
// provider's events and carries the treatments:
//
//	main     the client as it was before #360: the proxy swallows the client's
//	         delete of the opening direction, so the item stays (what the
//	         fix removed)
//	shipped  the client untouched: it deletes the opening direction when the
//	         opening turn ends (removeOpeningDirection)
//	T2       main, and the cut opener's assistant item replaced by the part the
//	         caller heard (a history edit the client does not make)
//	T3       shipped and T2
//
// Cells are subtests: <language>/<treatment>/<shape>@<offset ms>. Shapes: stop
// ("Stop calling."), stop2 (a second voice and a half word: "Do... stop
// calling."), bye (a stance and a goodbye in one breath), drive (a plain
// reply that does not ask who is calling), who ("Who is this?": repeating the
// opener is acceptable there, losing who is calling is not), control (the
// caller waits for the opener to be heard whole; offset ignored).
//
// Every run also records whether the opener was said in turn 1 (the
// coverage) and whether the provider echoed the deletion of the opening
// direction (conversation.item.deleted).
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run 'TestLiveQwenCutOpener/en/shipped/stop@2700' -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 5) is how many sessions a cell opens. AICC_LIVE_WIRE=1
// prints the provider's events of every run, not only the first of a cell.
func TestLiveQwenCutOpener(t *testing.T) {
	profile := QwenProfile()
	requireLive(t, profile)
	data, err := os.ReadFile("testdata/cut_opener.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]cutOpenerSession
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	runs := 5
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	type cell struct {
		treatment, shape string
		offsetMs         int
	}
	// The opener is 12.2 s of audio in en and 10.7 s in zh (control runs); late
	// is its last second.
	late := map[string]int{"en": 11500, "zh": 9800}
	for _, language := range []string{"en", "zh"} {
		fx := fixtures[language]
		var cells []cell
		for _, treatment := range []string{"main", "shipped", "T2", "T3"} {
			for _, shape := range []string{"stop", "stop2", "bye", "drive", "who"} {
				cells = append(cells, cell{treatment, shape, 2700})
			}
			for _, offset := range []int{1000, 5000, late[language]} {
				cells = append(cells, cell{treatment, "stop", offset})
			}
		}
		// The control: the caller waits for the opener to be heard whole.
		cells = append(cells, cell{"main", "control", 0}, cell{"shipped", "control", 0})
		for _, c := range cells {
			name := fmt.Sprintf("%s/%s/%s@%d", language, c.treatment, c.shape, c.offsetMs)
			t.Run(name, func(t *testing.T) {
				counts := map[string]int{}
				var said, echoed, disclosed int
				for run := 1; run <= runs; run++ {
					r := runCutOpener(t, profile, language, fx, c.treatment, c.shape, c.offsetMs, run == 1)
					counts[r.class]++
					if r.isOpenerSaid {
						said++
					}
					if r.isDeleteEchoed {
						echoed++
					}
					if r.isDisclosed {
						disclosed++
					}
					t.Logf("[%s #%d] class=%s playedMs=%d openerCov=%.2f closingCov=%.2f openerSaid=%v deleteEchoed=%v disclosed=%v calls=%v turns=%d\n%s",
						name, run, r.class, r.playedMs, r.openerCov, r.closeCov, r.isOpenerSaid, r.isDeleteEchoed, r.isDisclosed,
						r.calls, len(r.turns), r.describe())
				}
				t.Logf("[%s] RESULT n=%d repeat=%d closing=%d other=%d who-repeat=%d who-identified=%d who-lost=%d openerSaid=%d deleteEchoed=%d disclosed=%d",
					name, runs, counts["repeat"], counts["closing"], counts["other"],
					counts["who-repeat"], counts["who-identified"], counts["who-lost"], said, echoed, disclosed)
			})
		}
	}
}

// cutOpenerSession is one language of testdata/cut_opener.json.
type cutOpenerSession struct {
	Instructions       string     `json:"instructions"`
	Opening            string     `json:"opening"`
	Tools              []ToolSpec `json:"tools"`
	Temperature        *float64   `json:"temperature,omitempty"`
	RecordOutput       string     `json:"recordOutput"`
	HangupOutput       string     `json:"hangupOutput"`
	FinishInstructions string     `json:"finishInstructions"`
	ClosingLine        string     `json:"closingLine"`
}

type cutTurn struct {
	text       string
	tools      []string
	audioMs    int
	end        string // the event type that ended it
	isAfterCut bool   // started after the caller took the floor
	isAfterRes bool   // started after a tool result was sent
}

type cutOpenerRun struct {
	class     string
	turns     []cutTurn
	calls     []string
	playedMs  int
	openerCov float64
	closeCov  float64
	wire      []string
	// isOpenerSaid: turn 1 carried the opener (its transcript covers most of
	// it). isDeleteEchoed: the provider echoed the deletion of the opening
	// direction.
	isOpenerSaid   bool
	isDeleteEchoed bool
	// isDisclosed (the "who" shape): a reply after the cut says it is an AI.
	isDisclosed bool
}

func (r *cutOpenerRun) describe() string {
	var b strings.Builder
	for i, turn := range r.turns {
		fmt.Fprintf(&b, "    turn %d (afterCut=%v afterResult=%v audio=%dms end=%s tools=%v): %q\n",
			i+1, turn.isAfterCut, turn.isAfterRes, turn.audioMs, turn.end, turn.tools, turn.text)
	}
	return b.String()
}

func runCutOpener(t *testing.T, profile Profile, language string, fx cutOpenerSession,
	treatment, shape string, offsetMs int, isFirst bool) *cutOpenerRun {
	t.Helper()
	input, output := profile.FormatsFor(media.LawMu)
	proxy := startWireProxy(t, profile, treatment, fx.Opening, output)
	defer proxy.close()
	profile.Endpoint = proxy.endpoint()

	clip := map[string]string{"stop": "q_stop_", "control": "q_stop_", "stop2": "q_stop2_",
		"bye": "q_bye_", "drive": "q_driving_", "who": "q_who_"}[shape] + language
	var caller []byte
	{
		caller = readMidClip(t, input, clip)
	}
	c := openLiveCall(t, profile, slog.New(&liveLogTap{}), SessionConfig{
		Instructions: fx.Instructions,
		OpeningText:  fx.Opening,
		Language:     language,
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
		Tools:        fx.Tools,
		Temperature:  fx.Temperature,
	})
	defer c.close(t)

	r := &cutOpenerRun{}
	var (
		firstAudio     time.Time
		openerBytes    int
		speakAt        <-chan time.Time
		quietAt        <-chan time.Time
		isSpoke, isCut bool
		isResultSent   bool
		turn           *cutTurn
		bytesPerMs     = output.RateHz * 2 / 1000
		deadline       = time.After(75 * time.Second)
	)
loop:
	for {
		select {
		case <-deadline:
			t.Logf("no end within 75 s")
			break loop
		case <-quietAt:
			break loop
		case <-speakAt:
			speakAt = nil
			isSpoke = true
			c.say(caller)
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				quietAt = nil
				r.turns = append(r.turns, cutTurn{isAfterCut: isCut, isAfterRes: isResultSent})
				turn = &r.turns[len(r.turns)-1]
			case EventTypeAudioDelta:
				if turn == nil {
					break
				}
				turn.audioMs += len(ev.Audio) / bytesPerMs
				if firstAudio.IsZero() {
					firstAudio = time.Now()
					if shape != "control" {
						speakAt = time.After(time.Duration(offsetMs-200) * time.Millisecond)
					}
				}
				if !isCut {
					openerBytes += len(ev.Audio)
				}
			case EventTypeOutputTranscript:
				if ev.IsFinal && turn != nil {
					turn.text += ev.Text
				}
			case EventTypeSpeechStarted:
				if isSpoke && !isCut {
					isCut = true
					if shape == "control" {
						break
					}
					played := int(time.Since(firstAudio) / time.Millisecond)
					if total := openerBytes / bytesPerMs; played > total {
						played = total
					}
					r.playedMs = played
					if err := c.session.Interrupt(InterruptReasonSpeech, played); err != nil {
						t.Fatalf("interrupt: %v", err)
					}
				}
			case EventTypeToolCall:
				r.calls = append(r.calls, ev.ToolName)
				if turn != nil {
					turn.tools = append(turn.tools, ev.ToolName)
				}
				var result string
				switch ev.ToolName {
				case "record_lead":
					result = fx.RecordOutput
				case "hangup":
					if err := c.session.UpdateInstructions(fx.FinishInstructions); err != nil {
						t.Fatalf("update instructions: %v", err)
					}
					result = fx.HangupOutput
				default:
					result = `{"ok":"1"}`
				}
				isResultSent = true
				if err := c.session.SendToolResult(ev.ToolCallID, result); err != nil {
					t.Fatalf("send tool result: %v", err)
				}
			case EventTypeResponseDone, EventTypeInterrupted:
				if turn != nil {
					turn.end = string(ev.Type)
				}
				if shape == "control" && !isSpoke && openerBytes > 0 {
					// The caller waits for the opener to be heard whole, then
					// answers it: no interruption.
					total := time.Duration(openerBytes/bytesPerMs) * time.Millisecond
					speakAt = time.After(max(0, total-time.Since(firstAudio)+300*time.Millisecond))
				}
				switch {
				case isCut:
					// The turn the results asked for ends the run; so does a
					// turn that called nothing. A turn that called tools is
					// followed by the results' turn within a second or two.
					if turn != nil && (turn.isAfterRes || len(turn.tools) == 0) {
						quietAt = time.After(3 * time.Second)
					} else {
						quietAt = time.After(10 * time.Second)
					}
				}
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
			}
		}
	}
	r.wire = proxy.lines()
	if isFirst || os.Getenv("AICC_LIVE_WIRE") != "" {
		for _, line := range r.wire {
			t.Logf("wire: %s", line)
		}
	}
	r.classify(fx, shape, language)
	r.isDeleteEchoed = proxy.isDeleteEchoed()
	return r
}

func turnHasCalls(turns []cutTurn) bool {
	for _, turn := range turns {
		if len(turn.tools) > 0 {
			return true
		}
	}
	return false
}

// classify reads the turns the caller's interruption led to. The opener is
// repeated when a turn after the cut carries most of its words; the closing
// line is said when a turn after it carries most of the closing line's.
//
// For the "who" shape the caller asked who is calling: repeating the opener is
// acceptable there, so the classes are whether the turns after the cut
// identified the company or the assistant (the opener's own words count) or
// lost who is calling.
func (r *cutOpenerRun) classify(fx cutOpenerSession, shape, language string) {
	r.class = "other"
	if len(r.turns) > 0 {
		r.isOpenerSaid = coverage(r.turns[0].text, fx.Opening) >= 0.5
	}
	if shape == "who" {
		identity := map[string][]string{"en": {"starcom", "voice assistant"}, "zh": {"星讯", "助手"}}[language]
		r.class = "who-lost"
		for _, turn := range r.turns {
			if !turn.isAfterCut {
				continue
			}
			r.openerCov = max(r.openerCov, coverage(turn.text, fx.Opening))
			if saysItIsAI(turn.text) {
				r.isDisclosed = true
			}
			for _, word := range identity {
				if strings.Contains(strings.ToLower(turn.text), word) {
					r.class = "who-identified"
				}
			}
		}
		if r.class == "who-identified" && r.openerCov >= 0.5 {
			r.class = "who-repeat"
		}
		return
	}
	for _, turn := range r.turns {
		if !turn.isAfterCut {
			continue
		}
		open := coverage(turn.text, fx.Opening)
		closing := coverage(turn.text, fx.ClosingLine)
		r.openerCov, r.closeCov = max(r.openerCov, open), max(r.closeCov, closing)
		if open >= 0.5 {
			r.class = "repeat"
		}
	}
	if r.class != "repeat" && r.closeCov >= 0.6 {
		r.class = "closing"
	}
}

// saysItIsAI reports whether text says the speaker is an AI.
func saysItIsAI(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range []string{"ai ", "ai,", "ai.", "artificial intelligence", "人工智能", "ai\u00a0"} {
		if strings.Contains(lower+" ", word) {
			return true
		}
	}
	return strings.Contains(lower, "ai助手") || strings.Contains(lower, "ai语音")
}

// coverage is the share of ref's word n-grams (character 4-grams for text
// without spaces) that text carries.
func coverage(text, ref string) float64 {
	grams := func(s string) map[string]bool {
		var words []string
		if strings.Count(s, " ") > 3 {
			words = strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
			})
		} else {
			for _, r := range s {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					words = append(words, string(r))
				}
			}
		}
		n := 3
		if len(words) > 0 && len([]rune(words[0])) == 1 && !strings.Contains(s, " ") {
			n = 4
		}
		out := map[string]bool{}
		for i := 0; i+n <= len(words); i++ {
			out[strings.Join(words[i:i+n], " ")] = true
		}
		return out
	}
	want, have := grams(ref), grams(text)
	if len(want) == 0 {
		return 0
	}
	hit := 0
	for g := range want {
		if have[g] {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

// wireProxy sits between the client and the provider: it relays every frame,
// logs the provider's events, and applies a treatment to the history.
type wireProxy struct {
	t         *testing.T
	ln        net.Listener
	srv       *http.Server
	upstream  string
	treatment string
	opening   string
	bytesMs   int
	started   time.Time

	mu           sync.Mutex
	log          []string
	up           *websocket.Conn // to the provider
	down         *websocket.Conn // to the client
	upMu, downMu sync.Mutex

	directionID string // the first caller item: the opening direction
	prevOf      map[string]string
	audioBytes  int // of the first response
	firstRespID string
	isFirstDone bool
	firstDoneCh chan struct{}
	isEchoed    bool
}

// isSwallowing says the treatment reproduces the client as it was before #360:
// its delete of the opening direction does not reach the provider.
func (p *wireProxy) isSwallowing() bool { return p.treatment == "main" || p.treatment == "T2" }

func (p *wireProxy) isDeleteEchoed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isEchoed
}

func startWireProxy(t *testing.T, profile Profile, treatment, opening string, output media.AudioFormat) *wireProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &wireProxy{t: t, ln: ln, upstream: profile.Endpoint, treatment: treatment, opening: opening,
		bytesMs: output.RateHz * 2 / 1000, started: time.Now(),
		prevOf: map[string]string{}, firstDoneCh: make(chan struct{})}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serve)}
	go func() { _ = p.srv.Serve(ln) }()
	return p
}

func (p *wireProxy) endpoint() string { return "ws://" + p.ln.Addr().String() + "/realtime" }

func (p *wireProxy) close() {
	_ = p.srv.Close()
}

func (p *wireProxy) lines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.log...)
}

func (p *wireProxy) note(format string, args ...any) {
	p.mu.Lock()
	p.log = append(p.log, fmt.Sprintf("+%6dms ", time.Since(p.started).Milliseconds())+fmt.Sprintf(format, args...))
	p.mu.Unlock()
}

func (p *wireProxy) sendUp(data []byte) {
	p.upMu.Lock()
	defer p.upMu.Unlock()
	_ = p.up.WriteMessage(websocket.TextMessage, data)
}

func (p *wireProxy) injectUp(event map[string]any) {
	data, _ := json.Marshal(event)
	p.note("INJECT %s", abbreviate(string(data)))
	p.sendUp(data)
}

func abbreviate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func (p *wireProxy) serve(w http.ResponseWriter, req *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	down, err := up.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	target, _ := url.Parse(p.upstream)
	target.RawQuery = req.URL.RawQuery
	headers := http.Header{}
	headers.Set("Authorization", req.Header.Get("Authorization"))
	var upstream *websocket.Conn
	for attempt := 0; attempt < 3; attempt++ {
		upstream, _, err = websocket.DefaultDialer.Dial(target.String(), headers)
		if err == nil {
			break
		}
		p.note("dial upstream (attempt %d): %v", attempt+1, err)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		p.note("dial upstream: %v", err)
		_ = down.Close()
		return
	}
	p.up, p.down = upstream, down
	go func() {
		defer func() { _ = down.Close(); _ = upstream.Close() }()
		for {
			mt, data, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				p.onUpstream(data)
			}
			p.downMu.Lock()
			err = down.WriteMessage(mt, data)
			p.downMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	defer func() { _ = down.Close(); _ = upstream.Close() }()
	for {
		mt, data, err := down.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.TextMessage && !p.onDownstream(data) {
			continue
		}
		p.upMu.Lock()
		err = upstream.WriteMessage(mt, data)
		p.upMu.Unlock()
		if err != nil {
			return
		}
	}
}

// onDownstream sees a frame from the client and reports whether it goes on to
// the provider.
func (p *wireProxy) onDownstream(data []byte) (isForwarded bool) {
	var ev map[string]any
	if json.Unmarshal(data, &ev) != nil {
		return true
	}
	switch ev["type"] {
	case "input_audio_buffer.append":
		return true
	case "conversation.item.truncate":
		p.note("CLIENT %s", string(data))
		if p.treatment == "T2" || p.treatment == "T3" {
			id, _ := ev["item_id"].(string)
			ms, _ := ev["audio_end_ms"].(float64)
			go p.rewriteOpener(id, int(ms))
		}
		return true
	case "conversation.item.delete":
		id, _ := ev["item_id"].(string)
		p.mu.Lock()
		isDirection := id != "" && id == p.directionID
		p.mu.Unlock()
		if isDirection && p.isSwallowing() {
			p.note("SWALLOWED %s", string(data))
			return false
		}
	}
	p.note("CLIENT %s", abbreviate(string(data)))
	return true
}

// rewriteOpener replaces the cut opener's assistant item with the part the
// caller heard, once the opening response is over.
func (p *wireProxy) rewriteOpener(itemID string, playedMs int) {
	select {
	case <-p.firstDoneCh:
	case <-time.After(20 * time.Second):
		p.note("rewrite: the opening response never ended")
		return
	}
	p.mu.Lock()
	total := p.audioBytes / p.bytesMs
	previous := p.prevOf[itemID]
	p.mu.Unlock()
	heard := p.opening
	if total > 0 && playedMs < total {
		heard = heardPrefix(p.opening, float64(playedMs)/float64(total))
	}
	p.note("rewrite: item %s previous=%q played=%dms of %dms, heard %q", itemID, previous, playedMs, total, heard)
	create := map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "message", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": heard}}}}
	if previous != "" {
		create["previous_item_id"] = previous
	}
	p.injectUp(create)
	p.injectUp(map[string]any{"type": "conversation.item.delete", "item_id": itemID})
}

// heardPrefix is the part of text a listener has heard after ratio of its
// audio, cut at a word (or, without spaces, a character).
func heardPrefix(text string, ratio float64) string {
	runes := []rune(text)
	n := int(float64(len(runes)) * ratio)
	if n <= 0 {
		return ""
	}
	if n >= len(runes) {
		return text
	}
	if strings.Contains(text, " ") {
		for n > 0 && runes[n] != ' ' {
			n--
		}
	}
	return strings.TrimSpace(string(runes[:n]))
}

// onUpstream sees an event from the provider.
func (p *wireProxy) onUpstream(data []byte) {
	var ev struct {
		Type           string `json:"type"`
		Delta          string `json:"delta"`
		ResponseID     string `json:"response_id"`
		PreviousItemID string `json:"previous_item_id"`
		ItemID         string `json:"item_id"`
		Transcript     string `json:"transcript"`
		Item           *struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Type    string `json:"type"`
			Content []struct {
				Text       string `json:"text"`
				Transcript string `json:"transcript"`
			} `json:"content"`
		} `json:"item"`
		Response *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch {
	case strings.Contains(ev.Type, "audio.delta") && !strings.Contains(ev.Type, "transcript"):
		raw, _ := base64.StdEncoding.DecodeString(ev.Delta)
		p.mu.Lock()
		if p.firstRespID == "" || ev.ResponseID == p.firstRespID {
			p.firstRespID = ev.ResponseID
			p.audioBytes += len(raw)
		}
		p.mu.Unlock()
		return
	case strings.Contains(ev.Type, "transcript.delta"), strings.Contains(ev.Type, "input_audio_buffer.committed"):
		return
	case ev.Type == "conversation.item.created" || ev.Type == "conversation.item.added":
		if ev.Item != nil {
			p.mu.Lock()
			p.prevOf[ev.Item.ID] = ev.PreviousItemID
			isDirection := p.directionID == "" && ev.Item.Role == "user"
			if isDirection {
				p.directionID = ev.Item.ID
			}
			p.mu.Unlock()
			text := ""
			if len(ev.Item.Content) > 0 {
				text = ev.Item.Content[0].Text + ev.Item.Content[0].Transcript
			}
			p.note("PROVIDER item.created id=%s previous=%s role=%s type=%s direction=%v text=%q",
				ev.Item.ID, ev.PreviousItemID, ev.Item.Role, ev.Item.Type, isDirection, abbreviate(text))
		}
		return
	case ev.Type == "response.created":
		id := ""
		if ev.Response != nil {
			id = ev.Response.ID
		}
		p.mu.Lock()
		if p.firstRespID == "" {
			p.firstRespID = id
		}
		p.mu.Unlock()
		p.note("PROVIDER response.created id=%s", id)
		return
	case ev.Type == "response.done":
		id, status := "", ""
		if ev.Response != nil {
			id, status = ev.Response.ID, ev.Response.Status
		}
		p.note("PROVIDER response.done id=%s status=%s audio=%dms", id, status, p.audioBytes/p.bytesMs)
		p.mu.Lock()
		first := !p.isFirstDone && id == p.firstRespID
		if first {
			p.isFirstDone = true
		}
		p.mu.Unlock()
		if first {
			close(p.firstDoneCh)
		}
		return
	case strings.Contains(ev.Type, "transcript.done"):
		p.note("PROVIDER %s item=%s %q", ev.Type, ev.ItemID, ev.Transcript)
		return
	}
	if ev.Type == "conversation.item.deleted" {
		p.mu.Lock()
		if ev.ItemID != "" && ev.ItemID == p.directionID {
			p.isEchoed = true
		}
		p.mu.Unlock()
	}
	switch ev.Type {
	case "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "conversation.item.deleted",
		"conversation.item.truncated", "error", "response.output_item.added", "response.function_call_arguments.done":
		p.note("PROVIDER %s", abbreviate(string(data)))
	}
}
