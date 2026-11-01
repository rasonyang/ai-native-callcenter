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
)

// uuidSwitch answers create_uuid as the test chooses, and counts the asks.
type uuidSwitch struct {
	mu      sync.Mutex
	version int
	err     error
	asked   int
}

func (s *uuidSwitch) ask() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	return s.version, s.err
}

func (s *uuidSwitch) set(v int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version, s.err = v, err
}

func uuidFixture(t *testing.T, sw *uuidSwitch) (*wiringFixture, *switchUUIDCheck, *bytes.Buffer) {
	t.Helper()
	f := newWiringFixture(t)
	var logs bytes.Buffer
	check := newSwitchUUIDCheck(sw.ask, slog.New(slog.NewTextHandler(&logs, nil)))
	f.comp.SwitchUUID = check
	f.comp.connect()
	return f, check, &logs
}

func TestConnectingToAV7SwitchRecordsV7(t *testing.T) {
	t.Parallel()
	f, check, logs := uuidFixture(t, &uuidSwitch{version: 7})
	if got := check.Status(); got.Checked {
		t.Fatalf("status before any connect = %+v, want unknown", got)
	}
	f.link.connected(t.Context())
	if got := check.Status(); !got.Checked || got.Version != 7 || got.IsViolation() {
		t.Fatalf("status = %+v, want v7", got)
	}
	if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a v7 switch logged: %s", logs)
	}
}

func TestConnectingToAV4SwitchRecordsAViolationAndLogsTheFix(t *testing.T) {
	t.Parallel()
	f, check, logs := uuidFixture(t, &uuidSwitch{version: 4})
	f.link.connected(t.Context())
	if got := check.Status(); !got.IsViolation() || got.Version != 4 {
		t.Fatalf("status = %+v, want a v4 violation", got)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "uuid-version=7") || !strings.Contains(out, "version=4") {
		t.Errorf("log lacks the error, the version or the fix: %s", out)
	}
}

func TestACommandErrorIsUnknownAndWarns(t *testing.T) {
	t.Parallel()
	f, check, logs := uuidFixture(t, &uuidSwitch{err: errors.New("timeout")})
	f.link.connected(t.Context())
	if check.Status().Checked {
		t.Fatalf("status = %+v, want unknown", check.Status())
	}
	if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("want a WARN only: %s", logs)
	}
}

func TestLosingTheLinkForgetsTheResultAndReconnectingChecksAgain(t *testing.T) {
	t.Parallel()
	sw := &uuidSwitch{version: 4}
	f, check, _ := uuidFixture(t, sw)
	f.link.connected(t.Context())
	f.link.lost()
	if check.Status().Checked {
		t.Fatalf("status after lost = %+v, want unknown", check.Status())
	}
	// The operator fixed the switch while the link was down.
	sw.set(7, nil)
	f.link.connected(t.Context())
	if got := check.Status(); !got.Checked || got.Version != 7 {
		t.Fatalf("status after reconnect = %+v, want v7", got)
	}
	if sw.asked != 2 {
		t.Errorf("asked %d times, want once per connect (2)", sw.asked)
	}
}

func TestACompositionWithoutTheCheckRegistersNothingForIt(t *testing.T) {
	t.Parallel()
	with, without := newWiringFixture(t), newWiringFixture(t)
	with.comp.SwitchUUID = newSwitchUUIDCheck(func() (int, error) { return 7, nil }, slog.Default())
	with.comp.connect()
	without.comp.connect()
	if len(with.link.onConnect) != len(without.link.onConnect)+1 || len(with.link.onLost) != len(without.link.onLost)+1 {
		t.Errorf("connect hooks %d/%d, lost hooks %d/%d", len(with.link.onConnect), len(without.link.onConnect),
			len(with.link.onLost), len(without.link.onLost))
	}
}

// The status is read by /readyz while the link's goroutine writes it.
func TestTheStatusIsSafeToReadWhileTheLinkWritesIt(t *testing.T) {
	t.Parallel()
	f, check, _ := uuidFixture(t, &uuidSwitch{version: 7})
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

// An app that /readyz fails for the UUID reason is named as such by doctor,
// not blamed on the database.
func TestDoctorNamesTheUUIDWhenReadyzFailsForIt(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "the switch mints UUID version 4, not 7\ndatabase: ok\nswitch: up\nmigrations: 33/33\n"+
			"switch-uuid: NOT_V7 version=4 (set uuid-version=7 in switch.conf.xml; needs FreeSWITCH 1.11 or later)\n")
	}))
	t.Cleanup(srv.Close)
	h.opts.readyzURL = srv.URL + "/readyz"
	r := h.run()[DoctorCheckApp]
	if r.State != DoctorStateFail || !strings.Contains(r.Fix, "switch_uuid") {
		t.Fatalf("app check = %+v", r)
	}
}
