// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// uplinkRecorder is a model that only counts what the uplink clock feeds it.
// Real frames carry their sequence number in the first two bytes (visible
// only on the G.711 passthrough path, where the bytes arrive untouched); a
// fill is told from a real frame by content.
type uplinkRecorder struct {
	provider.VoiceSession
	silence []byte
	sent    int
	fills   int
	real    []int // sequence numbers of real frames, passthrough only
	isPass  bool
}

func (m *uplinkRecorder) SendAudio(audio []byte) error {
	m.sent++
	if isAllSilence(audio, m.silence) {
		m.fills++
		return nil
	}
	if m.isPass {
		m.real = append(m.real, int(audio[0])<<8|int(audio[1]))
	}
	return nil
}

func isAllSilence(audio, silence []byte) bool { return bytes.Equal(audio, silence) }

// loudFrame is 20 ms of a sample that is far from silence in either law (0xFF
// and 0xD5 are the idle bytes) and not zero once converted to linear audio.
var loudFrame = bytes.Repeat([]byte{0x10}, media.FrameSamples)

// realFrame builds a caller frame that is never silence in any format. It is
// a plain allocation, not a pooled buffer: a scenario builds 30,000 of them,
// and sync.Pool under -race drops a quarter of what it is given, so pooled
// buffers of 8 KiB would turn the run into an allocator benchmark.
func realFrame(_ media.Law, seq int) []byte {
	frame := append(make([]byte, 0, media.FrameSamples), loudFrame...)
	frame[0], frame[1] = byte(seq>>8), byte(seq)
	return frame
}

type uplinkFormat struct {
	name    string
	law     media.Law
	profile provider.Profile
	isPass  bool
}

var uplinkFormats = []uplinkFormat{
	{"PCMU passthrough", media.LawMu, provider.OpenAIProfile(), true},
	{"PCMA passthrough", media.LawAlaw, provider.OpenAIProfile(), true},
	{"PCMU to PCM16", media.LawMu, provider.QwenProfile(), false},
	{"PCMA to PCM16", media.LawAlaw, provider.QwenProfile(), false},
}

func newUplinkSession(t *testing.T, f uplinkFormat) (*Session, *uplinkRecorder) {
	t.Helper()
	rec := &uplinkRecorder{isPass: f.isPass}
	session, err := New(newFakeLeg(f.law), rec, f.profile, Config{})
	if err != nil {
		t.Fatal(err)
	}
	rec.silence = session.uplinkSilence
	return session, rec
}

type arrival struct {
	at  time.Duration
	seq int
}

// uplinkRun plays arrivals (in the order they arrive) into a session's clock
// against the wake-up times tickAt returns, in virtual time, as the pump
// would: the frames that have arrived are queued, then the slots that have
// elapsed are sent.
type uplinkRun struct {
	session *Session
	rec     *uplinkRecorder
	clock   *uplinkClock
	last    time.Time
}

func runUplink(t *testing.T, f uplinkFormat, total time.Duration, arrivals []arrival,
	tickAt func(k int) time.Duration, hushedFrom, hushedTo time.Duration) *uplinkRun {

	t.Helper()
	session, rec := newUplinkSession(t, f)
	epoch := time.Unix(1_000_000, 0)
	run := &uplinkRun{session: session, rec: rec, clock: newUplinkClock(epoch)}
	converted := make([]byte, 0, media.FrameSamples*8)
	next := 0
	for k := 1; ; k++ {
		at := tickAt(k)
		if at > total {
			break
		}
		for next < len(arrivals) && arrivals[next].at <= at {
			run.clock.push(realFrame(f.law, arrivals[next].seq))
			if run.clock.queued() > uplinkMaxBacklog {
				t.Fatalf("backlog %d passed the bound", run.clock.queued())
			}
			next++
		}
		session.hush(hushedTo > hushedFrom && at >= hushedFrom && at < hushedTo)
		var err error
		if converted, err = session.sendDueSlots(run.clock, epoch.Add(at), converted); err != nil {
			t.Fatal(err)
		}
		if epoch.Add(at).After(run.last) {
			run.last = epoch.Add(at)
		}
	}
	return run
}

func steadyTicks(k int) time.Duration { return time.Duration(k) * uplinkSlot }

func (r *uplinkRun) check(t *testing.T, f uplinkFormat, arrived []arrival, hushedSlots int64) {
	t.Helper()
	st := r.clock.stats
	slots := int64(r.last.Sub(r.clock.start) / uplinkSlot)

	if want := slots - hushedSlots; st.Sent < want-1 || st.Sent > want+1 {
		// Hushed slots send nothing; every other slot sends exactly one.
		t.Errorf("slots: sent %d, want %d +-1 (%d elapsed, %d hushed)", st.Sent, want, slots, hushedSlots)
	}
	if int64(r.rec.sent) != st.Sent || int64(r.rec.fills) != st.Filled {
		t.Errorf("recorder saw %d sent %d filled, clock counted %d / %d",
			r.rec.sent, r.rec.fills, st.Sent, st.Filled)
	}
	if st.MaxBacklog > uplinkMaxBacklog {
		t.Errorf("max backlog %d, bound %d", st.MaxBacklog, uplinkMaxBacklog)
	}
	// received = sent - filled + dropped (+ hushed) + still queued.
	want := st.Sent - st.Filled + st.DroppedBacklog + st.DroppedHushed + int64(r.clock.queued())
	if st.Received != want {
		t.Errorf("counters do not reconcile: received %d, sent-filled+dropped+hushed+queued = %d (%+v)",
			st.Received, want, st)
	}
	if f.isPass && !isSubsequence(r.rec.real, arrived) {
		// Real frames leave in the order they arrived: the sent sequence is a
		// subsequence of the arrival sequence.
		t.Error("real frames left out of the order they arrived in")
	}
}

const tenMinutes = 10 * time.Minute

// steady builds frames every period from 0 to total, skipping the gaps.
func steady(total, period time.Duration, gaps func(at time.Duration) bool) []arrival {
	var out []arrival
	seq := 0
	for at := period; at <= total; at += period {
		if gaps != nil && gaps(at) {
			continue
		}
		out = append(out, arrival{at, seq})
		seq++
	}
	return out
}

// clockRun is what a bare uplinkClock did with a stream: the clock logic with
// none of the session around it, cheap enough to run a long stream in every
// scenario. Nothing in it depends on the audio format.
type clockRun struct {
	sent, filled int64
	// leftIn is the real frames in the order they left, by sequence number.
	leftIn []int
	slots  int64
	clock  *uplinkClock
}

// driveClock plays arrivals into a clock and takes its slots at the wake-up
// times tickAt returns, as the pump does. Frames are two-byte tokens holding
// the sequence number, carved from one allocation.
func driveClock(t *testing.T, total time.Duration, arrivals []arrival, tickAt func(k int) time.Duration) *clockRun {
	t.Helper()
	epoch := time.Unix(1_000_000, 0)
	run := &clockRun{clock: newUplinkClock(epoch)}
	tokens := make([]byte, 2*len(arrivals))

	var last time.Duration
	next := 0
	for k := 1; ; k++ {
		at := tickAt(k)
		if at > total {
			break
		}
		for ; next < len(arrivals) && arrivals[next].at <= at; next++ {
			token := tokens[2*next : 2*next+2 : 2*next+2]
			token[0], token[1] = byte(arrivals[next].seq>>8), byte(arrivals[next].seq)
			run.clock.push(token)
			if run.clock.queued() > uplinkMaxBacklog {
				t.Fatalf("backlog %d passed the bound", run.clock.queued())
			}
		}
		for n := run.clock.owed(epoch.Add(at)); n > 0; n-- {
			run.sent++
			if token := run.clock.pop(); token != nil {
				run.leftIn = append(run.leftIn, int(token[0])<<8|int(token[1]))
			} else {
				run.filled++
			}
		}
		last = max(last, at)
	}
	run.slots = int64(last / uplinkSlot)
	return run
}

// sorted returns arrivals in the order they reach the clock.
func sorted(arrivals []arrival) []arrival {
	out := slices.Clone(arrivals)
	slices.SortStableFunc(out, func(a, b arrival) int { return int(a.at - b.at) })
	return out
}

// isSubsequence reports whether sent leaves in the order it arrived.
func isSubsequence(sent []int, arrived []arrival) bool {
	j := 0
	for _, seq := range sent {
		for j < len(arrived) && arrived[j].seq != seq {
			j++
		}
		if j == len(arrived) {
			return false
		}
		j++
	}
	return true
}

func TestTheUplinkClockIsOneFramePerSlotWhateverTheCallerDoes(t *testing.T) {
	type scenario struct {
		name  string
		build func() []arrival
		ticks func(k int) time.Duration
		// pause, when set, is the caller's silence every 10 s.
		pause time.Duration
		// minFilled and minDropped are what a sender off the clock must cost:
		// a slow one is filled for, a fast one is dropped, and neither is
		// ever answered with more or fewer slots than elapsed.
		minFilled, minDropped int64
	}
	pauseEvery := func(pause time.Duration) func() []arrival {
		return func() []arrival {
			return steady(tenMinutes, uplinkSlot, func(at time.Duration) bool {
				return at%(10*time.Second) < pause
			})
		}
	}

	scenarios := []scenario{
		{name: "steady 20 ms", build: func() []arrival { return steady(tenMinutes, uplinkSlot, nil) }, ticks: steadyTicks},
		{name: "pause 40 ms", build: pauseEvery(40 * time.Millisecond), ticks: steadyTicks, pause: 40 * time.Millisecond},
		{name: "pause 300 ms", build: pauseEvery(300 * time.Millisecond), ticks: steadyTicks, pause: 300 * time.Millisecond},
		{name: "pause 1 s", build: pauseEvery(time.Second), ticks: steadyTicks, pause: time.Second},
		{name: "pause 4.9 s", build: pauseEvery(4900 * time.Millisecond), ticks: steadyTicks, pause: 4900 * time.Millisecond},
		{name: "2 s pause then 100 frames at once", ticks: steadyTicks, build: func() []arrival {
			var out []arrival
			seq := 0
			for base := time.Duration(0); base+32*time.Second <= tenMinutes; base += 32 * time.Second {
				for at := base + uplinkSlot; at <= base+30*time.Second; at += uplinkSlot {
					out = append(out, arrival{at, seq})
					seq++
				}
				// Two seconds of nothing, then the burst.
				for range 100 {
					out = append(out, arrival{base + 32*time.Second, seq})
					seq++
				}
			}
			return out
		}},
		{name: "+-40 ms jitter, reorder, 1% loss", ticks: steadyTicks, build: jitteryStream},
		{name: "silence suppression, jitter, a held-up burst", ticks: steadyTicks, build: suppressedStream},
		{name: "sender 1% fast", ticks: steadyTicks, minDropped: 250,
			build: func() []arrival { return steady(tenMinutes, 19800*time.Microsecond, nil) }},
		{name: "sender 1% slow", ticks: steadyTicks, minFilled: 250,
			build: func() []arrival { return steady(tenMinutes, 20200*time.Microsecond, nil) }},
		{name: "late wake-ups", build: func() []arrival { return steady(tenMinutes, uplinkSlot, nil) },
			// Every 50th wake-up is 140 ms late; the ones after it catch up.
			ticks: func(k int) time.Duration {
				at := time.Duration(k) * uplinkSlot
				if k%50 == 0 {
					at += 140 * time.Millisecond
				}
				return at
			}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			arrived := sorted(sc.build())
			run := driveClock(t, tenMinutes, arrived, sc.ticks)
			st := run.clock.stats

			if run.sent < run.slots-1 || run.sent > run.slots+1 {
				t.Errorf("sent %d frames in %d elapsed slots", run.sent, run.slots)
			}
			if st.MaxBacklog > uplinkMaxBacklog {
				t.Errorf("backlog reached %d, bound %d", st.MaxBacklog, uplinkMaxBacklog)
			}
			if !isSubsequence(run.leftIn, arrived) {
				t.Error("real frames left out of the order they arrived in")
			}
			// received = real frames sent + dropped by the bound + still queued.
			if want := run.sent - run.filled + st.DroppedBacklog + int64(run.clock.queued()); st.Received != want {
				t.Errorf("counters do not reconcile: received %d, sent-filled+dropped+queued = %d", st.Received, want)
			}
			if run.filled < sc.minFilled || st.DroppedBacklog < sc.minDropped {
				t.Errorf("filled %d dropped %d, want at least %d and %d", run.filled, st.DroppedBacklog,
					sc.minFilled, sc.minDropped)
			}
			if sc.pause > 0 {
				// Each pause owes the provider its slots, whatever else happens.
				perPause := int64(sc.pause/uplinkSlot) - 1
				if want := int64(tenMinutes/(10*time.Second)) * perPause * 9 / 10; run.filled < want {
					t.Errorf("filled %d slots, want at least %d for a %v pause", run.filled, want, sc.pause)
				}
			}
		})
	}
}

// jitteryStream is a 20 ms stream with +-40 ms of jitter, so frames reorder,
// and 1% loss.
func jitteryStream() []arrival {
	r := rand.New(rand.NewSource(7))
	var out []arrival
	for i, at := 0, uplinkSlot; at <= tenMinutes; i, at = i+1, at+uplinkSlot {
		if r.Intn(100) == 0 {
			continue
		}
		jitter := time.Duration(r.Intn(81)-40) * time.Millisecond
		out = append(out, arrival{max(at+jitter, 0), i})
	}
	return out
}

// suppressedStream is a phone with silence suppression, the call this clock
// exists for: a second of speech every 30 s, with jitter and 1% loss, and
// nothing between. Every third burst is held up in the network and arrives
// at once, two seconds late.
func suppressedStream() []arrival {
	r := rand.New(rand.NewSource(11))
	var out []arrival
	seq := 0
	for burst, base := 0, time.Duration(0); base+30*time.Second <= tenMinutes; burst, base = burst+1, base+30*time.Second {
		for k := 1; k <= 50; k++ {
			at := base + 5*time.Second + time.Duration(k)*uplinkSlot
			seq++
			if r.Intn(100) == 0 {
				continue
			}
			if burst%3 == 2 {
				at = base + 7*time.Second
			} else {
				at += time.Duration(r.Intn(81)-40) * time.Millisecond
			}
			out = append(out, arrival{at, seq})
		}
	}
	return out
}

// Whatever the format, the session around the clock feeds the provider the
// same slots: ten minutes of a phone with silence suppression through each of
// them, with the provider's recorder counting what it was sent. Most of the
// slots are fills, which is the point; the frames that are real are converted.
func TestEveryInputFormatIsFedOneFramePerSlotForTenMinutes(t *testing.T) {
	for _, f := range uplinkFormats {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			arrived := sorted(suppressedStream())
			run := runUplink(t, f, tenMinutes, arrived, steadyTicks, 0, 0)
			run.check(t, f, arrived, 0)
			if run.clock.stats.Filled == 0 || run.clock.stats.DroppedBacklog == 0 {
				t.Errorf("the stream should have produced fills and drops: %+v", run.clock.stats)
			}
		})
	}
}

func TestAFillIsSilenceInTheProvidersFormat(t *testing.T) {
	for _, f := range uplinkFormats {
		t.Run(f.name, func(t *testing.T) {
			session, _ := newUplinkSession(t, f)
			fill := session.uplinkSilence
			// The length of a real frame after the call's own conversion.
			converted := session.uplink.Convert(nil, realFrame(f.law, 0))
			if len(fill) != len(converted) {
				t.Fatalf("fill is %d bytes, a converted frame is %d", len(fill), len(converted))
			}
			// The two laws' idle bytes, written out: 0xFF is mu-law zero, 0xD5 A-law.
			want := byte(0)
			if f.isPass {
				want = map[media.Law]byte{media.LawMu: 0xFF, media.LawAlaw: 0xD5}[f.law]
			}
			for i, b := range fill {
				if b != want {
					t.Fatalf("byte %d is %#x, want %#x", i, b, want)
				}
			}
		})
	}
}

func TestNothingIsSentWhileHushed(t *testing.T) {
	for _, f := range uplinkFormats {
		t.Run(f.name, func(t *testing.T) {
			arrivals := steady(time.Minute, uplinkSlot, func(at time.Duration) bool {
				return at > 20*time.Second && at < 30*time.Second // a quiet stretch inside the hush
			})
			run := runUplink(t, f, time.Minute, arrivals, steadyTicks, 15*time.Second, 35*time.Second)
			hushedSlots := int64((20 * time.Second) / uplinkSlot)
			run.check(t, f, arrivals, hushedSlots)

			slots := int64(time.Minute / uplinkSlot)
			if got := int64(run.rec.sent); got < slots-hushedSlots-1 || got > slots-hushedSlots+1 {
				t.Errorf("sent %d, want %d +-1: the hushed 20 s must send nothing, fills included",
					got, slots-hushedSlots)
			}
			if run.clock.stats.DroppedHushed == 0 {
				t.Error("frames that arrived while hushed were not counted as dropped")
			}
		})
	}
}

// After a hush ends, the slots that passed are not replayed as a burst.
func TestAHushDoesNotLeaveASlotDebt(t *testing.T) {
	f := uplinkFormats[0]
	run := runUplink(t, f, 10*time.Second, nil, steadyTicks, 2*time.Second, 8*time.Second)
	if want := int((4 * time.Second) / uplinkSlot); run.rec.sent < want-2 || run.rec.sent > want+2 {
		t.Errorf("sent %d, want about %d (2 s before and 2 s after the hush)", run.rec.sent, want)
	}
}

func TestALateFrameDoesNotPushBackLaterSpeech(t *testing.T) {
	// 2 s of nothing, then 100 frames at once: only the newest 100 ms is kept.
	f := uplinkFormats[0]
	var arrivals []arrival
	for i := 0; i < 100; i++ {
		arrivals = append(arrivals, arrival{2 * time.Second, i})
	}
	run := runUplink(t, f, 3*time.Second, arrivals, steadyTicks, 0, 0)
	run.check(t, f, arrivals, 0)
	if len(run.rec.real) != uplinkMaxBacklog {
		t.Fatalf("%d real frames sent, want the newest %d", len(run.rec.real), uplinkMaxBacklog)
	}
	if run.rec.real[0] != 100-uplinkMaxBacklog {
		t.Errorf("first kept frame %d, want %d: the oldest must be dropped", run.rec.real[0], 100-uplinkMaxBacklog)
	}
	if run.clock.stats.DroppedBacklog != 100-uplinkMaxBacklog {
		t.Errorf("backlog drops %d, want %d", run.clock.stats.DroppedBacklog, 100-uplinkMaxBacklog)
	}
}

//
// The pump itself, in virtual time.
//

// uplinkLine is the one "ai uplink clock" record the session wrote.
func uplinkLine(t *testing.T, logs *lockedBuffer) map[string]float64 {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if record["msg"] == "ai uplink clock" {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d \"ai uplink clock\" lines, want one per call", len(found))
	}
	out := map[string]float64{}
	for key, value := range found[0] {
		if number, ok := value.(float64); ok {
			out[key] = number
		}
	}
	return out
}

// virtualPump runs a Session's real pump on a clock the test owns: the pump
// wakes only when the test sends a tick and reads the time the test set. A
// scenario takes no wall time and has no timing in it to get wrong.
type virtualPump struct {
	t       *testing.T
	session *Session
	model   *fakeModel
	// frames is the fake leg's inbound queue; nil when the leg brings its own.
	frames chan []byte
	epoch  time.Time
	ticks  chan time.Time
	offset atomic.Int64
	at     time.Duration
	logs   *lockedBuffer
}

func startVirtualPump(t *testing.T, leg Leg, frames chan []byte, profile provider.Profile) *virtualPump {
	t.Helper()
	v := &virtualPump{
		t: t, model: newFakeModel(), frames: frames,
		epoch: time.Unix(1_000_000, 0), ticks: make(chan time.Time), logs: &lockedBuffer{},
	}
	session, err := New(leg, v.model, profile, Config{
		BargeGuard: -1, NoInput: -1, Logger: slog.New(slog.NewJSONHandler(v.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The pump starts its clock on its first read of the time. The test must
	// not move the time before that, or the clock would start late and lose
	// the slots in between.
	started := make(chan struct{})
	var startOnce sync.Once
	session.uplinkNow = func() time.Time {
		at := v.epoch.Add(time.Duration(v.offset.Load()))
		startOnce.Do(func() { close(started) })
		return at
	}
	session.uplinkTicks = v.ticks
	v.session = session
	if err := session.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the uplink pump never started its clock")
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	return v
}

// wake sends the pump one tick.
func (v *virtualPump) wake() {
	v.t.Helper()
	select {
	case v.ticks <- v.epoch.Add(v.at):
	case <-time.After(10 * time.Second):
		v.t.Fatal("the uplink pump did not take its tick")
	}
}

// advanceTo moves the clock to at (counted from the start) and wakes the pump.
// It wakes it a second time: the pump takes that one only after it has finished
// the first, so on return every slot the first owed has been sent.
func (v *virtualPump) advanceTo(at time.Duration) {
	v.t.Helper()
	v.offset.Store(int64(at))
	v.at = at
	v.wake()
	v.wake()
}

func (v *virtualPump) step(slots int) {
	v.t.Helper()
	for range slots {
		v.advanceTo(v.at + uplinkSlot)
	}
}

// feed hands the leg one frame per marker. It does not wait for the pump to
// take them: a tick takes whatever is waiting before it judges a slot empty.
func (v *virtualPump) feed(markers ...byte) {
	v.t.Helper()
	for _, marker := range markers {
		select {
		case v.frames <- callerFrame(media.LawMu, marker):
		case <-time.After(10 * time.Second):
			v.t.Fatal("the leg's queue stayed full")
		}
	}
}

const fill = -1

// heard is what the model was fed: a real frame's marker byte, or fill.
func (v *virtualPump) heard() []int {
	var out []int
	for _, frame := range v.model.receivedAudio() {
		if isAllSilence(frame, media.SilenceFrame(media.LawMu)) {
			out = append(out, fill)
			continue
		}
		out = append(out, int(frame[0]))
	}
	return out
}

func fills(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func markers(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

func (v *virtualPump) end() map[string]float64 {
	v.t.Helper()
	v.session.Close(context.Background())
	v.session.Wait()
	return uplinkLine(v.t, v.logs)
}

func TestThePumpFeedsTheModelOneFramePerSlotInVirtualTime(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	v := startVirtualPump(t, leg, leg.frames, provider.OpenAIProfile())

	// Three frames and then nothing: three real slots, then fills.
	v.feed(1, 2, 3)
	v.step(6)
	want := slices.Concat([]int{1, 2, 3}, fills(3))
	if got := v.heard(); !slices.Equal(got, want) {
		t.Fatalf("heard %v, want %v", got, want)
	}

	// A 2 s pause is 100 slots of fill, however the audio that follows arrives.
	v.step(100)
	want = slices.Concat(want, fills(100))
	if got := v.heard(); !slices.Equal(got, want) {
		t.Fatalf("after a 2 s pause: heard %d frames, want %d fills appended", len(got), 100)
	}

	// Then 100 frames at once: the newest 100 ms are kept, and they leave at
	// one per slot, not at once.
	var burst []byte
	for i := range 100 {
		burst = append(burst, byte(i))
	}
	v.feed(burst...)
	v.step(10)
	want = slices.Concat(want, markers(95, 100), fills(5))
	if got := v.heard(); !slices.Equal(got, want) {
		t.Fatalf("after the burst: heard %v, want the newest five frames then fills", got[len(got)-10:])
	}

	// A wake-up 140 ms late sends the seven slots it owes, no fewer.
	v.advanceTo(v.at + 140*time.Millisecond)
	want = slices.Concat(want, fills(7))
	if got := v.heard(); !slices.Equal(got, want) {
		t.Fatalf("after a late wake-up: heard %d frames, want %d", len(got), len(want))
	}

	leg.rxDropped.Store(7)
	line := v.end()
	for key, wantValue := range map[string]float64{
		"framesReceived":       103,
		"framesSent":           123, // 6 + 100 + 10 + 7 slots
		"framesFilled":         115,
		"framesDroppedBacklog": 95,
		"framesDroppedRx":      7,
		"framesDroppedHushed":  0,
		"framesQueued":         0,
		"maxBacklogFrames":     uplinkMaxBacklog,
	} {
		if line[key] != wantValue {
			t.Errorf("%s = %v, want %v", key, line[key], wantValue)
		}
	}
}

func TestThePumpSendsNothingWhileHushedAndOwesNothingAfterwards(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	v := startVirtualPump(t, leg, leg.frames, provider.OpenAIProfile())

	v.feed(1, 2)
	v.step(2)
	v.session.hush(true)
	v.feed(3, 4)
	v.step(50) // a second of transfer: no frame, no fill
	if got := v.heard(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("heard %v, want only the two frames before the hush", got)
	}

	v.session.hush(false)
	v.step(1)
	if got := v.heard(); !slices.Equal(got, []int{1, 2, fill}) {
		t.Fatalf("heard %v, want one fill after the hush: the hushed slots are not owed", got)
	}

	line := v.end()
	if line["framesReceived"] != 4 || line["framesSent"] != 3 || line["framesFilled"] != 1 ||
		line["framesDroppedHushed"] != 2 {
		t.Errorf("counters %v do not describe the hush", line)
	}
}

func TestAModelThatCannotBeFedFailsTheCallOnTheFirstFill(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	v := startVirtualPump(t, leg, leg.frames, provider.OpenAIProfile())

	v.model.failSends(errors.New("connection reset"))
	v.offset.Store(int64(uplinkSlot))
	v.at = uplinkSlot
	v.wake() // one tick: the pump fails the call and returns, so no barrier tick

	event := awaitBridgeEvent(t, v.session, EventTypeFailed)
	if event.Err == nil {
		t.Error("the failure carried no cause")
	}
	select {
	case <-leg.stopped:
	case <-time.After(5 * time.Second):
		t.Error("the telephone leg was left up after the model failed")
	}
}

// rtpLeg is a fake leg whose audio and media state come from a real RTP
// session: the state the dead-media watch reads (voice.UAS.watchForDeadMedia).
type rtpLeg struct {
	*fakeLeg
	rtp *voice.RTPSession
}

func (l rtpLeg) Frames() <-chan []byte         { return l.rtp.Frames() }
func (l rtpLeg) MediaStarted() <-chan struct{} { return l.rtp.MediaStarted() }
func (l rtpLeg) FirstMediaAt() time.Time       { return l.rtp.FirstMediaAt() }
func (l rtpLeg) RxDropped() int64              { return l.rtp.RxDropped() }

// Liveness reads the RTP session: HasReceived and LastPacketAt for "media went
// dead", MediaStarted and FirstMediaAt for "media never started" and "first
// media". A fill is made after the RTP session and never touches it, so it
// cannot keep a dead path alive.
func TestFillsDoNotFeedMediaLiveness(t *testing.T) {
	rtpSession := voice.NewRTPSession(0, media.LawMu, -1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := rtpSession.Start(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rtpSession.Stop)
	seed := rtpSession.LastPacketAt()
	leg := rtpLeg{newFakeLeg(media.LawMu), rtpSession}
	v := startVirtualPump(t, leg, nil, provider.OpenAIProfile())

	// A caller whose packets never arrive: ten seconds of slots, all filled.
	v.step(500)
	if got := v.heard(); !slices.Equal(got, fills(500)) {
		t.Fatalf("heard %d frames, want 500 fills", len(got))
	}
	select {
	case <-rtpSession.MediaStarted():
		t.Error("fills started the media")
	default:
	}
	if rtpSession.HasReceived() || !rtpSession.FirstMediaAt().IsZero() || !rtpSession.LastPacketAt().Equal(seed) {
		t.Errorf("fills moved the liveness the dead-media watch reads: received %v, first media %v, last packet %v (seed %v)",
			rtpSession.HasReceived(), rtpSession.FirstMediaAt(), rtpSession.LastPacketAt(), seed)
	}

	// The control: one real packet moves all of it, and its audio is heard in
	// place of a fill.
	payload := bytes.Repeat([]byte{0x42}, media.FrameSamples)
	packet, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: media.LawMu.PayloadType(), SequenceNumber: 1, Timestamp: 160, SSRC: 0x1234},
		Payload: payload,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rtpSession.LocalPort})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(packet); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rtpSession.MediaStarted():
	case <-time.After(10 * time.Second):
		t.Fatal("the real packet never arrived")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(v.heard(), 0x42) {
		if time.Now().After(deadline) {
			t.Fatal("the real frame never reached the model")
		}
		time.Sleep(time.Millisecond) // the receive goroutine and the pump run on their own
		v.step(1)
	}
	if !rtpSession.HasReceived() || rtpSession.FirstMediaAt().IsZero() || !rtpSession.LastPacketAt().After(seed) {
		t.Error("a real packet did not move the liveness")
	}
	heardReal := 0
	for _, kind := range v.heard() {
		if kind != fill {
			heardReal++
		}
	}
	if heardReal != 1 {
		t.Errorf("%d real frames heard, want the one packet", heardReal)
	}
}

// The real-frame slot, without media.PutBytes: the pool's Put boxes a slice
// header (24 B), which is the same in every consumer of the pool and is not
// what is being measured here.
func BenchmarkUplinkClockRealFrame(b *testing.B) {
	for _, f := range []uplinkFormat{uplinkFormats[0], uplinkFormats[2]} {
		b.Run(f.name, func(b *testing.B) {
			session, err := New(newFakeLeg(f.law), nopModel{}, f.profile, Config{})
			if err != nil {
				b.Fatal(err)
			}
			epoch := time.Unix(1_000_000, 0)
			clock := newUplinkClock(epoch)
			converted := make([]byte, 0, media.FrameSamples*8)
			frame := make([]byte, media.FrameSamples)
			now := epoch
			b.ReportAllocs()
			for b.Loop() {
				clock.push(frame)
				now = now.Add(uplinkSlot)
				for n := clock.owed(now); n > 0; n-- {
					converted, _ = session.feedSlot(clock, clock.pop(), converted)
				}
			}
		})
	}
}

func BenchmarkUplinkClockFill(b *testing.B) {
	session, err := New(newFakeLeg(media.LawMu), nopModel{}, provider.QwenProfile(), Config{})
	if err != nil {
		b.Fatal(err)
	}
	epoch := time.Unix(1_000_000, 0)
	clock := newUplinkClock(epoch)
	converted := make([]byte, 0, media.FrameSamples*8)
	now := epoch
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(uplinkSlot)
		converted, _ = session.sendDueSlots(clock, now, converted)
	}
}

type nopModel struct{ provider.VoiceSession }

func (nopModel) SendAudio([]byte) error { return nil }
