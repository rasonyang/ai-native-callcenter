// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// readyz is read by `aicc doctor` as well as by container probes, so its body
// is a contract of its own: the lines below are what an installer parses.

func readyz(t *testing.T, ready Readiness) (int, string) {
	t.Helper()
	h := MetricsHandler(http.NotFoundHandler(), ready)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

func migrationsAt(db, latest int64) func(context.Context) (store.MigrationState, error) {
	return func(context.Context) (store.MigrationState, error) {
		return store.MigrationState{DBVersion: db, LatestVersion: latest}, nil
	}
}

func TestReadyReportsEachDependencyOnItsOwnLine(t *testing.T) {
	code, body := readyz(t, Readiness{
		Ping:       func(context.Context) error { return nil },
		IsSwitchUp: func() bool { return true },
		Migrations: migrationsAt(33, 33),
	})
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	want := "ready\ndatabase: ok\nswitch: up\nmigrations: 33/33\nswitch-uuid: unknown\nbot-rtp-range: unknown\n"
	if body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
}

// The switch being down is reported and does not fail the probe: the API
// still serves, and restarting the app would not bring the switch back.
func TestASwitchThatIsDownIsReportedButStillReady(t *testing.T) {
	code, body := readyz(t, Readiness{
		Ping:       func(context.Context) error { return nil },
		IsSwitchUp: func() bool { return false },
		Migrations: migrationsAt(32, 33),
	})
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	want := "ready\ndatabase: ok\nswitch: down\nmigrations: 32/33\nswitch-uuid: unknown\nbot-rtp-range: unknown\n"
	if body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
}

func TestADatabaseThatDoesNotAnswerIsNotReady(t *testing.T) {
	asked := false
	code, body := readyz(t, Readiness{
		// A multi-line error must not add lines of its own.
		Ping:       func(context.Context) error { return errors.New("dial tcp:\nconnection refused") },
		IsSwitchUp: func() bool { return true },
		Migrations: func(context.Context) (store.MigrationState, error) {
			asked = true
			return store.MigrationState{}, nil
		},
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	want := "dial tcp: connection refused\ndatabase: dial tcp: connection refused\nswitch: up\nmigrations: unknown\nswitch-uuid: unknown\nbot-rtp-range: unknown\n"
	if body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
	if asked {
		t.Fatal("asked an unreachable database for its schema version")
	}
}

func TestAMigrationStatusItCannotReadIsUnknown(t *testing.T) {
	code, body := readyz(t, Readiness{
		Ping:       func(context.Context) error { return nil },
		IsSwitchUp: func() bool { return true },
		Migrations: func(context.Context) (store.MigrationState, error) {
			return store.MigrationState{}, errors.New("permission denied")
		},
	})
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	want := "ready\ndatabase: ok\nswitch: up\nmigrations: unknown\nswitch-uuid: unknown\nbot-rtp-range: unknown\n"
	if body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
}

func uuidReadiness(st SwitchUUIDStatus) Readiness {
	return Readiness{
		Ping:       func(context.Context) error { return nil },
		IsSwitchUp: func() bool { return true },
		Migrations: migrationsAt(33, 33),
		SwitchUUID: func() SwitchUUIDStatus { return st },
	}
}

// The one switch fact that fails readiness: a switch minting anything but v7
// would lose every inbound CDR to the database's call id check.
func TestASwitchThatMintsANonV7UUIDIsNotReady(t *testing.T) {
	code, body := readyz(t, uuidReadiness(SwitchUUIDStatus{Checked: true, Version: 4}))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	want := "the switch mints UUID version 4, not 7\ndatabase: ok\nswitch: up\nmigrations: 33/33\n" +
		"switch-uuid: NOT_V7 version=4 (set uuid-version=7 in switch.conf.xml; needs FreeSWITCH 1.11 or later)\nbot-rtp-range: unknown\n"
	if body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
}

func TestASwitchThatMintsV7IsReady(t *testing.T) {
	code, body := readyz(t, uuidReadiness(SwitchUUIDStatus{Checked: true, Version: 7}))
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	want := "ready\ndatabase: ok\nswitch: up\nmigrations: 33/33\nswitch-uuid: v7\nbot-rtp-range: unknown\n"
	if body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
}

func TestAnUnknownSwitchUUIDDoesNotFailReadiness(t *testing.T) {
	code, body := readyz(t, uuidReadiness(SwitchUUIDStatus{}))
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if want := "switch-uuid: unknown\nbot-rtp-range: unknown\n"; !strings.HasSuffix(body, want) {
		t.Fatalf("body %q lacks %q", body, want)
	}
}

func botRTPReadiness(st BotRTPRangeStatus) Readiness {
	r := uuidReadiness(SwitchUUIDStatus{Checked: true, Version: 7})
	r.BotRTPRange = func() BotRTPRangeStatus { return st }
	return r
}

func TestOverlappingRTPRangesAreNotReady(t *testing.T) {
	code, body := readyz(t, botRTPReadiness(BotRTPRangeStatus{State: BotRTPRangeOverlaps, BotLow: 26000, BotHigh: 29999, SwitchStart: 16384, SwitchEnd: 32768}))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	if !strings.Contains(body, "\nbot-rtp-range: OVERLAPS_SWITCH bot=26000-29999 switch=16384-32768\n") {
		t.Fatalf("body %q lacks the overlap line", body)
	}
}

func TestEachOtherRTPRangeStateStaysReady(t *testing.T) {
	for st, want := range map[BotRTPRangeStatus]string{
		{State: BotRTPRangeOK, BotLow: 26000, BotHigh: 29999, SwitchStart: 20000, SwitchEnd: 25999}: "bot-rtp-range: ok bot=26000-29999 switch=20000-25999\n",
		{State: BotRTPRangeOff}:     "bot-rtp-range: off\n",
		{State: BotRTPRangeUnknown}: "bot-rtp-range: unknown\n",
	} {
		code, body := readyz(t, botRTPReadiness(st))
		if code != http.StatusOK || !strings.HasSuffix(body, want) {
			t.Errorf("%+v: status %d body %q, want 200 ending %q", st, code, body, want)
		}
	}
}

func TestAnOverlapDoesNotHideADatabaseFailure(t *testing.T) {
	r := botRTPReadiness(BotRTPRangeStatus{State: BotRTPRangeOverlaps})
	r.Ping = func(context.Context) error { return errors.New("down") }
	code, body := readyz(t, r)
	if code != http.StatusServiceUnavailable || !strings.HasPrefix(body, "down\n") {
		t.Errorf("status %d body %q", code, body)
	}
}
