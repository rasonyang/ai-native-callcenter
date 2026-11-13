// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

const loopback = "127.0.0.1"

func intp(n int) *int { return &n }

// startSim runs a simulator on host and stops it with the test.
func startSim(t *testing.T, host string, scenario *Scenario, adjust func(*Config)) *Server {
	t.Helper()
	cfg := Config{
		Addr: net.JoinHostPort(host, "0"), Scenario: scenario,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if adjust != nil {
		adjust(&cfg)
	}
	sim, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sim.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})
	return sim
}

func scenarioOf(t *testing.T, numbers map[string][]Behaviour) *Scenario {
	t.Helper()
	s := &Scenario{Numbers: numbers}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

// only returns the single record of a report, failing on any other count.
func only(t *testing.T, r Report) DialogRecord {
	t.Helper()
	if len(r.Dialogs) != 1 {
		t.Fatalf("want exactly 1 dialog, got %d", len(r.Dialogs))
	}
	return r.Dialogs[0]
}

func recordFor(t *testing.T, r Report, number string) DialogRecord {
	t.Helper()
	var matches []DialogRecord
	for _, d := range r.Dialogs {
		if d.Number == number {
			matches = append(matches, d)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly 1 dialog for %s, got %d", number, len(matches))
	}
	return matches[0]
}

func provisionalStatuses(d DialogRecord) []int {
	out := []int{}
	for _, p := range d.Provisionals {
		out = append(out, p.Status)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRingingThenEarlyMediaThenAnswer(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"13800000001": {{
			Provisionals: []Provisional{
				{Status: 180, AtMs: 0},
				{Status: 183, AtMs: 60, EarlyMedia: true},
			},
			FinalAfterMs: 400,
			Uplink:       Uplink{Kind: UplinkTone},
		}},
	}), nil)

	uac := newUAC(t, loopback, sim.Addr(), "13800000001")
	uac.invite()
	early := uac.waitResponse(183, "INVITE")
	// Early media: the simulator already sends audio, before the 200.
	eventually(t, "uplink audio during early media", func() bool {
		for _, f := range uac.uplinkFrames() {
			if f.rms > silenceRMS {
				return true
			}
		}
		return false
	})
	if len(uac.statuses()) == 0 || uac.statuses()[len(uac.statuses())-1] != 183 {
		t.Fatalf("the 200 arrived before the early media was heard: %v", uac.statuses())
	}
	final := uac.waitFinal()
	if final.status != 200 {
		t.Fatalf("final = %d", final.status)
	}
	if got, want := remoteRTP(t, final).String(), remoteRTP(t, early).String(); got != want {
		t.Errorf("media address moved between the 183 and the 200: %s -> %s", want, got)
	}

	var rec DialogRecord
	eventually(t, "the ACK to be recorded", func() bool {
		rec = only(t, sim.Report())
		return rec.AckAt != nil
	})
	if got := provisionalStatuses(rec); !equalInts(got, []int{180, 183}) {
		t.Errorf("provisionals = %v, want [180 183]", got)
	}
	if len(rec.Provisionals) == 2 && (rec.Provisionals[0].HasEarlyMedia || !rec.Provisionals[1].HasEarlyMedia) {
		t.Errorf("early media flags = %+v", rec.Provisionals)
	}
	if rec.Final == nil || rec.Final.Status != 200 {
		t.Fatalf("final = %+v", rec.Final)
	}
	if len(rec.Provisionals) == 2 &&
		!(rec.InviteAt.Before(rec.Provisionals[0].SentAt) &&
			rec.Provisionals[0].SentAt.Before(rec.Provisionals[1].SentAt) &&
			rec.Provisionals[1].SentAt.Before(rec.Final.SentAt) &&
			rec.Final.SentAt.Before(*rec.AckAt)) {
		t.Errorf("timestamps are out of order: %+v", rec)
	}
	if rec.Codec != "PCMU" {
		t.Errorf("codec = %q", rec.Codec)
	}
	if rec.Number != "13800000001" || rec.Attempt != 0 || !rec.IsExpected {
		t.Errorf("identity = %q attempt %d expected %v", rec.Number, rec.Attempt, rec.IsExpected)
	}
	if rec.InviteHeaders["x-aicc-attempt-id"] != "attempt-13800000001" {
		t.Errorf("invite headers = %v", rec.InviteHeaders)
	}
	if rec.End != nil {
		t.Errorf("a dialog nobody ended has an end: %+v", rec.End)
	}
}

func TestTheFinalWaitsForItsDelay(t *testing.T) {
	const delay = 300
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{FinalAfterMs: delay}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	if final := uac.waitFinal(); final.status != 200 {
		t.Fatalf("final = %d", final.status)
	}
	rec := only(t, sim.Report())
	if rec.Final == nil {
		t.Fatal("no final recorded")
	}
	// A floor, not a latency figure: the scripted delay cannot be shortened.
	if held := rec.Final.SentAt.Sub(rec.InviteAt); held < delay*time.Millisecond {
		t.Errorf("200 sent after %v, scripted delay is %dms", held, delay)
	}
	if len(rec.Provisionals) != 0 {
		t.Errorf("no provisionals were scripted, got %+v", rec.Provisionals)
	}
}

func TestTheCalleeHangsUpAtTheScriptedTime(t *testing.T) {
	const after = 250
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{ByeAfterMs: intp(after)}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitFinal()
	eventually(t, "the callee's BYE", func() bool { return uac.byeCount() > 0 })
	if got := uac.byeCount(); got != 1 {
		t.Fatalf("BYEs received = %d, want 1", got)
	}
	rec := only(t, sim.Report())
	if rec.End == nil || rec.End.Method != EndedWithBye || rec.End.By != EndedByCallee {
		t.Fatalf("end = %+v", rec.End)
	}
	if held := rec.End.At.Sub(rec.Final.SentAt); held < after*time.Millisecond {
		t.Errorf("BYE sent %v after the 200, scripted %dms", held, after)
	}
}

func TestTheCallerHangsUp(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitFinal()
	uac.bye()
	uac.waitResponse(200, "BYE")
	rec := only(t, sim.Report())
	if rec.End == nil || rec.End.Method != EndedWithBye || rec.End.By != EndedByCaller {
		t.Fatalf("end = %+v", rec.End)
	}
	if uac.byeCount() != 0 {
		t.Errorf("the callee also sent a BYE")
	}
}

// A record-routing proxy between the switch and the callee asked to see the
// BYE, so the callee's BYE carries the route set in the INVITE's order
// (RFC 3261 §12.1.1) and goes to its top entry, not to the caller's Contact.
func TestTheCalleesByeFollowsTheRouteSet(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{ByeAfterMs: intp(50)}},
	}), nil)
	proxy, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(loopback)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.Close() })
	top := fmt.Sprintf("<sip:%s;lr>", proxy.LocalAddr())
	const second = "<sip:10.0.0.8:5070;lr>"

	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.write(uac.request("INVITE", 1, uac.branch,
		"Record-Route: "+top+",\r\n "+second+"\r\nContent-Type: application/sdp\r\n", uac.offer()))
	uac.waitFinal()

	buf := make([]byte, 65536)
	_ = proxy.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := proxy.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no BYE reached the top of the route set: %v", err)
	}
	bye, err := voice.ParseSIP(buf[:n])
	if err != nil || bye.Method() != "BYE" {
		t.Fatalf("the top of the route set got %q, want a BYE", buf[:n])
	}
	if want := []string{top, second}; !slices.Equal(bye.Routes(), want) {
		t.Errorf("Route headers = %q, want %q", bye.Routes(), want)
	}
	if want := "sip:caller@" + uac.local(); bye.RequestURI() != want {
		t.Errorf("Request-URI = %q, want the caller's Contact %q", bye.RequestURI(), want)
	}
	// The simulator sends one BYE, and it went to the proxy.
	if got := uac.byeCount(); got != 0 {
		t.Errorf("the caller's Contact got %d BYEs as well", got)
	}
}

func TestEveryFinalStatusIsSentAndAcknowledged(t *testing.T) {
	statuses := []int{404, 480, 486, 500, 503, 600, 603, 604}
	numbers := map[string][]Behaviour{}
	for _, status := range statuses {
		numbers[fmt.Sprint(status)] = []Behaviour{{
			Provisionals: []Provisional{{Status: 180}},
			FinalStatus:  status,
			FinalAfterMs: 50,
		}}
	}
	sim := startSim(t, loopback, scenarioOf(t, numbers), nil)

	for _, status := range statuses {
		uac := newUAC(t, loopback, sim.Addr(), fmt.Sprint(status))
		uac.invite()
		if final := uac.waitFinal(); final.status != status {
			t.Errorf("number %d: final = %d", status, final.status)
		}
	}
	var report Report
	eventually(t, "every ACK", func() bool {
		report = sim.Report()
		acked := 0
		for _, d := range report.Dialogs {
			if d.AckAt != nil {
				acked++
			}
		}
		return acked == len(statuses)
	})
	if len(report.Dialogs) != len(statuses) {
		t.Fatalf("dialogs = %d, want %d", len(report.Dialogs), len(statuses))
	}
	for _, status := range statuses {
		rec := recordFor(t, report, fmt.Sprint(status))
		if rec.Final == nil || rec.Final.Status != status {
			t.Errorf("number %d recorded final %+v", status, rec.Final)
		}
		if rec.Codec != "PCMU" {
			t.Errorf("number %d: codec %q", status, rec.Codec)
		}
	}
	if report.Failed {
		t.Error("scripted rejections marked the run failed")
	}
}

func TestCancelDuringRingingIsAnswered487AndRecorded(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{Provisionals: []Provisional{{Status: 180}}, FinalAfterMs: 600}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitResponse(180, "INVITE")
	uac.cancel()
	uac.waitResponse(200, "CANCEL")
	uac.waitResponse(487, "INVITE")

	// The scripted answer must not follow the cancel.
	time.Sleep(900 * time.Millisecond)
	if got := uac.statuses(); !equalInts(got, []int{100, 180, 487}) {
		t.Errorf("INVITE responses = %v, want [100 180 487]", got)
	}
	rec := only(t, sim.Report())
	if rec.End == nil || rec.End.Method != EndedWithCancel || rec.End.By != EndedByCaller {
		t.Fatalf("end = %+v", rec.End)
	}
	if rec.Final == nil || rec.Final.Status != 487 {
		t.Errorf("final = %+v", rec.Final)
	}
	eventually(t, "the ACK to the 487", func() bool { return only(t, sim.Report()).AckAt != nil })
}

// afterInviteBounds fails unless got, an interval the simulator read on its
// monotonic clock, lies between hold (the test waited that long between two
// events it saw in order) and elapsed (the test's own time from before the
// first event to after the report was read): the simulator's interval is
// contained in the test's. elapsed is taken after Report returns, since a
// BYE's 200 is sent before the End record is written. Both sides keep
// sub-millisecond precision.
func afterInviteBounds(t *testing.T, what string, got float64, hold, elapsed time.Duration) {
	t.Helper()
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	if got < ms(hold) || got > ms(elapsed) {
		t.Errorf("%s: %.3f ms on the simulator's monotonic clock, want between %.3f and %.3f", what, got, ms(hold), ms(elapsed))
	}
}

// A ringing dialog ended by CANCEL records how long it rang on the
// simulator's monotonic clock.
func TestCancelRecordsTheWaitOnTheMonotonicClock(t *testing.T) {
	const hold = 700 * time.Millisecond
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{Provisionals: []Provisional{{Status: 180}}, FinalAfterMs: 5000}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	start := time.Now()
	uac.invite()
	uac.waitResponse(180, "INVITE")
	time.Sleep(hold)
	uac.cancel()
	uac.waitResponse(487, "INVITE")
	rec := only(t, sim.Report())
	elapsed := time.Since(start)
	if rec.End == nil || rec.End.Method != EndedWithCancel {
		t.Fatalf("end = %+v", rec.End)
	}
	afterInviteBounds(t, "CANCEL", rec.End.AfterInviteMs, hold, elapsed)
	// The 487 is sent at the CANCEL's arrival, read once for both records.
	if rec.Final == nil || rec.Final.Status != 487 || rec.Final.AfterInviteMs != rec.End.AfterInviteMs || rec.Final.AfterStartMs != rec.End.AfterStartMs {
		t.Errorf("final %+v, end %+v: the 487 is not on the CANCEL's reading", rec.Final, rec.End)
	}
}

// onTheTimeline fails unless an event's afterStartMs is its dialog's INVITE
// on the timeline plus its afterInviteMs: the two are one reading of one
// monotonic clock, so they agree to the rounding of float64 milliseconds.
func onTheTimeline(t *testing.T, what string, rec DialogRecord, afterInviteMs, afterStartMs float64) {
	t.Helper()
	if diff := afterStartMs - (rec.InviteAfterStartMs + afterInviteMs); diff > 1e-6 || diff < -1e-6 {
		t.Errorf("%s: afterStartMs %.6f, but the INVITE's %.6f plus afterInviteMs %.6f is %.6f",
			what, afterStartMs, rec.InviteAfterStartMs, afterInviteMs, rec.InviteAfterStartMs+afterInviteMs)
	}
}

// The answer and the end of a dialog are read on one monotonic clock, so the
// time between them (K3's hold, issue #515) is their difference: at least
// what the test waited between the two, at most the test's whole run.
func TestTheAnswerAndTheEndShareTheMonotonicClock(t *testing.T) {
	const hold = 400 * time.Millisecond
	t.Run("caller", func(t *testing.T) {
		sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{FinalAfterMs: 150}}}), nil)
		uac := newUAC(t, loopback, sim.Addr(), "1")
		start := time.Now()
		uac.invite()
		uac.waitResponse(200, "INVITE")
		time.Sleep(hold)
		uac.bye()
		uac.waitResponse(200, "BYE")
		rec := only(t, sim.Report())
		elapsed := time.Since(start)
		if rec.Final == nil || rec.Final.Status != 200 || rec.End == nil || rec.End.By != EndedByCaller {
			t.Fatalf("final %+v, end %+v", rec.Final, rec.End)
		}
		afterInviteBounds(t, "the 200", rec.Final.AfterInviteMs, 0, elapsed)
		afterInviteBounds(t, "the 200 to the caller's BYE", rec.End.AfterInviteMs-rec.Final.AfterInviteMs, hold, elapsed)
		onTheTimeline(t, "the 200", rec, rec.Final.AfterInviteMs, rec.Final.AfterStartMs)
		onTheTimeline(t, "the BYE", rec, rec.End.AfterInviteMs, rec.End.AfterStartMs)
	})
	t.Run("callee", func(t *testing.T) {
		// The callee's BYE is timed from the answer on the monotonic clock, so
		// the difference is at least the scripted time, as K3's R1 reads it.
		const after = 400
		sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{FinalAfterMs: 150, ByeAfterMs: intp(after)}}}), nil)
		uac := newUAC(t, loopback, sim.Addr(), "1")
		start := time.Now()
		uac.invite()
		uac.waitFinal()
		eventually(t, "the callee's BYE", func() bool { return uac.byeCount() > 0 })
		rec := only(t, sim.Report())
		elapsed := time.Since(start)
		if rec.Final == nil || rec.Final.Status != 200 || rec.End == nil || rec.End.By != EndedByCallee {
			t.Fatalf("final %+v, end %+v", rec.Final, rec.End)
		}
		afterInviteBounds(t, "the 200 to the callee's BYE", rec.End.AfterInviteMs-rec.Final.AfterInviteMs, after*time.Millisecond, elapsed)
		onTheTimeline(t, "the 200", rec, rec.Final.AfterInviteMs, rec.Final.AfterStartMs)
		onTheTimeline(t, "the BYE", rec, rec.End.AfterInviteMs, rec.End.AfterStartMs)
	})
}

// A refusal records its final response on both clocks too.
func TestARefusalRecordsItsResponseOnTheMonotonicClock(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{Provisionals: []Provisional{{Status: 180}}, FinalAfterMs: 200, FinalStatus: 486}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	start := time.Now()
	uac.invite()
	uac.waitResponse(180, "INVITE")
	busy := uac.waitResponse(486, "INVITE")
	rec := only(t, sim.Report())
	if rec.Final == nil || rec.Final.Status != 486 {
		t.Fatalf("final %+v", rec.Final)
	}
	afterInviteBounds(t, "the 486", rec.Final.AfterInviteMs, 0, busy.at.Sub(start))
	onTheTimeline(t, "the 486", rec, rec.Final.AfterInviteMs, rec.Final.AfterStartMs)
}

// Every dialog of one simulator is on one monotonic timeline, counted from its
// start: the gap between two INVITEs (C04's retry interval) and the order of
// events across dialogs (K1's and K3's preconditions) are read from it. The
// report says when it was taken on that timeline, which is after every event
// it holds (C10's read after the retry fell due).
func TestDialogsShareOneMonotonicTimeline(t *testing.T) {
	const lead, gap = 150 * time.Millisecond, 300 * time.Millisecond
	before := time.Now()
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{FinalStatus: 486}, {FinalStatus: 486}},
	}), nil)
	created := time.Now()
	time.Sleep(lead)

	first := newUAC(t, loopback, sim.Addr(), "1")
	sent := time.Now()
	first.invite()
	first.waitFinal()
	time.Sleep(gap)
	second := newUAC(t, loopback, sim.Addr(), "1")
	second.invite()
	second.waitFinal()
	eventually(t, "both ACKs", func() bool {
		r := sim.Report()
		return len(r.Dialogs) == 2 && r.Dialogs[0].AckAt != nil && r.Dialogs[1].AckAt != nil
	})
	report := sim.Report()
	end := time.Since(before)

	if len(report.Dialogs) != 2 {
		t.Fatalf("dialogs = %d, want 2", len(report.Dialogs))
	}
	one, two := report.Dialogs[0], report.Dialogs[1]
	// The simulator started after before and before created; the first INVITE
	// arrived after sent.
	afterInviteBounds(t, "the first INVITE after the start", one.InviteAfterStartMs, sent.Sub(created), end)
	// The test sent the second INVITE gap after it had the first one's answer.
	afterInviteBounds(t, "the second INVITE after the first", two.InviteAfterStartMs-one.InviteAfterStartMs, gap, time.Since(sent))
	// The 486 was sent before the test had it, and after the test sent the
	// INVITE it answers.
	afterInviteBounds(t, "the first 486 to the second INVITE", two.InviteAfterStartMs-one.Final.AfterStartMs, gap, time.Since(sent))
	for i, rec := range report.Dialogs {
		if rec.Final == nil {
			t.Fatalf("dialog %d has no final response", i)
		}
		onTheTimeline(t, fmt.Sprintf("dialog %d's 486", i), rec, rec.Final.AfterInviteMs, rec.Final.AfterStartMs)
	}
	if report.AfterStartMs < two.Final.AfterStartMs || report.AfterStartMs > float64(end)/float64(time.Millisecond) {
		t.Errorf("the report was taken at %.3f ms, want after the last event (%.3f ms) and within the test's %s",
			report.AfterStartMs, two.Final.AfterStartMs, end)
	}
	if !report.StartedAt.Equal(sim.startedAt) || report.StartedAt.Location() != time.UTC {
		t.Errorf("startedAt %v, want the simulator's start %v in UTC", report.StartedAt, sim.startedAt)
	}

	// The monotonic values travel under their own names, beside the stamps.
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"startedAt":`, `"afterStartMs":`, `"inviteAfterStartMs":`, `"afterInviteMs":`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("the report's JSON has no %s", key)
		}
	}
}

func TestByeFromEachSideRecordsTheWaitOnTheMonotonicClock(t *testing.T) {
	const hold = 400 * time.Millisecond
	t.Run("caller", func(t *testing.T) {
		sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
		uac := newUAC(t, loopback, sim.Addr(), "1")
		start := time.Now()
		uac.invite()
		uac.waitFinal()
		time.Sleep(hold)
		uac.bye()
		uac.waitResponse(200, "BYE")
		rec := only(t, sim.Report())
		elapsed := time.Since(start)
		if rec.End == nil || rec.End.By != EndedByCaller {
			t.Fatalf("end = %+v", rec.End)
		}
		afterInviteBounds(t, "caller BYE", rec.End.AfterInviteMs, hold, elapsed)
	})
	t.Run("callee", func(t *testing.T) {
		const after = 400
		sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{ByeAfterMs: intp(after)}}}), nil)
		uac := newUAC(t, loopback, sim.Addr(), "1")
		start := time.Now()
		uac.invite()
		uac.waitFinal()
		eventually(t, "the callee's BYE", func() bool { return uac.byeCount() > 0 })
		rec := only(t, sim.Report())
		elapsed := time.Since(start)
		if rec.End == nil || rec.End.By != EndedByCallee {
			t.Fatalf("end = %+v", rec.End)
		}
		afterInviteBounds(t, "callee BYE", rec.End.AfterInviteMs, after*time.Millisecond, elapsed)
	})
}

// The measurement reads the INVITE's monotonic reading; Time.UTC() strips it
// without a sound, which would turn the measurement back into a wall-clock
// subtraction. Time.String() prints an m= field only while one is present.
func TestTheInviteTimeKeepsItsMonotonicReading(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitFinal()
	sim.mu.Lock()
	if len(sim.order) != 1 {
		sim.mu.Unlock()
		t.Fatalf("dialogs = %d, want 1", len(sim.order))
	}
	at := sim.order[0].inviteAt
	sim.mu.Unlock()
	if !strings.Contains(at.String(), " m=") {
		t.Errorf("the INVITE time %q has no monotonic reading", at.String())
	}
	// The same holds for the zero of the timeline every afterStartMs is read on.
	if !strings.Contains(sim.startedAt.String(), " m=") {
		t.Errorf("the start time %q has no monotonic reading", sim.startedAt.String())
	}
}

func TestEachAttemptOnANumberUsesItsOwnBehaviour(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{FinalStatus: 486}, {}},
	}), nil)

	var finals []int
	for range 3 {
		uac := newUAC(t, loopback, sim.Addr(), "1")
		uac.invite()
		finals = append(finals, uac.waitFinal().status)
	}
	// Attempt 0 is busy, attempt 1 answers, attempt 2 is past the end and
	// repeats the last entry.
	if !equalInts(finals, []int{486, 200, 200}) {
		t.Fatalf("finals = %v, want [486 200 200]", finals)
	}
	report := sim.Report()
	if len(report.Dialogs) != 3 {
		t.Fatalf("dialogs = %d, want 3", len(report.Dialogs))
	}
	for i, d := range report.Dialogs {
		if d.Attempt != i {
			t.Errorf("dialog %d has attempt %d", i, d.Attempt)
		}
	}
}

func TestAnUnknownNumberGets404AndFailsTheRun(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)

	known := newUAC(t, loopback, sim.Addr(), "1")
	known.invite()
	known.waitFinal()
	if sim.Report().Failed {
		t.Fatal("a run with only known numbers is marked failed")
	}

	stranger := newUAC(t, loopback, sim.Addr(), "9999")
	stranger.invite()
	if final := stranger.waitFinal(); final.status != 404 {
		t.Fatalf("final = %d, want 404", final.status)
	}
	report := sim.Report()
	if len(report.Dialogs) != 2 {
		t.Fatalf("dialogs = %d, want 2", len(report.Dialogs))
	}
	if !report.Failed || report.UnexpectedCount != 1 {
		t.Errorf("failed=%v unexpected=%d", report.Failed, report.UnexpectedCount)
	}
	if rec := recordFor(t, report, "9999"); rec.IsExpected || rec.Final == nil || rec.Final.Status != 404 {
		t.Errorf("record = %+v", rec)
	}
	if rec := recordFor(t, report, "1"); !rec.IsExpected {
		t.Error("the known number was flagged")
	}
}

func TestConcurrentDialogsAreRecordedIndependently(t *testing.T) {
	const calls = 16
	numbers := map[string][]Behaviour{}
	for i := range calls {
		b := Behaviour{FinalAfterMs: 20 + 10*(i%5)}
		switch i % 4 {
		case 0:
			b.FinalStatus = 486
		case 1:
			b.FinalStatus = 603
		case 2:
			b.ByeAfterMs = intp(30 + 10*(i%3))
		}
		numbers[fmt.Sprintf("n%02d", i)] = []Behaviour{b}
	}
	sim := startSim(t, loopback, scenarioOf(t, numbers), nil)

	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uac := newUAC(t, loopback, sim.Addr(), fmt.Sprintf("n%02d", i))
			uac.invite()
			uac.waitFinal()
			switch i % 4 {
			case 2:
				eventually(t, "the callee's BYE", func() bool { return uac.byeCount() == 1 })
			case 3:
				uac.bye() // answered and never scripted to hang up: the caller does
			}
		}()
	}
	wg.Wait()

	var report Report
	eventually(t, "every dialog to finish", func() bool {
		report = sim.Report()
		ended := 0
		for _, d := range report.Dialogs {
			if d.Final != nil && (d.Final.Status != 200 || d.End != nil) {
				ended++
			}
		}
		return len(report.Dialogs) == calls && ended == calls
	})
	if report.UnexpectedCount != 0 || len(report.Dialogs) != calls {
		t.Fatalf("dialogs=%d unexpected=%d", len(report.Dialogs), report.UnexpectedCount)
	}
	seen := map[string]bool{}
	for i := range calls {
		number := fmt.Sprintf("n%02d", i)
		rec := recordFor(t, report, number)
		if seen[rec.CallID] {
			t.Errorf("call id %s recorded twice", rec.CallID)
		}
		seen[rec.CallID] = true
		want := 200
		switch i % 4 {
		case 0:
			want = 486
		case 1:
			want = 603
		}
		if rec.Final.Status != want || rec.Attempt != 0 {
			t.Errorf("%s: final %d attempt %d, want %d attempt 0", number, rec.Final.Status, rec.Attempt, want)
		}
		if gotBye := rec.End != nil && rec.End.By == EndedByCallee; gotBye != (i%4 == 2) {
			t.Errorf("%s: callee BYE recorded = %v", number, gotBye)
		}
		if gotBye := rec.End != nil && rec.End.By == EndedByCaller; gotBye != (i%4 == 3) {
			t.Errorf("%s: caller BYE recorded = %v", number, gotBye)
		}
	}
	if len(seen) != calls {
		t.Errorf("distinct call ids = %d, want %d", len(seen), calls)
	}
}

func TestFirstAudioIsMeasuredAgainstAKnownSilentLeadIn(t *testing.T) {
	const lead = 400 * time.Millisecond
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	final := uac.waitFinal()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan struct{})
	go func() {
		uac.sendDownlink(ctx, remoteRTP(t, final), lead, 600*time.Millisecond)
		close(sent)
	}()
	<-sent
	var rec DialogRecord
	eventually(t, "the first audible frame", func() bool {
		rec = only(t, sim.Report())
		return rec.Downlink.FirstAudioAfterAnswerMs != nil
	})
	got := *rec.Downlink.FirstAudioAfterAnswerMs
	t.Logf("first audio %.1f ms after the 200 OK, silent lead-in %v", got, lead)
	// A floor, not a latency figure: audio cannot be heard before it was sent.
	if got < float64(lead.Milliseconds()) {
		t.Errorf("first audio at %.1f ms, but the first %v were silent", got, lead)
	}

	if rec.Downlink.Frames < 40 {
		t.Fatalf("frames = %d, want most of the 50 sent", rec.Downlink.Frames)
	}
	var silent, loud int
	var lastSilentStart, firstLoudStart = -1, -1
	for _, b := range rec.Downlink.Energy {
		if b.PeakRMS > silenceRMS {
			loud++
			if firstLoudStart < 0 {
				firstLoudStart = b.StartMs
			}
		} else {
			silent++
			lastSilentStart = b.StartMs
		}
	}
	if silent == 0 || loud == 0 {
		t.Fatalf("energy has %d silent and %d loud buckets, want both", silent, loud)
	}
	if lastSilentStart > firstLoudStart {
		t.Errorf("a silent bucket (%d ms) follows a loud one (%d ms)", lastSilentStart, firstLoudStart)
	}
}

func TestAudioBeforeTheAnswerDoesNotCountAsFirstAudio(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{Provisionals: []Provisional{{Status: 183, EarlyMedia: true}}, FinalAfterMs: 500}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	early := uac.waitResponse(183, "INVITE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An announcement over early media, finished before the answer.
	uac.sendDownlink(ctx, remoteRTP(t, early), 0, 200*time.Millisecond)
	uac.waitFinal()
	time.Sleep(100 * time.Millisecond)
	rec := only(t, sim.Report())
	if rec.Downlink.Frames == 0 {
		t.Fatal("the early audio was not recorded at all")
	}
	if rec.Downlink.FirstAudioAfterAnswerMs != nil {
		t.Errorf("first audio after answer = %.1f, but nothing was sent after it", *rec.Downlink.FirstAudioAfterAnswerMs)
	}
}

func TestCodecIsNegotiatedPerOffer(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)

	alaw := newUAC(t, loopback, sim.Addr(), "1")
	alaw.offerCodecs = "8"
	alaw.invite()
	if final := alaw.waitFinal(); final.status != 200 {
		t.Fatalf("PCMA-only offer: final = %d", final.status)
	}

	g729 := newUAC(t, loopback, sim.Addr(), "1")
	g729.offerCodecs = "18"
	g729.invite()
	if final := g729.waitFinal(); final.status != 488 {
		t.Fatalf("G.729-only offer: final = %d, want 488", final.status)
	}

	report := sim.Report()
	if len(report.Dialogs) != 2 {
		t.Fatalf("dialogs = %d, want 2", len(report.Dialogs))
	}
	if got := report.Dialogs[0].Codec; got != "PCMA" {
		t.Errorf("codec = %q, want PCMA", got)
	}
	if got := report.Dialogs[1].Codec; got != "" {
		t.Errorf("a refused offer recorded codec %q", got)
	}
}

func TestARetransmittedInviteIsOneDialog(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{FinalStatus: 486}, {}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitFinal()
	uac.invite()
	eventually(t, "the final to be repeated", func() bool {
		n := 0
		for _, s := range uac.statuses() {
			if s == 486 {
				n++
			}
		}
		return n >= 2
	})
	report := sim.Report()
	if len(report.Dialogs) != 1 || report.Dialogs[0].Attempt != 0 {
		t.Fatalf("dialogs = %+v", report.Dialogs)
	}
	next := newUAC(t, loopback, sim.Addr(), "1")
	next.invite()
	if final := next.waitFinal(); final.status != 200 {
		t.Errorf("the retransmission consumed attempt 1's behaviour: final %d", final.status)
	}
}

func TestAnUnacknowledged200IsRetransmitted(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.noAck = true
	uac.invite()
	uac.waitFinal()
	eventually(t, "a second 200", func() bool {
		n := 0
		for _, s := range uac.statuses() {
			if s == 200 {
				n++
			}
		}
		return n >= 2
	})
	if only(t, sim.Report()).AckAt != nil {
		t.Fatal("an ACK was recorded that nobody sent")
	}
	uac.ack()
	eventually(t, "the ACK", func() bool { return only(t, sim.Report()).AckAt != nil })
}

func TestBindsTheConfiguredAddressNotLoopback(t *testing.T) {
	host := nonLoopbackIPv4(t)
	sim := startSim(t, host, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
	if got := sim.Addr().IP.String(); got != host {
		t.Fatalf("bound %s, configured %s", got, host)
	}
	uac := newUAC(t, host, sim.Addr(), "1")
	uac.invite()
	final := uac.waitFinal()
	if got := remoteRTP(t, final).IP.String(); got != host {
		t.Errorf("SDP advertises %s, want the bound %s", got, host)
	}
	// Nothing listens on loopback.
	probe, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(loopback), Port: sim.Addr().Port})
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err := probe.Write([]byte("OPTIONS sip:x@y SIP/2.0\r\nCall-ID: p\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = probe.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := probe.Read(make([]byte, 1500)); err == nil {
		t.Errorf("loopback answered with %d bytes", n)
	}
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address on this host")
	return ""
}

func TestWAVUplinkPlaysOnceThenGoesQuiet(t *testing.T) {
	// 300 ms of a 1 kHz square-ish tone at 8 kHz mono 16-bit.
	samples := make([]int16, 2400)
	for i := range samples {
		if (i/4)%2 == 0 {
			samples[i] = 6000
		} else {
			samples[i] = -6000
		}
	}
	path := filepath.Join(t.TempDir(), "voice.wav")
	if err := os.WriteFile(path, wavBytes(samples), 0o644); err != nil {
		t.Fatal(err)
	}
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"1": {{Uplink: Uplink{Kind: UplinkWAV, File: path}}},
	}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	final := uac.waitFinal()
	// The uplink flows only once media is open; give the caller's side a
	// frame to come up, then listen.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uac.sendDownlink(ctx, remoteRTP(t, final), 0, 1200*time.Millisecond)
	eventually(t, "the WAV to be heard", func() bool {
		loud := 0
		for _, f := range uac.uplinkFrames() {
			if f.rms > silenceRMS {
				loud++
			}
		}
		return loud >= 10
	})
	time.Sleep(600 * time.Millisecond)
	frames := uac.uplinkFrames()
	loud, quietTail := 0, 0
	for i, f := range frames {
		if f.rms > silenceRMS {
			loud++
			quietTail = 0
		} else if i > 0 {
			quietTail++
		}
	}
	if loud < 10 || loud > 17 {
		t.Errorf("loud frames = %d, want about the 15 the file holds", loud)
	}
	if quietTail < 5 {
		t.Errorf("only %d quiet frames after the file ended", quietTail)
	}
}

func wavBytes(samples []int16) []byte {
	data := media.PCM16ToBytes(nil, samples)
	le32 := func(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	le16 := func(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
	var b []byte
	b = append(b, "RIFF"...)
	b = append(b, le32(uint32(36+len(data)))...)
	b = append(b, "WAVEfmt "...)
	b = append(b, le32(16)...)
	b = append(b, le16(1)...)    // PCM
	b = append(b, le16(1)...)    // mono
	b = append(b, le32(8000)...) // rate
	b = append(b, le32(16000)...)
	b = append(b, le16(2)...)
	b = append(b, le16(16)...)
	b = append(b, "data"...)
	b = append(b, le32(uint32(len(data)))...)
	return append(b, data...)
}

func TestScenarioValidation(t *testing.T) {
	bad := map[string]string{
		"redirect final":      `{"numbers":{"1":[{"finalStatus":302}]}}`,
		"no behaviours":       `{"numbers":{"1":[]}}`,
		"empty number":        `{"numbers":{"":[{}]}}`,
		"bye after a refusal": `{"numbers":{"1":[{"finalStatus":486,"byeAfterMs":10}]}}`,
		"180 early media":     `{"numbers":{"1":[{"provisionals":[{"status":180,"earlyMedia":true}]}]}}`,
		"provisional 200":     `{"numbers":{"1":[{"provisionals":[{"status":200}]}]}}`,
		"provisional late":    `{"numbers":{"1":[{"provisionals":[{"status":180,"atMs":500}],"finalAfterMs":100}]}}`,
		"unknown uplink":      `{"numbers":{"1":[{"uplink":{"kind":"MUSIC"}}]}}`,
		"wav without file":    `{"numbers":{"1":[{"uplink":{"kind":"WAV"}}]}}`,
		"not json":            `{`,
	}
	for name, doc := range bad {
		if _, err := ParseScenario([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good, err := ParseScenario([]byte(`{"numbers":{"1":[{"provisionals":[{"status":183,"atMs":10,"earlyMedia":true}],
		"finalStatus":200,"finalAfterMs":50,"byeAfterMs":0,"uplink":{"kind":"TONE","frequencyHz":300}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(good.Numbers) != 1 || good.Numbers["1"][0].ByeAfterMs == nil {
		t.Errorf("parsed = %+v", good)
	}
	if _, ok := good.For("2", 0); ok {
		t.Error("an unknown number resolved to a behaviour")
	}
	if b, ok := good.For("1", 7); !ok || b.FinalAfterMs != 50 {
		t.Errorf("past the end = %+v ok=%v", b, ok)
	}
}

func TestAMissingWAVFailsAtStartup(t *testing.T) {
	_, err := New(Config{
		Addr: net.JoinHostPort(loopback, "0"),
		Scenario: scenarioOf(t, map[string][]Behaviour{
			"1": {{Uplink: Uplink{Kind: UplinkWAV, File: filepath.Join(t.TempDir(), "absent.wav")}}},
		}),
	})
	if err == nil {
		t.Fatal("a scenario naming a missing file started")
	}
}

func TestTheReportIsServedAsJSONAndWrittenToAFile(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{FinalStatus: 486}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitFinal()
	stranger := newUAC(t, loopback, sim.Addr(), "2")
	stranger.invite()
	stranger.waitFinal()

	server := httptest.NewServer(sim.Handler())
	defer server.Close()
	resp, err := http.Get(server.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var served Report
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil {
		t.Fatal(err)
	}
	if len(served.Dialogs) != 2 || !served.Failed || served.UnexpectedCount != 1 {
		t.Fatalf("served = %+v", served)
	}

	path := filepath.Join(t.TempDir(), "report.json")
	if err := sim.Report().WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var written Report
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	if len(written.Dialogs) != 2 {
		t.Fatalf("written dialogs = %d, want 2", len(written.Dialogs))
	}
	for _, d := range written.Dialogs {
		if d.CallID == "" || d.InviteAt.IsZero() || d.Final == nil || len(d.InviteHeaders) == 0 {
			t.Errorf("incomplete record: %+v", d)
		}
	}
}

func TestShuttingDownEndsOpenDialogsAndKeepsTheirRecord(t *testing.T) {
	sim, err := New(Config{
		Addr: net.JoinHostPort(loopback, "0"),
		Scenario: scenarioOf(t, map[string][]Behaviour{
			"1": {{Provisionals: []Provisional{{Status: 180}}, FinalAfterMs: 60000}},
		}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sim.Serve(ctx) }()

	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitResponse(180, "INVITE")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return with a dialog still ringing")
	}
	rec := only(t, sim.Report())
	if len(rec.Provisionals) != 1 || rec.Final != nil {
		t.Errorf("record = %+v", rec)
	}
}

// With a called-number prefix configured, the gateway's prefix is taken off
// before the scenario lookup: the record keeps the dialled number, the wire
// form stays in the Request-URI and the To header, and a second dialog on the
// same number counts as its second attempt.
func TestACalledPrefixIsRemovedBeforeTheScenarioLookup(t *testing.T) {
	const prefix = "a1b2cc"
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"13800000001": {{FinalStatus: 486}, {}},
	}), func(c *Config) { c.CalledPrefix = prefix })

	var finals []int
	for range 2 {
		uac := newUAC(t, loopback, sim.Addr(), prefix+"13800000001")
		uac.invite()
		finals = append(finals, uac.waitFinal().status)
	}
	if !equalInts(finals, []int{486, 200}) {
		t.Fatalf("finals = %v, want [486 200]", finals)
	}
	report := sim.Report()
	if report.Failed || report.UnexpectedCount != 0 {
		t.Fatalf("failed=%v unexpected=%d, want a clean run", report.Failed, report.UnexpectedCount)
	}
	for i, d := range report.Dialogs {
		if d.Number != "13800000001" || !d.IsExpected || d.Attempt != i {
			t.Errorf("dialog %d: number=%q expected=%v attempt=%d", i, d.Number, d.IsExpected, d.Attempt)
		}
		if !strings.HasPrefix(d.RequestURI, "sip:"+prefix+"13800000001@") {
			t.Errorf("dialog %d: request URI %q lost the prefix", i, d.RequestURI)
		}
		if !strings.Contains(d.InviteHeaders["to"], "sip:"+prefix+"13800000001@") {
			t.Errorf("dialog %d: To %q lost the prefix", i, d.InviteHeaders["to"])
		}
	}
}

// A gateway that lost its prefix is caught: the INVITE for a number the
// scenario knows, but without the prefix, is unexpected and fails the run.
func TestAnInviteWithoutTheCalledPrefixIsUnexpected(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{
		"13800000001": {{}},
	}), func(c *Config) { c.CalledPrefix = "a1b2cc" })

	bare := newUAC(t, loopback, sim.Addr(), "13800000001")
	bare.invite()
	if final := bare.waitFinal(); final.status != 404 {
		t.Fatalf("final = %d, want 404", final.status)
	}
	report := sim.Report()
	if !report.Failed || report.UnexpectedCount != 1 {
		t.Fatalf("failed=%v unexpected=%d, want the run failed with one unexpected dialog", report.Failed, report.UnexpectedCount)
	}
	if d := only(t, report); d.IsExpected || d.Number != "13800000001" {
		t.Errorf("record = number %q expected %v", d.Number, d.IsExpected)
	}
}
