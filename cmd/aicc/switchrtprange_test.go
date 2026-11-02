// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

type rtpSwitch struct {
	mu  sync.Mutex
	r   telephony.RTPRange
	err error
}

func (s *rtpSwitch) read() (telephony.RTPRange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r, s.err
}

func (s *rtpSwitch) set(r telephony.RTPRange, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r, s.err = r, err
}

func fixedRange(a, b int) telephony.RTPRange {
	return telephony.RTPRange{Start: a, End: b, StartSet: true, EndSet: true}
}

func rtpFixture(t *testing.T, sw *rtpSwitch, enabled bool) (*wiringFixture, *botRTPRangeCheck, *bytes.Buffer) {
	t.Helper()
	f := newWiringFixture(t)
	var logs bytes.Buffer
	check := newBotRTPRangeCheck(sw.read, 26000, 29999, enabled, slog.New(slog.NewTextHandler(&logs, nil)))
	f.comp.BotRTPRange = check
	f.comp.connect()
	return f, check, &logs
}

func TestAnOverlapIsRecordedLoggedAndReported(t *testing.T) {
	t.Parallel()
	f, check, logs := rtpFixture(t, &rtpSwitch{r: telephony.RTPRange{Start: 16384, End: 32768}}, true)
	if got := check.Status().State; got != httpapi.BotRTPRangeUnknown {
		t.Fatalf("before any connect: %q", got)
	}
	f.link.connected(t.Context())
	st := check.Status()
	if !st.IsViolation() || st.SwitchStart != 16384 || st.SwitchEnd != 32768 || st.BotLow != 26000 {
		t.Fatalf("status %+v", st)
	}
	out := logs.String()
	for _, want := range []string{"level=ERROR", "botStart=26000", "botEnd=29999", "switchStart=16384", "switchEnd=32768", "AICC_BOT_RTP_PORT_LOW"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %s", want, out)
		}
	}
}

func TestClearRangesAreOK(t *testing.T) {
	t.Parallel()
	f, check, logs := rtpFixture(t, &rtpSwitch{r: fixedRange(20000, 25999)}, true)
	f.link.connected(t.Context())
	if st := check.Status(); st.State != httpapi.BotRTPRangeOK || st.IsViolation() {
		t.Fatalf("status %+v", st)
	}
	if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("logged: %s", logs)
	}
}

func TestADisabledBotIsOffAndAsksNothing(t *testing.T) {
	t.Parallel()
	sw := &rtpSwitch{r: fixedRange(26000, 29999)}
	f, check, _ := rtpFixture(t, sw, false)
	f.link.connected(t.Context())
	if st := check.Status(); st.State != httpapi.BotRTPRangeOff || st.IsViolation() {
		t.Fatalf("status %+v", st)
	}
	f.link.lost()
	if check.Status().State != httpapi.BotRTPRangeOff {
		t.Fatalf("lost changed an off bot: %+v", check.Status())
	}
}

func TestAnUnreadableOrInvalidRangeIsUnknownNotAViolation(t *testing.T) {
	t.Parallel()
	for name, sw := range map[string]*rtpSwitch{
		"read failed": {err: errors.New("timeout")},
		"inverted":    {r: telephony.RTPRange{Start: 30000, End: 20000, StartSet: true, EndSet: true}},
	} {
		f, check, logs := rtpFixture(t, sw, true)
		f.link.connected(t.Context())
		if check.Status().State != httpapi.BotRTPRangeUnknown || check.Status().IsViolation() {
			t.Errorf("%s: status %+v", name, check.Status())
		}
		if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
			t.Errorf("%s: want a WARN only: %s", name, logs)
		}
	}
}

func TestLosingTheLinkForgetsAnOverlapAndReconnectingRechecks(t *testing.T) {
	t.Parallel()
	sw := &rtpSwitch{r: fixedRange(26000, 27000)}
	f, check, _ := rtpFixture(t, sw, true)
	f.link.connected(t.Context())
	if !check.Status().IsViolation() {
		t.Fatalf("status %+v", check.Status())
	}
	f.link.lost()
	if check.Status().State != httpapi.BotRTPRangeUnknown {
		t.Fatalf("after lost: %+v", check.Status())
	}
	sw.set(fixedRange(20000, 25999), nil)
	f.link.connected(t.Context())
	if st := check.Status(); st.State != httpapi.BotRTPRangeOK || st.SwitchEnd != 25999 {
		t.Fatalf("after reconnect: %+v", st)
	}
}

func TestTheRTPRangeStatusIsSafeToReadWhileTheLinkWritesIt(t *testing.T) {
	t.Parallel()
	f, check, _ := rtpFixture(t, &rtpSwitch{r: fixedRange(20000, 25999)}, true)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			f.link.connected(t.Context())
			f.link.lost()
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = check.Status()
		}
	}()
	wg.Wait()
}

// An app that /readyz fails for the RTP range is named as such by doctor, not
// blamed on the database.
func TestDoctorNamesTheRTPRangeWhenReadyzFailsForIt(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "the bot's RTP port range overlaps the switch's\ndatabase: ok\nswitch: up\nmigrations: 47/47\n"+
			"switch-uuid: v7\nbot-rtp-range: OVERLAPS_SWITCH bot=26000-29999 switch=16384-32768\n")
	}))
	t.Cleanup(srv.Close)
	h.opts.readyzURL = srv.URL + "/readyz"
	r := h.run()[DoctorCheckApp]
	if r.State != DoctorStateFail || !strings.Contains(r.Fix, "bot_rtp_range") {
		t.Fatalf("app check = %+v", r)
	}
}
