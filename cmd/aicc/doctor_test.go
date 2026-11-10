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
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// capturedSwitch answers with the replies captured from a live v0.1.1 switch
// (internal/telephony/testdata/switchstatus), so doctor is tested against what
// a real switch says rather than what somebody remembered it saying.
type capturedSwitch struct {
	t *testing.T
	// conf is the switch.conf XML xml_locate answers with; empty is a switch
	// on the image's default range.
	conf string
}

func (c capturedSwitch) API(cmd string) (string, error) {
	if strings.HasPrefix(cmd, "xml_locate configuration") {
		if c.conf != "" {
			return c.conf, nil
		}
		return switchConfXML("20000", "25999"), nil
	}
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
	// outsideUS is how many do-not-call numbers lie outside +1; outsideErr is
	// what reading them answers.
	outsideUS  int64
	outsideErr error
}

func (f fakeMigrations) DNCCountOutsideCountryCode(context.Context, string) (int64, error) {
	return f.outsideUS, f.outsideErr
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
			IsBotEnabled:   true,
			BotRTPPortLow:  26000,
			BotRTPPortHigh: 29999,
			Provider:       "openai",
			ESLAddr:        "freeswitch:18021",
			// The captured switch knows this gateway, so a healthy deployment
			// passes the outbound checks as it passes the bot gateway's.
			OutboundEndpoint: "sofia/gateway/aicc_bot/%s",
		},
		getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "sk-test"
			}
			return ""
		},
		http: &http.Client{Timeout: time.Second},
		// A Linux default, so the harness never reads the machine running it.
		ephemeralPorts: func() (int, int, error) { return 32768, 60999, nil },
		openStore: func(context.Context) (migrationReader, func(), error) {
			return fakeMigrations{state: store.MigrationState{DBVersion: 33, LatestVersion: 33}}, func() {}, nil
		},
		dialSwitch: func(context.Context) (switchReader, func(), error) {
			return telephony.NewAdapter(capturedSwitch{t: t}, "aicc.test"), func() {}, nil
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
	// A healthy deployment records to S3 through a spool, the one mode in
	// which this application uploads.
	h.withRecordings("/spool", &fakeRecordings{backend: "S3"})
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
	case (r.State == DoctorStateFail || r.State == DoctorStateWarn) && r.Fix == "":
		t.Errorf("%s failed without saying how to fix it", check)
	case (r.State == DoctorStateFail || r.State == DoctorStateWarn) && r.Code != codeFor(check):
		t.Errorf("%s failed with code %q, want %q", check, r.Code, codeFor(check))
	case r.State != DoctorStateFail && r.State != DoctorStateWarn && (r.Code != "" || r.Fix != ""):
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
	// trunks is the switch's gateway table, which carries both the names that
	// exist and whether each would place a call now.
	trunks    []telephony.Trunk
	trunksErr error
	// conf and confErr answer the switch.conf lookup.
	conf    string
	confErr error
	// asked, when set, collects the names GatewayUp is asked about.
	asked *[]string
	// uuidVersion is what the switch mints; zero means v7.
	uuidVersion int
	uuidErr     error
}

func (f fakeStatus) UUIDVersion() (int, error) {
	if f.uuidVersion == 0 {
		return 7, f.uuidErr
	}
	return f.uuidVersion, f.uuidErr
}

func (f fakeStatus) Profiles() ([]telephony.SwitchProfile, error) { return f.profiles, nil }
func (f fakeStatus) GatewayUp(name string) (bool, error) {
	if f.asked != nil {
		*f.asked = append(*f.asked, name)
	}
	return f.gateway, f.gwErr
}
func (f fakeStatus) Trunks() ([]telephony.Trunk, error) { return f.trunks, f.trunksErr }
func (f fakeStatus) SwitchConf() (string, error)        { return f.conf, f.confErr }

// healthyTrunks is a switch holding the bot gateway (which never registers)
// and one carrier gateway that does.
func healthyTrunks() []telephony.Trunk {
	return []telephony.Trunk{
		{Name: "aicc_bot", Profile: "external", State: "NOREG", IsUp: true},
		{Name: "carrier_b", Profile: "external", State: "REGED", IsUp: true},
	}
}

// externalProfiles is the listener table checkSwitch asks for; the outbound
// checks only need it to be there and running.
func externalProfiles() []telephony.SwitchProfile {
	return []telephony.SwitchProfile{
		{Name: "internal", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
		{Name: "external", IsRunning: true, AdvertisedMediaIP: "192.168.31.111"},
	}
}

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

// Linux makes the whole prefix of a loopback address local, so 127.0.0.2 is on
// the host though only 127.0.0.1/8 is listed. A non-loopback network is not
// like that: a /24 on eth0 makes only its own address local.
func TestALoopbackRangeIsTheHostsOwn(t *testing.T) {
	ipnet := func(cidr string) net.Addr {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	cases := []struct {
		name       string
		externalIP string
		hostAddrs  []string
		ifaces     []net.Addr
		want       DoctorState
		message    string
	}{
		{"a range from the installer", "127.0.0.2", []string{"127.0.0.1", "127.0.0.0/8"}, nil,
			DoctorStatePass, "127.0.0.2 is within 127.0.0.0/8 of --host-addrs"},
		{"the installer lists only 127.0.0.1", "127.0.0.2", []string{"127.0.0.1"}, nil, DoctorStateFail, ""},
		{"outside the range", "128.0.0.2", []string{"127.0.0.0/8"}, nil, DoctorStateFail, ""},
		{"a non-loopback interface covers only itself", "10.0.0.9", nil,
			[]net.Addr{ipnet("10.0.0.2/24")}, DoctorStateFail, ""},
		{"a non-loopback interface address itself", "10.0.0.2", nil,
			[]net.Addr{ipnet("10.0.0.2/24")}, DoctorStatePass, ""},
		{"a loopback interface covers its network", "127.0.0.3", nil,
			[]net.Addr{ipnet("127.0.0.1/8")}, DoctorStatePass, "127.0.0.3 is within 127.0.0.0/8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.opts.externalIP = tc.externalIP
			h.opts.hostAddrs = tc.hostAddrs
			if tc.ifaces != nil {
				h.deps.interfaceAddrs = func() ([]net.Addr, error) { return tc.ifaces, nil }
			}
			got := h.run()
			wantState(t, got, DoctorCheckExternalIP, tc.want)
			if !strings.Contains(got[DoctorCheckExternalIP].Message, tc.message) {
				t.Errorf("message %q does not contain %q", got[DoctorCheckExternalIP].Message, tc.message)
			}
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
		{"grok without its key", func(h *harness) {
			h.deps.cfg.Provider = "grok"
		}, DoctorStateFail, DoctorStateSkip},
		{"grok with its key", func(h *harness) {
			h.deps.cfg.Provider = "grok"
			h.deps.getenv = func(k string) string {
				if k == "XAI_API_KEY" {
					return "xai-test"
				}
				return ""
			}
		}, DoctorStatePass, DoctorStatePass},
		{"no key", func(h *harness) {
			h.deps.getenv = func(string) string { return "" }
		}, DoctorStateFail, DoctorStateSkip},
		{"AICC_PROVIDER names a provider that was removed", func(h *harness) {
			h.deps.cfg.Provider = "gemini"
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
	for _, state := range []DoctorState{DoctorStatePass, DoctorStateFail, DoctorStateWarn, DoctorStateSkip} {
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

// Two WARNs, neither of which changes the exit status: a deployment that takes
// calls in works without an outbound endpoint, and one that runs campaigns does
// not. Doctor says which, before a campaign rings nothing.
func TestAnEmptyOutboundEndpointIsAWarningNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = ""
	got := h.run()

	wantState(t, got, DoctorCheckOutboundEndpoint, DoctorStateWarn)
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateSkip)
	if !strings.Contains(got[DoctorCheckOutboundEndpoint].Message, "AICC_OUTBOUND_ENDPOINT") {
		t.Errorf("the warning does not name the setting: %q", got[DoctorCheckOutboundEndpoint].Message)
	}
	if n := countFailed([]doctorResult{got[DoctorCheckOutboundEndpoint]}); n != 0 {
		t.Errorf("countFailed = %d, want 0: a WARN does not change the exit status", n)
	}
}

func TestAnEndpointNamingAMissingGatewayIsAWarningThatListsTheOnesThereAre(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/pstn_gatewya/%s"
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), gateway: true,
			// The names the operator's own switch has: enough to see the typo.
			trunks: healthyTrunks(),
		}, func() {}, nil
	}
	got := h.run()

	wantState(t, got, DoctorCheckOutboundEndpoint, DoctorStatePass)
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateWarn)
	msg := got[DoctorCheckOutboundGateway].Message
	for _, want := range []string{"pstn_gatewya", "aicc_bot", "carrier_b"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the warning does not mention %q: %q", want, msg)
		}
	}
	if n := countFailed([]doctorResult{got[DoctorCheckOutboundGateway]}); n != 0 {
		t.Errorf("countFailed = %d, want 0: a WARN does not change the exit status", n)
	}
}

// A carrier gateway that registers and is stuck before it ever did is not
// reachable, whatever its OPTIONS ping says: the switch's own state (FAIL_WAIT,
// UNREGED) is what a call would meet. The check reads the trunk table, not the
// ping status, so it warns here — that is the silent outbound failure it exists
// to catch (#293 review).
func TestACarrierGatewayStuckRegisteringWarnsEvenIfItAnswersPings(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/carrier_b/%s"
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{
			profiles: externalProfiles(),
			// The ping side says UP.
			gateway: true,
			trunks: []telephony.Trunk{
				{Name: "aicc_bot", Profile: "external", State: "NOREG", IsUp: true},
				{Name: "carrier_b", Profile: "external", State: "FAIL_WAIT", IsUp: false},
			},
		}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateWarn)
	msg := got[DoctorCheckOutboundGateway].Message
	if !strings.Contains(msg, "carrier_b") || !strings.Contains(msg, "FAIL_WAIT") {
		t.Errorf("the warning does not name the gateway and its state: %q", msg)
	}
}

// The switch prints a gateway as profile::name, and the endpoint may be written
// the same way. That is a gateway the switch has, not a typo.
func TestAnEndpointQualifiedWithItsProfilePassesAndNamesTheProfileOnTheSwitch(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/external::carrier_b/%s"
	var asked []string
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), gateway: true, trunks: healthyTrunks(), asked: &asked}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStatePass)
	if len(asked) == 0 || asked[len(asked)-1] != "external::carrier_b" {
		t.Errorf("the ping status was asked about %v, want external::carrier_b", asked)
	}

	// The profile counts: the same name on another profile is not this gateway.
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/internal::carrier_b/%s"
	got = h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateWarn)
}

// Doctor cannot read the trunk table: that is a warning naming the error, not a
// failure and not a pass.
func TestAnUnreadableTrunkTableIsAWarning(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/carrier_b/%s"
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), gateway: true, trunksErr: errors.New("sofia status timed out")}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateWarn)
	if !strings.Contains(got[DoctorCheckOutboundGateway].Message, "sofia status timed out") {
		t.Errorf("the warning does not carry the error: %q", got[DoctorCheckOutboundGateway].Message)
	}
}

// A gateway that never registers (NOREG) is dialable only while it answers the
// switch's OPTIONS pings: once the carrier stops, sofia refuses every originate
// through it, though the trunk table still says NOREG. Passing on the table
// alone would call that a healthy trunk (#293 review).
func TestANoregGatewayWhosePingIsDownWarns(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/carrier_b/%s"
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{
			profiles: externalProfiles(),
			gateway:  false,
			trunks: []telephony.Trunk{
				{Name: "carrier_b", Profile: "external", State: "NOREG", IsUp: true},
			},
		}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateWarn)
	msg := got[DoctorCheckOutboundGateway].Message
	if !strings.Contains(msg, "carrier_b") || !strings.Contains(msg, "NOREG") || !strings.Contains(msg, "DOWN") {
		t.Errorf("the warning does not name the gateway, its state and the ping: %q", msg)
	}
	if n := countFailed([]doctorResult{got[DoctorCheckOutboundGateway]}); n != 0 {
		t.Errorf("countFailed = %d, want 0", n)
	}
}

// An error reading the ping status is a warning too.
func TestAnUnreadablePingStatusIsAWarning(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/gateway/carrier_b/%s"
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), trunks: healthyTrunks(), gwErr: errors.New("reply lost")}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateWarn)
	if !strings.Contains(got[DoctorCheckOutboundGateway].Message, "reply lost") {
		t.Errorf("the warning does not carry the error: %q", got[DoctorCheckOutboundGateway].Message)
	}
}

// The upgrade hazard of the built-in country code: a deployment that never set
// AICC_DEFAULT_COUNTRY_CODE now completes national numbers to +1, and a
// do-not-call list in another country stops matching them. A WARN, never a
// failure; silent when the operator chose the code or the list is all +1.
func TestAnUnsetCountryCodeWithForeignDoNotCallNumbersWarnsButDoesNotFail(t *testing.T) {
	ok := store.MigrationState{DBVersion: 33, LatestVersion: 33}
	cases := []struct {
		name  string
		isSet bool
		db    fakeMigrations
		want  DoctorState
	}{
		{"unset, numbers outside +1", false, fakeMigrations{state: ok, outsideUS: 3}, DoctorStateWarn},
		{"set, numbers outside +1", true, fakeMigrations{state: ok, outsideUS: 3}, DoctorStatePass},
		{"unset, every number in +1", false, fakeMigrations{state: ok}, DoctorStatePass},
		{"unset, list unreadable", false, fakeMigrations{state: ok, outsideErr: errors.New("denied")}, DoctorStateSkip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.cfg.DefaultCountryCode = "1"
			h.deps.cfg.IsDefaultCountryCodeSet = c.isSet
			h.deps.openStore = func(context.Context) (migrationReader, func(), error) {
				return c.db, func() {}, nil
			}
			got := h.run()
			wantState(t, got, DoctorCheckDNCCountryCode, c.want)
			if c.want == DoctorStateWarn {
				r := got[DoctorCheckDNCCountryCode]
				for _, want := range []string{"+1", "take_message", "AICC_DEFAULT_COUNTRY_CODE"} {
					if !strings.Contains(r.Message+r.Fix, want) {
						t.Errorf("the warning does not mention %q: %+v", want, r)
					}
				}
				if r.Code != "DNC_COUNTRY_CODE_UNSET" {
					t.Errorf("code = %q, want DNC_COUNTRY_CODE_UNSET", r.Code)
				}
				if n := countFailed([]doctorResult{r}); n != 0 {
					t.Errorf("countFailed = %d, want 0: a WARN does not change the exit status", n)
				}
			}
		})
	}
}

// A gateway that never registers by design (NOREG) is a trunk as it stands:
// calling it down would report a fault on every healthy deployment.
func TestAGatewayThatNeverRegistersPasses(t *testing.T) {
	h := newHarness(t)
	got := h.run()
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStatePass)
}

// An endpoint that is not a gateway (a direct sip: dial string) has no gateway
// to look up, and doctor says so rather than inventing a failure.
func TestAnEndpointThatNamesNoGatewaySkipsTheGatewayCheck(t *testing.T) {
	h := newHarness(t)
	h.deps.cfg.OutboundEndpoint = "sofia/external/2125550101@10.0.0.9"
	got := h.run()
	wantState(t, got, DoctorCheckOutboundEndpoint, DoctorStatePass)
	wantState(t, got, DoctorCheckOutboundGateway, DoctorStateSkip)
}

func TestTheGatewayNameIsReadOutOfTheDialString(t *testing.T) {
	cases := []struct {
		endpoint string
		name     string
		profile  string
		ok       bool
	}{
		{"sofia/gateway/pstn_gateway/%s", "pstn_gateway", "", true},
		{"sofia/gateway/pstn_gateway/+1%s", "pstn_gateway", "", true},
		{"sofia/gateway/external::pstn_gateway/%s", "pstn_gateway", "external", true},
		{"sofia/gateway/::pstn_gateway/%s", "", "", false},
		{"sofia/gateway/external::/%s", "", "", false},
		{"sofia/gateway//%s", "", "", false},
		{"sofia/gateway/pstn_gateway", "", "", false},
		{"sofia/external/sip:2125550101@10.0.0.9", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		name, profile, ok := outboundGatewayOf(c.endpoint)
		if name != c.name || profile != c.profile || ok != c.ok {
			t.Errorf("outboundGatewayOf(%q) = %q, %q, %v; want %q, %q, %v",
				c.endpoint, name, profile, ok, c.name, c.profile, c.ok)
		}
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

// switchConfXML is what xml_locate prints for switch.conf: the whole tree, of
// which the two RTP params are the part doctor reads. An empty value leaves the
// param out, as a switch.conf that does not set it.
func switchConfXML(start, end string) string {
	var params string
	if end != "" {
		params += `      <param name="rtp-end-port" value="` + end + `"></param>` + "\n"
	}
	if start != "" {
		params += `      <param name="rtp-start-port" value="` + start + `"></param>` + "\n"
	}
	return `<configuration name="switch.conf" description="Core Configuration">
  <cli-keybindings>
    <key name="1" value="help"/>
  </cli-keybindings>
  <settings>
      <param name="colorize-console" value="true"></param>
` + params + `      <param name="max-sessions" value="1000"></param>
  </settings>
</configuration>
`
}

func rtpHarness(t *testing.T, conf string, botMaxCalls int, isBotEnabled bool) doctorResult {
	t.Helper()
	h := newHarness(t)
	h.deps.cfg.IsBotEnabled = isBotEnabled
	h.deps.cfg.BotMaxCalls = botMaxCalls
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return telephony.NewAdapter(capturedSwitch{t: t, conf: conf}, "aicc.test"), func() {}, nil
	}
	r, ok := h.run()[DoctorCheckSwitchRTPRange]
	if !ok {
		t.Fatal("switch_rtp_range was not reported")
	}
	return r
}

func TestTheSwitchRTPRangeIsReadAndCounted(t *testing.T) {
	const eavesdrop = "a call holds two (the caller and the agent or the bot), a supervisor's eavesdrop adds one"
	cases := []struct {
		name       string
		start, end string
		bot        int
		isBot      bool
		wantState  DoctorState
		wantCode   DoctorCode
		wantInMsg  []string
	}{
		{name: "unset is the built-in default", wantState: DoctorStatePass, isBot: true,
			wantInMsg: []string{"sets no RTP range", "built-in 16384–32768", "8,193 media legs", eavesdrop}},
		{name: "the image default", start: "20000", end: "25999", bot: 220, isBot: true, wantState: DoctorStatePass,
			wantInMsg: []string{"20000–25999, RTP on the even ports 20000–25998", "3,000 media legs", eavesdrop}},
		{name: "macOS", start: "16384", end: "16583", bot: 50, isBot: true, wantState: DoctorStatePass,
			wantInMsg: []string{"16384–16583", "100 media legs"}},
		{name: "quick start range with the default bot cap is narrow", start: "16384", end: "16483", bot: 220, isBot: true,
			wantState: DoctorStateWarn, wantCode: DoctorCodeSwitchRTPRangeNarrow,
			wantInMsg: []string{"50 media legs", "AICC_BOT_MAX_CALLS", "440"}},
		{name: "odd start", start: "16385", end: "16484", isBot: false, wantState: DoctorStatePass,
			wantInMsg: []string{"16385–16484, RTP on the even ports 16386–16484", "50 media legs"}},
		{name: "end equals an even start holds one leg", start: "16384", end: "16384", isBot: false, wantState: DoctorStatePass,
			wantInMsg: []string{"1 media legs"}},
		{name: "end equals an odd start holds none", start: "16385", end: "16385", isBot: false,
			wantState: DoctorStateFail, wantCode: DoctorCodeSwitchRTPRangeInvalid, wantInMsg: []string{"no even port pair", "0 media legs"}},
		{name: "inverted", start: "16394", end: "16384", isBot: true, wantState: DoctorStateFail, wantCode: DoctorCodeSwitchRTPRangeInvalid,
			wantInMsg: []string{"16394–16384", "does not refuse it", "anywhere in 1–65535", "memory"}},
		{name: "very wide", start: "1024", end: "65535", bot: 220, isBot: true, wantState: DoctorStatePass,
			wantInMsg: []string{"1024–65535, RTP on the even ports 1024–65534", "32,256 media legs"}},
		{name: "non-numeric", start: "sixteen", end: "29999", isBot: true, wantState: DoctorStateFail, wantCode: DoctorCodeSwitchRTPRangeInvalid,
			wantInMsg: []string{"rtp-start-port", "sixteen"}},
		{name: "out of range", start: "16384", end: "70000", isBot: true, wantState: DoctorStateFail, wantCode: DoctorCodeSwitchRTPRangeInvalid,
			wantInMsg: []string{"rtp-end-port", "70000"}},
		{name: "bot off is never narrow", start: "16384", end: "16483", bot: 220, isBot: false, wantState: DoctorStatePass},
		{name: "exactly two legs per bot call is enough", start: "16384", end: "16483", bot: 25, isBot: true, wantState: DoctorStatePass},
		{name: "one leg short of it is narrow", start: "16384", end: "16481", bot: 25, isBot: true,
			wantState: DoctorStateWarn, wantCode: DoctorCodeSwitchRTPRangeNarrow, wantInMsg: []string{"49 media legs", "50 legs"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rtpHarness(t, switchConfXML(tc.start, tc.end), tc.bot, tc.isBot)
			if r.State != tc.wantState || r.Code != tc.wantCode {
				t.Fatalf("state %s code %q (%s), want %s %q", r.State, r.Code, r.Message, tc.wantState, tc.wantCode)
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message %q lacks %q", r.Message, want)
				}
			}
			if (r.State == DoctorStateFail || r.State == DoctorStateWarn) && r.Fix == "" {
				t.Error("no fix")
			}
			if r.State == DoctorStatePass && (r.Code != "" || r.Fix != "") {
				t.Errorf("a PASS carries code %q / fix %q", r.Code, r.Fix)
			}
		})
	}
}

func TestAHalfSetRangeSaysWhichSideIsTheDefault(t *testing.T) {
	r := rtpHarness(t, switchConfXML("", "29999"), 0, false)
	if r.State != DoctorStatePass || !strings.Contains(r.Message, "rtp-start-port is unset: FreeSWITCH's built-in 16384") {
		t.Errorf("got %s %q", r.State, r.Message)
	}
	// An end below the built-in start is an inverted range, whoever set it.
	r = rtpHarness(t, switchConfXML("", "16000"), 0, false)
	if r.State != DoctorStateFail {
		t.Errorf("got %s %q, want FAIL", r.State, r.Message)
	}
}

func TestAnUnreadableSwitchConfIsAWarningNotAFailure(t *testing.T) {
	for name, conf := range map[string]string{
		"error reply":  "-ERR no reply",
		"empty":        "",
		"other config": `<configuration name="modules.conf"><settings/></configuration>`,
		"not xml":      "garbage <<<",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
				return fakeStatus{profiles: externalProfiles(), gateway: true, trunks: healthyTrunks(), conf: conf}, func() {}, nil
			}
			wantState(t, h.run(), DoctorCheckSwitchRTPRange, DoctorStateWarn)
		})
	}
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), gateway: true, trunks: healthyTrunks(), confErr: errors.New("link dropped")}, func() {}, nil
	}
	wantState(t, h.run(), DoctorCheckSwitchRTPRange, DoctorStateWarn)
}

func TestTheRTPRangeIsSkippedWhenTheSwitchWasNotReachable(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return nil, nil, errors.New("connection refused")
	}
	wantState(t, h.run(), DoctorCheckSwitchRTPRange, DoctorStateSkip)
}

func TestTheNarrowWarningDoesNotChangeTheExitStatus(t *testing.T) {
	r := rtpHarness(t, switchConfXML("16384", "16483"), 220, true)
	if r.State != DoctorStateWarn {
		t.Fatalf("state %s", r.State)
	}
	if countFailed([]doctorResult{r}) != 0 {
		t.Error("a WARN counted as a failure")
	}
}

// The captured reply is a real switch's create_uuid: a v7.
func TestASwitchThatMintsV7PassesTheUUIDCheck(t *testing.T) {
	h := newHarness(t)
	got := h.run()
	wantState(t, got, DoctorCheckSwitchUUID, DoctorStatePass)
}

func TestASwitchThatMintsV4FailsTheUUIDCheck(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), gateway: true, trunks: healthyTrunks(), uuidVersion: 4}, func() {}, nil
	}
	got := h.run()
	wantState(t, got, DoctorCheckSwitchUUID, DoctorStateFail)
	r := got[DoctorCheckSwitchUUID]
	if r.Code != DoctorCodeSwitchUUIDNotV7 || !strings.Contains(r.Fix, "uuid-version=7") {
		t.Fatalf("code %q fix %q", r.Code, r.Fix)
	}
}

func TestAUUIDItCannotReadIsSkippedNotPassed(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return fakeStatus{profiles: externalProfiles(), gateway: true, trunks: healthyTrunks(), uuidErr: errors.New("timeout")}, func() {}, nil
	}
	wantState(t, h.run(), DoctorCheckSwitchUUID, DoctorStateSkip)
}

// botRangeRun runs doctor against a switch with the given switch.conf and the
// bot on the given range, and returns the two bot-range results.
func botRangeRun(t *testing.T, conf string, low, high int, isBot bool, eph func() (int, int, error)) map[DoctorCheck]doctorResult {
	t.Helper()
	h := newHarness(t)
	h.deps.cfg.IsBotEnabled = isBot
	h.deps.cfg.BotRTPPortLow, h.deps.cfg.BotRTPPortHigh = low, high
	h.deps.ephemeralPorts = eph
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) {
		return telephony.NewAdapter(capturedSwitch{t: t, conf: conf}, "aicc.test"), func() {}, nil
	}
	return h.run()
}

func TestTheBotAndTheSwitchMayNotShareARTPPort(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		low, high  int
		overlap    bool
		wantInMsg  string
	}{
		{"identical", "26000", "29999", 26000, 29999, true, "bot=26000-29999, switch=26000-29999"},
		{"bot inside the switch", "20000", "32000", 26000, 29999, true, ""},
		{"switch inside the bot", "27000", "28000", 26000, 29999, true, ""},
		{"bot low equals the switch end", "20000", "26000", 26000, 29999, true, ""},
		{"bot low one past the switch end", "20000", "25999", 26000, 29999, false, "switch=20000-25999"},
		{"bot high equals the switch start", "29999", "35000", 26000, 29999, true, ""},
		{"bot high one below the switch start", "30000", "35000", 26000, 29999, false, ""},
		{"unset switch range is FreeSWITCH's built-in", "", "", 26000, 29999, true, "switch=16384-32768 (FreeSWITCH's built-in default"},
		{"unset switch range clear of a high bot", "", "", 40000, 40999, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := botRangeRun(t, switchConfXML(tc.start, tc.end), tc.low, tc.high, true, nil)
			r := got[DoctorCheckBotRTPRange]
			if tc.overlap {
				if r.State != DoctorStateFail || r.Code != DoctorCodeBotRTPRangeOverlapsSwitch ||
					!strings.Contains(r.Fix, "AICC_BOT_RTP_PORT_LOW") || !strings.Contains(r.Fix, "RTP_START") {
					t.Fatalf("%+v", r)
				}
			} else if r.State != DoctorStatePass || r.Code != "" {
				t.Fatalf("%+v", r)
			}
			if !strings.Contains(r.Message, tc.wantInMsg) {
				t.Errorf("message %q lacks %q", r.Message, tc.wantInMsg)
			}
		})
	}
}

func TestNoOverlapVerdictWithoutAnAbleBotOrASwitchToJudge(t *testing.T) {
	// The bot is off.
	got := botRangeRun(t, switchConfXML("26000", "29999"), 26000, 29999, false, nil)
	wantState(t, got, DoctorCheckBotRTPRange, DoctorStateSkip)
	wantState(t, got, DoctorCheckBotRTPEphemeral, DoctorStateSkip)
	// The switch's range is invalid: only SWITCH_RTP_RANGE_INVALID speaks.
	for _, conf := range []string{switchConfXML("sixteen", "29999"), switchConfXML("29999", "26000"), switchConfXML("26001", "26001")} {
		got = botRangeRun(t, conf, 26000, 29999, true, nil)
		wantState(t, got, DoctorCheckSwitchRTPRange, DoctorStateFail)
		wantState(t, got, DoctorCheckBotRTPRange, DoctorStateSkip)
	}
	// The conf cannot be read.
	got = botRangeRun(t, "garbage <<<", 26000, 29999, true, nil)
	wantState(t, got, DoctorCheckBotRTPRange, DoctorStateSkip)
}

func TestTheBotRangeIsSkippedWhenTheSwitchWasNotReachable(t *testing.T) {
	h := newHarness(t)
	h.deps.dialSwitch = func(context.Context) (switchReader, func(), error) { return nil, nil, errors.New("refused") }
	wantState(t, h.run(), DoctorCheckBotRTPRange, DoctorStateSkip)
}

func TestTheBotRangeInTheOSEphemeralRangeWarns(t *testing.T) {
	linux := func() (int, int, error) { return parseEphemeralLinux("32768\t60999\n") }
	macos := func() (int, int, error) { return parseEphemeralDarwin("49152\n", "65535\n") }
	cases := []struct {
		name      string
		low, high int
		eph       func() (int, int, error)
		wantWarn  bool
	}{
		{"linux, the old default", 40000, 40999, linux, true},
		{"linux, the new default", 26000, 29999, linux, false},
		{"linux, straddling the edge", 32000, 33000, linux, true},
		{"macOS, inside", 50000, 50999, macos, true},
		{"macOS, the old default is clear", 40000, 40999, macos, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := botRangeRun(t, switchConfXML("20000", "25999"), tc.low, tc.high, true, tc.eph)
			r := got[DoctorCheckBotRTPEphemeral]
			if tc.wantWarn {
				if r.State != DoctorStateWarn || r.Code != DoctorCodeBotRTPRangeEphemeral || r.Fix == "" {
					t.Fatalf("%+v", r)
				}
			} else if r.State != DoctorStatePass {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestAnUnreadableEphemeralRangeIsNotAWarning(t *testing.T) {
	got := botRangeRun(t, switchConfXML("20000", "25999"), 40000, 40999, true, func() (int, int, error) { return 0, 0, errors.New("no /proc") })
	wantState(t, got, DoctorCheckBotRTPEphemeral, DoctorStateSkip)
	got = botRangeRun(t, switchConfXML("20000", "25999"), 40000, 40999, true, nil)
	wantState(t, got, DoctorCheckBotRTPEphemeral, DoctorStateSkip)
	if _, _, err := parseEphemeralLinux("garbage"); err == nil {
		t.Error("accepted garbage")
	}
}

// fakeRecordings is a recording store with a scripted answer to the bucket
// check and the self-check.
type fakeRecordings struct {
	recording.Storage
	backend     string
	bucketErr   error
	selfCheckEr error
	selfChecks  int
}

func (f *fakeRecordings) Backend() string { return f.backend }
func (f *fakeRecordings) Bucket() string  { return "aicc-recordings" }
func (f *fakeRecordings) CheckBucket(context.Context) error {
	return f.bucketErr
}
func (f *fakeRecordings) SelfCheck(context.Context) error {
	f.selfChecks++
	return f.selfCheckEr
}

func (h *harness) withRecordings(dir string, st recording.Storage) {
	h.deps.cfg.RecordingDir = dir
	h.deps.cfg.S3Endpoint = "store.example:9000"
	h.deps.recordings = func() (recording.Storage, error) { return st, nil }
}

func TestDoctorRecordingStorageSkipsWhereTheApplicationNeverUploads(t *testing.T) {
	t.Parallel()
	cases := map[string]func(h *harness) *fakeRecordings{
		"none configured": func(h *harness) *fakeRecordings {
			h.deps.recordings = func() (recording.Storage, error) { return nil, nil }
			return nil
		},
		"filesystem": func(h *harness) *fakeRecordings {
			f := &fakeRecordings{backend: "FS"}
			h.withRecordings("/spool", f)
			return f
		},
		"S3 without a spool": func(h *harness) *fakeRecordings {
			f := &fakeRecordings{backend: "S3"}
			h.withRecordings("", f)
			return f
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			f := setup(h)
			got := h.run()
			wantState(t, got, DoctorCheckRecordingStorage, DoctorStateSkip)
			if f != nil && f.selfChecks != 0 {
				t.Errorf("the self-check ran %d times, want none", f.selfChecks)
			}
		})
	}
}

func TestDoctorRecordingStoragePassesWhenTheUploadWorks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	f := &fakeRecordings{backend: "S3"}
	h.withRecordings("/spool", f)
	wantState(t, h.run(), DoctorCheckRecordingStorage, DoctorStatePass)
	if f.selfChecks != 1 {
		t.Errorf("self-checks = %d, want 1", f.selfChecks)
	}
}

func TestDoctorRecordingStorageFailsWithItsCode(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]*fakeRecordings{
		"upload refused": {backend: "S3", selfCheckEr: errors.New("Aws MultiChunkedEncoding is not supported.")},
		"no bucket":      {backend: "S3", bucketErr: errors.New("bucket aicc-recordings does not exist")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.withRecordings("/spool", f)
			got := h.run()
			wantState(t, got, DoctorCheckRecordingStorage, DoctorStateFail)
			r := got[DoctorCheckRecordingStorage]
			if r.Code != DoctorCodeRecordingUploadFailed || r.Fix == "" {
				t.Errorf("code = %q fix = %q", r.Code, r.Fix)
			}
			if f.bucketErr != nil && f.selfChecks != 0 {
				t.Error("the self-check ran against a bucket that is not there")
			}
		})
	}
}
