// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// capturedSwitch answers with the replies captured from a live v0.1.1 switch
// (internal/telephony/testdata/switchstatus), so doctor is tested against what
// a real switch says rather than what somebody remembered it saying.
type capturedSwitch struct{ t *testing.T }

func (c capturedSwitch) API(cmd string) (string, error) {
	name := strings.ReplaceAll(cmd, " ", "_") + ".txt"
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "telephony", "testdata", "switchstatus", name))
	if err != nil {
		c.t.Fatalf("no captured reply for %q: %v", cmd, err)
	}
	return string(b), nil
}
func (capturedSwitch) BgAPI(string) (string, error) { return "", errors.New("read-only") }
func (capturedSwitch) IsUp() bool                   { return true }

type fakeMigrations struct {
	state store.MigrationState
	err   error
}

func (f fakeMigrations) MigrationStatus(context.Context) (store.MigrationState, error) {
	return f.state, f.err
}

// fakeSession is a provider session that only knows how to start and stop.
type fakeSession struct {
	provider.VoiceSession
	startErr error
	closed   atomic.Bool
	events   chan provider.Event
}

func (s *fakeSession) Start(context.Context, provider.SessionConfig) error { return s.startErr }
func (s *fakeSession) Events() <-chan provider.Event                       { return s.events }
func (s *fakeSession) Close(context.Context) error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.events)
	}
	return nil
}

// readyzServer answers /readyz with the bodies it is given, one per request,
// repeating the last.
func readyzServer(t *testing.T, replies ...func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := min(int(n.Add(1))-1, len(replies)-1)
		replies[i](w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readyBody(sw string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		fmt.Fprintf(w, "ready\ndatabase: ok\nswitch: %s\nmigrations: 33/33\n", sw)
	}
}

func notReady(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, "connection refused\ndatabase: connection refused\nswitch: down\nmigrations: unknown\n")
}

// harness is a healthy deployment; each test breaks one thing.
type harness struct {
	deps    doctorDeps
	opts    doctorOptions
	session *fakeSession
	sleeps  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{session: &fakeSession{events: make(chan provider.Event)}}
	now := time.Unix(1_800_000_000, 0)
	h.deps = doctorDeps{
		cfg: config.Config{
			IsBotEnabled: true,
			Provider:     "openai",
			ESLAddr:      "freeswitch:18021",
		},
		getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "sk-test"
			}
			return ""
		},
		http: &http.Client{Timeout: time.Second},
		openStore: func(context.Context) (migrationReader, func(), error) {
			return fakeMigrations{state: store.MigrationState{DBVersion: 33, LatestVersion: 33}}, func() {}, nil
		},
		dialSwitch: func(context.Context) (switchReader, func(), error) {
			return telephony.NewAdapter(capturedSwitch{t}, "aicc.test"), func() {}, nil
		},
		sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
			return h.session, nil
		},
		interfaceAddrs: func() ([]net.Addr, error) {
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
				&net.IPNet{IP: net.ParseIP("192.168.31.111").To4(), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("2001:db8::111"), Mask: net.CIDRMask(64, 128)},
			}, nil
		},
		// A fake clock: sleeping moves time on, so --wait is exercised without
		// waiting.
		sleep: func(_ context.Context, d time.Duration) bool {
			h.sleeps++
			now = now.Add(d)
			return true
		},
		now: func() time.Time { return now },
	}
	h.opts = doctorOptions{
		readyzURL:  readyzServer(t, readyBody("up")).URL + "/readyz",
		externalIP: "192.168.31.111",
	}
	return h
}

func (h *harness) run() map[DoctorCheck]doctorResult {
	results := doctor(context.Background(), h.deps, h.opts)
	byCheck := map[DoctorCheck]doctorResult{}
	for _, r := range results {
		if _, dup := byCheck[r.Check]; dup {
			panic("check reported twice: " + string(r.Check))
		}
		byCheck[r.Check] = r
	}
	return byCheck
}

func wantState(t *testing.T, got map[DoctorCheck]doctorResult, check DoctorCheck, state DoctorState) {
	t.Helper()
	r, ok := got[check]
	if !ok {
		t.Fatalf("%s was not reported", check)
	}
	if r.State != state {
		t.Errorf("%s = %s (%s), want %s", check, r.State, r.Message, state)
	}
	switch {
	case r.State == DoctorStateFail && r.Fix == "":
		t.Errorf("%s failed without saying how to fix it", check)
	case r.State == DoctorStateFail && r.Code != codeFor(check):
		t.Errorf("%s failed with code %q, want %q", check, r.Code, codeFor(check))
	case r.State != DoctorStateFail && (r.Code != "" || r.Fix != ""):
		t.Errorf("%s is %s but carries code %q / fix %q", check, r.State, r.Code, r.Fix)
	}
}

func TestAHealthyDeploymentPassesEveryCheckItCanAsk(t *testing.T) {
	h := newHarness(t)
	got := h.run()

	if len(got) != len(doctorChecks) {
		t.Fatalf("reported %d checks, want every one of %d", len(got), len(doctorChecks))
	}
	for _, c := range doctorChecks {
		wantState(t, got, c.check, DoctorStatePass)
	}
	if !h.session.closed.Load() {
		t.Error("the provider session was left open")
	}
}

func TestTheAppsSwitchLinkIsReadFromReadyz(t *testing.T) {
	h := newHarness(t)
	h.opts.readyzURL = readyzServer(t, readyBody("down")).URL
	got := h.run()
	wantState(t, got, DoctorCheckApp, DoctorStatePass)
	wantState(t, got, DoctorCheckSwitchLink, DoctorStateFail)
}

// An application older than this doctor answers "ready" and nothing else. Its
// link is unknown, which is not the same as down.
func TestAnAppThatDoesNotReportItsSwitchLinkIsSkippedNotFailed(t *testing.T) {
	h := newHarness(t)
	h.opts.readyzURL = readyzServer(t, func(w http.ResponseWriter) { fmt.Fprint(w, "ready\n") }).URL
	got := h.run()
	wantState(t, got, DoctorCheckApp, DoctorStatePass)
	wantState(t, got, DoctorCheckSwitchLink, DoctorStateSkip)
}

func TestAnAppThatDoesNotAnswerFails(t *testing.T) {
	h := newHarness(t)
	srv := readyzServer(t, readyBody("up"))
	srv.Close()
	h.opts.readyzURL = srv.URL
	got := h.run()
	wantState(t, got, DoctorCheckApp, DoctorStateFail)
	wantState(t, got, DoctorCheckSwitchLink, DoctorStateSkip)
}

func TestWaitKeepsAskingUntilTheSwitchLinkComesUp(t *testing.T) {
	h := newHarness(t)
	h.opts.wait = 30 * time.Second
	h.opts.readyzURL = readyzServer(t, notReady, readyBody("down"), readyBody("up")).URL
	got := h.run()
	wantState(t, got, DoctorCheckApp, DoctorStatePass)
	wantState(t, got, DoctorCheckSwitchLink, DoctorStatePass)
	if h.sleeps != 2 {
		t.Errorf("slept %d times, want 2", h.sleeps)
	}
}

func TestWaitGivesUpOnAnAppThatIsNeverReady(t *testing.T) {
	h := newHarness(t)
	h.opts.wait = 5 * time.Second
	h.opts.readyzURL = readyzServer(t, notReady).URL
	got := h.run()
	wantState(t, got, DoctorCheckApp, DoctorStateFail)
	wantState(t, got, DoctorCheckSwitchLink, DoctorStateFail)
	if h.sleeps != 5 {
		t.Errorf("slept %d times, want 5", h.sleeps)
	}
}

func TestWithoutWaitReadyzIsAskedOnce(t *testing.T) {
	h := newHarness(t)
	h.opts.readyzURL = readyzServer(t, notReady, readyBody("up")).URL
	got := h.run()
	wantState(t, got, DoctorCheckApp, DoctorStateFail)
	if h.sleeps != 0 {
		t.Errorf("slept %d times without --wait", h.sleeps)
	}
}

func TestTheSchemaIsComparedWithThisBinary(t *testing.T) {
	cases := []struct {
		name             string
		state            store.MigrationState
		pending, isNewer DoctorState
	}{
		{"current", store.MigrationState{DBVersion: 33, LatestVersion: 33}, DoctorStatePass, DoctorStatePass},
		{"pending", store.MigrationState{DBVersion: 0, LatestVersion: 33, HasPending: true}, DoctorStateFail, DoctorStatePass},
		{"newer", store.MigrationState{DBVersion: 34, LatestVersion: 33}, DoctorStatePass, DoctorStateFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.openStore = func(context.Context) (migrationReader, func(), error) {
				return fakeMigrations{state: tc.state}, func() {}, nil
			}
			got := h.run()
			wantState(t, got, DoctorCheckDatabase, DoctorStatePass)
			wantState(t, got, DoctorCheckMigrations, tc.pending)
			wantState(t, got, DoctorCheckSchema, tc.isNewer)
		})
	}
}

func TestAnUnreachableDatabaseFailsOnceAndSkipsWhatDependsOnIt(t *testing.T) {
	h := newHarness(t)
	h.deps.openStore = func(context.Context) (migrationReader, func(), error) {
		return nil, nil, errors.New("dial tcp 10.130.0.3:5432: connect: connection refused")
	}
	got := h.run()
	wantState(t, got, DoctorCheckDatabase, DoctorStateFail)
	wantState(t, got, DoctorCheckMigrations, DoctorStateSkip)
	wantState(t, got, DoctorCheckSchema, DoctorStateSkip)
}

func TestAWrongSwitchPasswordIsNamedAsSuch(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return nil, nil, esl.ErrAuthFailed
	}
	got := h.run()
	wantState(t, got, DoctorCheckSwitchReachable, DoctorStatePass)
	wantState(t, got, DoctorCheckSwitchAuth, DoctorStateFail)
	wantState(t, got, DoctorCheckSIPProfiles, DoctorStateSkip)
	wantState(t, got, DoctorCheckBotGateway, DoctorStateSkip)
	wantState(t, got, DoctorCheckExternalIPAdvertised, DoctorStateSkip)
}

func TestASwitchThatDoesNotAnswerIsNotCalledAnAuthFailure(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return nil, nil, errors.New("dial esl: dial tcp: lookup freeswitch: no such host")
	}
	got := h.run()
	wantState(t, got, DoctorCheckSwitchReachable, DoctorStateFail)
	wantState(t, got, DoctorCheckSwitchAuth, DoctorStateSkip)
}

// fakeStatus is a switch whose answers a test chooses.
type fakeStatus struct {
	profiles []telephony.SwitchProfile
	gateway  bool
	gwErr    error
}

func (f fakeStatus) Profiles() ([]telephony.SwitchProfile, error) { return f.profiles, nil }
func (f fakeStatus) GatewayUp(string) (bool, error)               { return f.gateway, f.gwErr }

func TestAStoppedProfileAndADeadBotGatewayFail(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: []telephony.SwitchProfile{
			{Name: "internal", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
			{Name: "external", IsRunning: false},
		}}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckSIPProfiles, DoctorStateFail)
	wantState(t, got, DoctorCheckBotGateway, DoctorStateFail)
	if !strings.Contains(got[DoctorCheckSIPProfiles].Message, "external") {
		t.Errorf("the message does not name the profile: %q", got[DoctorCheckSIPProfiles].Message)
	}
}

func TestAMissingBotGatewayFails(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{
			profiles: []telephony.SwitchProfile{
				{Name: "internal", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
				{Name: "external", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
			},
			gwErr: fmt.Errorf("aicc_bot: %w", telephony.ErrUnknownGateway),
		}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckSIPProfiles, DoctorStatePass)
	wantState(t, got, DoctorCheckBotGateway, DoctorStateFail)
}

// recoveringGateway answers DOWN twice, then UP: the state a switch shows
// after the application is recreated, until its next OPTIONS ping.
type recoveringGateway struct {
	fakeStatus
	calls int
}

func (g *recoveringGateway) GatewayUp(string) (bool, error) {
	g.calls++
	return g.calls > 2, nil
}

func TestWaitLetsTheBotGatewayComeBackAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.opts.wait = 5 * time.Minute
	gw := &recoveringGateway{fakeStatus: fakeStatus{profiles: []telephony.SwitchProfile{
		{Name: "internal", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
		{Name: "external", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
	}}}
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return gw, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckBotGateway, DoctorStatePass)
	if gw.calls < 3 {
		t.Errorf("the gateway was asked %d times, want until it answered UP", gw.calls)
	}
}

func TestADownBotGatewayStillFailsInsideTheWaitGrace(t *testing.T) {
	h := newHarness(t)
	h.opts.wait = time.Hour
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: []telephony.SwitchProfile{
			{Name: "internal", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
			{Name: "external", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
		}}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckBotGateway, DoctorStateFail)
	if h.sleeps == 0 {
		t.Error("doctor never retried a gateway that reads DOWN")
	}
	if h.sleeps > 60 {
		t.Errorf("doctor waited %d sleeps; the grace must stay bounded, not follow --wait", h.sleeps)
	}
}

func TestTheExternalAddressIsLookedForAmongTheHostsOwn(t *testing.T) {
	cases := []struct {
		name       string
		externalIP string
		hostAddrs  []string
		want       DoctorState
	}{
		{"v4 on an interface", "192.168.31.111", nil, DoctorStatePass},
		{"v6 on an interface, written differently", "2001:db8:0:0::111", nil, DoctorStatePass},
		{"not on any interface", "203.0.113.9", nil, DoctorStateFail},
		{"given by the installer", "203.0.113.9", []string{"10.0.0.2", "203.0.113.9"}, DoctorStatePass},
		{"the installer's list wins", "192.168.31.111", []string{"10.0.0.2"}, DoctorStateFail},
		{"v6 given by the installer", "2001:db8::9", []string{"2001:DB8::9"}, DoctorStatePass},
		{"not an address", "phones.example.com", nil, DoctorStateFail},
		{"not set", "", nil, DoctorStateSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.opts.externalIP = tc.externalIP
			h.opts.hostAddrs = tc.hostAddrs
			wantState(t, h.run(), DoctorCheckExternalIP, tc.want)
		})
	}
}

// The captured switch advertises 192.168.31.111; an operator who changed
// FS_EXTERNAL_IP and only restarted the switch still has the old one there.
func TestASwitchStillAdvertisingAnOldAddressIsStale(t *testing.T) {
	h := newHarness(t)
	h.opts.externalIP = "203.0.113.9"
	h.opts.hostAddrs = []string{"203.0.113.9"}
	got := h.run()
	wantState(t, got, DoctorCheckExternalIP, DoctorStatePass)
	wantState(t, got, DoctorCheckExternalIPAdvertised, DoctorStateFail)
	if !strings.Contains(got[DoctorCheckExternalIPAdvertised].Message, "192.168.31.111") {
		t.Errorf("the message does not say what the switch advertises: %q",
			got[DoctorCheckExternalIPAdvertised].Message)
	}
}

func TestTheProviderChecks(t *testing.T) {
	cases := []struct {
		name         string
		arrange      func(h *harness)
		key, session DoctorState
	}{
		{"a session opens", func(*harness) {}, DoctorStatePass, DoctorStatePass},
		{"the provider refuses the session", func(h *harness) {
			h.session.startErr = errors.New("realtime: 401 invalid api key")
		}, DoctorStatePass, DoctorStateFail},
		{"the client cannot be built", func(h *harness) {
			h.deps.sessions = func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
				return nil, errors.New("bad endpoint")
			}
		}, DoctorStatePass, DoctorStateFail},
		{"no key", func(h *harness) {
			h.deps.getenv = func(string) string { return "" }
		}, DoctorStateFail, DoctorStateSkip},
		{"--skip-provider", func(h *harness) { h.opts.skipProvider = true }, DoctorStatePass, DoctorStateSkip},
		{"the AI leg is off", func(h *harness) { h.deps.cfg.IsBotEnabled = false }, DoctorStateSkip, DoctorStateSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			got := h.run()
			wantState(t, got, DoctorCheckProviderKey, tc.key)
			wantState(t, got, DoctorCheckProviderSession, tc.session)
			if tc.session == DoctorStateFail && h.session.startErr != nil && !h.session.closed.Load() {
				t.Error("a session that failed to start was not closed")
			}
		})
	}
}

func TestTheBotGatewayIsNotAskedAboutWhenTheAILegIsOff(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.IsBotEnabled = false
	wantState(t, h.run(), DoctorCheckBotGateway, DoctorStateSkip)
}

// An installer matches on these, so they follow the naming spec's enum rule
// and never carry the switch's own vocabulary.
func TestEveryDoctorCodeIsScreamingSnakeAndUnique(t *testing.T) {
	screaming := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	seen := map[DoctorCode]bool{}
	for _, c := range doctorChecks {
		code := c.code
		if !screaming.MatchString(string(code)) {
			t.Errorf("%q is not SCREAMING_SNAKE_CASE", code)
		}
		for _, upstream := range []string{"ESL", "SOFIA", "FREESWITCH", "FS"} {
			for _, word := range strings.Split(string(code), "_") {
				if word == upstream {
					t.Errorf("%q carries the upstream token %s", code, upstream)
				}
			}
		}
		if seen[code] {
			t.Errorf("%q is listed twice", code)
		}
		seen[code] = true
	}
	for _, state := range []DoctorState{DoctorStatePass, DoctorStateFail, DoctorStateSkip} {
		if !screaming.MatchString(string(state)) {
			t.Errorf("state %q is not SCREAMING_SNAKE_CASE", state)
		}
	}
}

// Every result carries its check's name, in every state, so the names are
// stable lower_snake and free of the switch's own vocabulary too.
func TestEveryDoctorCheckIsLowerSnakeAndUnique(t *testing.T) {
	lowerSnake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	seen := map[DoctorCheck]bool{}
	for _, c := range doctorChecks {
		if !lowerSnake.MatchString(string(c.check)) {
			t.Errorf("%q is not lower_snake_case", c.check)
		}
		for _, upstream := range []string{"esl", "sofia", "freeswitch", "fs"} {
			for _, word := range strings.Split(string(c.check), "_") {
				if word == upstream {
					t.Errorf("%q carries the upstream token %s", c.check, upstream)
				}
			}
		}
		if seen[c.check] {
			t.Errorf("%q is listed twice", c.check)
		}
		seen[c.check] = true
	}
}

func TestTheReadyzURLFollowsTheOpsListener(t *testing.T) {
	cases := map[string]string{
		":9090":          "http://127.0.0.1:9090/readyz",
		"0.0.0.0:9090":   "http://127.0.0.1:9090/readyz",
		"[::]:9091":      "http://127.0.0.1:9091/readyz",
		"127.0.0.1:9090": "http://127.0.0.1:9090/readyz",
		"10.0.0.5:9095":  "http://10.0.0.5:9095/readyz",
		"[::1]:9090":     "http://[::1]:9090/readyz",
	}
	for addr, want := range cases {
		if got := readyzURLFor(addr); got != want {
			t.Errorf("readyzURLFor(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestACodeIsPrintedOnlyOnAFailure(t *testing.T) {
	results := []doctorResult{
		pass(DoctorCheckApp, "the app is ready"),
		fail(DoctorCheckSwitchLink, "the app has no link to the switch", "start the switch"),
		skip(DoctorCheckExternalIP, "neither FS_EXTERNAL_IP nor --external-ip is set"),
	}
	var buf bytes.Buffer
	if err := printDoctor(&buf, results, false); err != nil {
		t.Fatal(err)
	}
	want := "PASS  app                     the app is ready\n" +
		"FAIL  switch_link             SWITCH_DOWN  the app has no link to the switch\n" +
		"      fix: start the switch\n" +
		"SKIP  external_ip             neither FS_EXTERNAL_IP nor --external-ip is set\n"
	if buf.String() != want {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), want)
	}

	buf.Reset()
	if err := printDoctor(&buf, results, true); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	wantJSON := []map[string]string{
		{"check": "app", "state": "PASS", "message": "the app is ready"},
		{"check": "switch_link", "state": "FAIL", "message": "the app has no link to the switch",
			"code": "SWITCH_DOWN", "fix": "start the switch"},
		{"check": "external_ip", "state": "SKIP", "message": "neither FS_EXTERNAL_IP nor --external-ip is set"},
	}
	if !reflect.DeepEqual(doc.Results, wantJSON) {
		t.Errorf("JSON results = %v, want %v", doc.Results, wantJSON)
	}

	if n := countFailed([]doctorResult{fail(DoctorCheckSwitchLink, "", "x"), skip(DoctorCheckSchema, "")}); n != 1 {
		t.Errorf("countFailed = %d, want 1 — a SKIP is not a failure", n)
	}
}
