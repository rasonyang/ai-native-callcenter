// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// `aicc doctor` checks a running deployment from the inside: it runs in the
// application's own network namespace (`docker compose exec aicc aicc doctor`)
// and asks each dependency the question an installer would otherwise have to
// answer by hand. It changes nothing: no migration, no instance lock, nothing
// written to the switch.
//
// Each check prints one line: its state, its name and a message, and a failed
// check also its code and a second line with what to do about it. The exit
// status is 1 if and only if a check failed; SKIP means the check could not be
// asked honestly here, never that it passed.

// DoctorState is the outcome of one check.
type DoctorState string

const (
	DoctorStatePass DoctorState = "PASS"
	DoctorStateFail DoctorState = "FAIL"
	// DoctorStateSkip is a check that does not apply to this deployment or
	// cannot be answered from where doctor runs.
	DoctorStateSkip DoctorState = "SKIP"
)

// DoctorCheck names a check. The values are stable lower_snake names; each
// result carries one, whatever its state.
type DoctorCheck string

const (
	DoctorCheckApp                  DoctorCheck = "app"
	DoctorCheckSwitchLink           DoctorCheck = "switch_link"
	DoctorCheckDatabase             DoctorCheck = "database"
	DoctorCheckMigrations           DoctorCheck = "migrations"
	DoctorCheckSchema               DoctorCheck = "schema"
	DoctorCheckSwitchReachable      DoctorCheck = "switch_reachable"
	DoctorCheckSwitchAuth           DoctorCheck = "switch_auth"
	DoctorCheckSIPProfiles          DoctorCheck = "sip_profiles"
	DoctorCheckBotGateway           DoctorCheck = "bot_gateway"
	DoctorCheckExternalIP           DoctorCheck = "external_ip"
	DoctorCheckExternalIPAdvertised DoctorCheck = "external_ip_advertised"
	DoctorCheckProviderKey          DoctorCheck = "provider_key"
	DoctorCheckProviderSession      DoctorCheck = "provider_session"
)

// DoctorCode names a failure. The values are stable: an installer matches on
// them. A code appears only on a failed check. SWITCH_DOWN means what it means
// in the API's error codes — the application has no working link to the
// switch.
type DoctorCode string

const (
	DoctorCodeAppNotReady           DoctorCode = "APP_NOT_READY"
	DoctorCodeDBUnreachable         DoctorCode = "DB_UNREACHABLE"
	DoctorCodeDBMigrationsPending   DoctorCode = "DB_MIGRATIONS_PENDING"
	DoctorCodeDBSchemaNewer         DoctorCode = "DB_SCHEMA_NEWER"
	DoctorCodeSwitchDown            DoctorCode = "SWITCH_DOWN"
	DoctorCodeSwitchUnreachable     DoctorCode = "SWITCH_UNREACHABLE"
	DoctorCodeSwitchAuthRejected    DoctorCode = "SWITCH_AUTH_REJECTED"
	DoctorCodeSwitchProfileDown     DoctorCode = "SWITCH_PROFILE_DOWN"
	DoctorCodeBotGatewayDown        DoctorCode = "BOT_GATEWAY_DOWN"
	DoctorCodeExternalIPNotOnHost   DoctorCode = "EXTERNAL_IP_NOT_ON_HOST"
	DoctorCodeExternalIPStale       DoctorCode = "EXTERNAL_IP_STALE"
	DoctorCodeProviderKeyMissing    DoctorCode = "PROVIDER_KEY_MISSING"
	DoctorCodeProviderSessionFailed DoctorCode = "PROVIDER_SESSION_FAILED"
)

// doctorChecks is every check, in the order they run, with the code it fails
// with.
var doctorChecks = []struct {
	check DoctorCheck
	code  DoctorCode
}{
	{DoctorCheckApp, DoctorCodeAppNotReady},
	{DoctorCheckSwitchLink, DoctorCodeSwitchDown},
	{DoctorCheckDatabase, DoctorCodeDBUnreachable},
	{DoctorCheckMigrations, DoctorCodeDBMigrationsPending},
	{DoctorCheckSchema, DoctorCodeDBSchemaNewer},
	{DoctorCheckSwitchReachable, DoctorCodeSwitchUnreachable},
	{DoctorCheckSwitchAuth, DoctorCodeSwitchAuthRejected},
	{DoctorCheckSIPProfiles, DoctorCodeSwitchProfileDown},
	{DoctorCheckBotGateway, DoctorCodeBotGatewayDown},
	{DoctorCheckExternalIP, DoctorCodeExternalIPNotOnHost},
	{DoctorCheckExternalIPAdvertised, DoctorCodeExternalIPStale},
	{DoctorCheckProviderKey, DoctorCodeProviderKeyMissing},
	{DoctorCheckProviderSession, DoctorCodeProviderSessionFailed},
}

// codeFor is the code a check fails with.
func codeFor(check DoctorCheck) DoctorCode {
	for _, c := range doctorChecks {
		if c.check == check {
			return c.code
		}
	}
	panic("doctor: unknown check " + string(check))
}

// doctorResult is one check's outcome.
type doctorResult struct {
	Check   DoctorCheck `json:"check"`
	State   DoctorState `json:"state"`
	Message string      `json:"message"`
	// Code and Fix say what failed and what to do about it; empty unless
	// State is FAIL.
	Code DoctorCode `json:"code,omitempty"`
	Fix  string     `json:"fix,omitempty"`
}

// botGatewayName is the gateway the switch reaches the AI leg through. It is
// the switch configuration's name and outbound's default (C47).
const botGatewayName = "aicc_bot"

// providerProbeTimeout bounds opening a session with the speech provider.
const providerProbeTimeout = 15 * time.Second

// migrationReader is what doctor asks the database.
type migrationReader interface {
	MigrationStatus(ctx context.Context) (store.MigrationState, error)
}

// switchReader is what doctor asks the switch.
type switchReader interface {
	Profiles() ([]telephony.SwitchProfile, error)
	GatewayUp(name string) (bool, error)
}

// doctorOptions are the command's flags.
type doctorOptions struct {
	wait         time.Duration
	readyzURL    string
	externalIP   string
	hostAddrs    []string
	skipProvider bool
	asJSON       bool
}

// doctorDeps is everything doctor reaches outside itself, so a test can hand
// it fakes.
type doctorDeps struct {
	cfg    config.Config
	getenv func(string) string
	http   *http.Client
	// openStore connects to the database without migrating it or taking the
	// instance lock the running server holds.
	openStore func(ctx context.Context) (migrationReader, func(), error)
	// dialSwitch opens doctor's own connection to the switch, with the
	// application's credentials.
	dialSwitch func(ctx context.Context) (switchReader, func(), error)
	// sessions is the application's own provider factory.
	sessions       func(provider.Profile, *slog.Logger) (provider.VoiceSession, error)
	interfaceAddrs func() ([]net.Addr, error)
	sleep          func(context.Context, time.Duration) bool
	now            func() time.Time
}

// runDoctor implements `aicc doctor`.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	wait := fs.Duration("wait", 0, "keep polling /readyz up to this long until the app is ready and its switch link is up (0: ask once)")
	readyzURL := fs.String("readyz-url", "", "the app's readiness URL (default: derived from AICC_METRICS_ADDR)")
	externalIP := fs.String("external-ip", "", "the address phones reach the switch at (default: $FS_EXTERNAL_IP)")
	hostAddrs := fs.String("host-addrs", "", "comma-separated addresses of the host (default: this namespace's interfaces)")
	skipProvider := fs.Bool("skip-provider", false, "do not open a session with the speech provider")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	opts := doctorOptions{
		wait:         *wait,
		readyzURL:    *readyzURL,
		externalIP:   *externalIP,
		skipProvider: *skipProvider,
		asJSON:       *asJSON,
	}
	if opts.readyzURL == "" {
		opts.readyzURL = readyzURLFor(cfg.MetricsAddr)
	}
	if opts.externalIP == "" {
		opts.externalIP = os.Getenv("FS_EXTERNAL_IP")
	}
	for _, a := range strings.Split(*hostAddrs, ",") {
		if a = strings.TrimSpace(a); a != "" {
			opts.hostAddrs = append(opts.hostAddrs, a)
		}
	}

	results := doctor(context.Background(), doctorDeps{
		cfg:    cfg,
		getenv: os.Getenv,
		http:   &http.Client{Timeout: 5 * time.Second},
		openStore: func(ctx context.Context) (migrationReader, func(), error) {
			st, err := store.Open(ctx, cfg.DatabaseURL, 2)
			if err != nil {
				return nil, nil, err
			}
			return st, st.Close, nil
		},
		dialSwitch: func(ctx context.Context) (switchReader, func(), error) {
			client, err := esl.Dial(ctx, cfg.ESLAddr, cfg.ESLPassword)
			if err != nil {
				return nil, nil, err
			}
			return telephony.NewAdapter(oneShotSwitch{client}, cfg.SwitchDomain),
				func() { _ = client.Close() }, nil
		},
		sessions:       voiceSession,
		interfaceAddrs: net.InterfaceAddrs,
		sleep:          sleepCtx,
		now:            time.Now,
	}, opts)

	if err := printDoctor(os.Stdout, results, opts.asJSON); err != nil {
		return err
	}
	if n := countFailed(results); n > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", n)
	}
	return nil
}

// oneShotSwitch lets doctor's own connection stand where the Adapter expects
// the application's reconnecting link.
type oneShotSwitch struct{ *esl.Client }

func (oneShotSwitch) IsUp() bool { return true }

// readyzURLFor derives the readiness URL from the ops listener's address. A
// wildcard or empty host is reached on loopback, which is where doctor finds
// it when it shares the application's network namespace.
func readyzURLFor(metricsAddr string) string {
	host, port, err := net.SplitHostPort(metricsAddr)
	if err != nil {
		host, port = "", "9090"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz"
}

// doctor runs every check, in order, and returns every result.
func doctor(ctx context.Context, d doctorDeps, opts doctorOptions) []doctorResult {
	var out []doctorResult
	// The application's own view first: it is the thing being installed.
	out = append(out, checkApp(ctx, d, opts)...)
	// Then each dependency, asked directly.
	out = append(out, checkDatabase(ctx, d)...)
	sw, closeSwitch, results := checkSwitch(ctx, d)
	defer closeSwitch()
	out = append(out, results...)
	out = append(out, checkBotGateway(ctx, d, opts, sw.reader)...)
	out = append(out, checkExternalIP(d, opts, sw)...)
	// LAN media (RTP) reachability is verified by the release checklist, not by doctor.
	out = append(out, checkProvider(ctx, d, opts)...)
	return out
}

func pass(check DoctorCheck, msg string) doctorResult {
	return doctorResult{Check: check, State: DoctorStatePass, Message: msg}
}

func fail(check DoctorCheck, msg, fix string) doctorResult {
	return doctorResult{Check: check, State: DoctorStateFail, Message: msg, Code: codeFor(check), Fix: fix}
}

func skip(check DoctorCheck, msg string) doctorResult {
	return doctorResult{Check: check, State: DoctorStateSkip, Message: msg}
}

// readyzReply is one answer from /readyz.
type readyzReply struct {
	status int
	head   string
	lines  map[string]string
}

func fetchReadyz(ctx context.Context, client *http.Client, url string) (readyzReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return readyzReply{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return readyzReply{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return readyzReply{}, err
	}
	reply := readyzReply{status: resp.StatusCode, lines: map[string]string{}}
	for i, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if i == 0 {
			reply.head = strings.TrimSpace(line)
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			reply.lines[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return reply, nil
}

// checkApp asks the running application whether it is ready, and what it
// says of its switch link. With --wait it keeps asking until both are good or
// the time is up, because an installer runs this seconds after `up`.
func checkApp(ctx context.Context, d doctorDeps, opts doctorOptions) []doctorResult {
	deadline := d.now().Add(opts.wait)
	var reply readyzReply
	var err error
	for {
		reply, err = fetchReadyz(ctx, d.http, opts.readyzURL)
		switchLine, reported := reply.lines["switch"]
		if err == nil && reply.status == http.StatusOK && (!reported || switchLine == "up") {
			break
		}
		if !d.now().Before(deadline) || !d.sleep(ctx, time.Second) {
			break
		}
	}

	if err != nil {
		return []doctorResult{
			fail(DoctorCheckApp, "no answer from "+opts.readyzURL+": "+err.Error(),
				"check `docker compose logs aicc`; the app exits on a configuration error, and --readyz-url must match AICC_METRICS_ADDR"),
			skip(DoctorCheckSwitchLink, "the app did not answer, so its switch link is unknown"),
		}
	}
	var out []doctorResult
	if reply.status != http.StatusOK {
		out = append(out, fail(DoctorCheckApp,
			fmt.Sprintf("%s answered %d: %s", opts.readyzURL, reply.status, reply.head),
			"the app cannot reach its database; see the database check below and `docker compose logs aicc`"))
	} else {
		msg := "the app is ready"
		if m, ok := reply.lines["migrations"]; ok {
			msg += ", schema " + m
		}
		out = append(out, pass(DoctorCheckApp, msg))
	}

	switch reply.lines["switch"] {
	case "up":
		out = append(out, pass(DoctorCheckSwitchLink, "the app's link to the switch is up"))
	case "down":
		out = append(out, fail(DoctorCheckSwitchLink, "the app has no link to the switch",
			"check that the freeswitch container is running and that AICC_ESL_ADDR and AICC_ESL_PASSWORD match it (see the switch_* checks below)"))
	default:
		// An application older than this doctor does not report its link.
		out = append(out, skip(DoctorCheckSwitchLink, "the app does not report its switch link"))
	}
	return out
}

func checkDatabase(ctx context.Context, d doctorDeps) []doctorResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	st, closeStore, err := d.openStore(ctx)
	if err != nil {
		return []doctorResult{
			fail(DoctorCheckDatabase, oneLine(err.Error()),
				"check that the postgres container is healthy and that AICC_DATABASE_URL points at it"),
			skip(DoctorCheckMigrations, "the database did not answer"),
			skip(DoctorCheckSchema, "the database did not answer"),
		}
	}
	defer closeStore()
	out := []doctorResult{pass(DoctorCheckDatabase, "the database answers")}

	m, err := st.MigrationStatus(ctx)
	if err != nil {
		return append(out,
			fail(DoctorCheckMigrations, "cannot read the schema version: "+oneLine(err.Error()),
				"check that AICC_DATABASE_URL's role can read goose_db_version"),
			skip(DoctorCheckSchema, "the schema version is unknown"))
	}
	version := fmt.Sprintf("schema %d, this binary's latest %d", m.DBVersion, m.LatestVersion)
	if m.HasPending {
		out = append(out, fail(DoctorCheckMigrations, version+", migrations pending",
			"start the app (it migrates at startup) and read `docker compose logs aicc` if it does not"))
	} else {
		out = append(out, pass(DoctorCheckMigrations, version))
	}
	if m.DBVersion > m.LatestVersion {
		out = append(out, fail(DoctorCheckSchema, version+": a newer release migrated this database",
			"run the release that migrated it (or newer); downgrading over a newer schema is not supported"))
	} else {
		out = append(out, pass(DoctorCheckSchema, "the schema is not ahead of this binary"))
	}
	return out
}

// switchProfiles is what the switch said about its SIP listeners, for the
// checks after checkSwitch. reader is nil when doctor could not connect.
type switchProfiles struct {
	reader   switchReader
	profiles []telephony.SwitchProfile
	err      error
}

// requiredProfiles are the listeners a deployment cannot work without: agents'
// phones on internal, carriers and the bot's gateway on external.
var requiredProfiles = []string{"internal", "external"}

// checkSwitch connects to the switch and reads its SIP profiles. The
// connection stays open for the checks after it; the caller closes it.
func checkSwitch(ctx context.Context, d doctorDeps) (switchProfiles, func(), []doctorResult) {
	nothing := func() {}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	sw, closeSwitch, err := d.dialSwitch(dialCtx)
	if err != nil {
		unknown := skip(DoctorCheckSIPProfiles, "doctor could not connect to the switch")
		if errors.Is(err, esl.ErrAuthFailed) {
			return switchProfiles{err: err}, nothing, []doctorResult{
				pass(DoctorCheckSwitchReachable, "the switch answers at "+d.cfg.ESLAddr),
				fail(DoctorCheckSwitchAuth, "the switch refused AICC_ESL_PASSWORD",
					"set AICC_ESL_PASSWORD to the switch's event-socket password (the stack's ESL_PASSWORD) and recreate both containers"),
				unknown,
			}
		}
		return switchProfiles{err: err}, nothing, []doctorResult{
			fail(DoctorCheckSwitchReachable, "no answer from the switch at "+d.cfg.ESLAddr+": "+oneLine(err.Error()),
				"check that the freeswitch container is running and healthy and that AICC_ESL_ADDR points at it"),
			skip(DoctorCheckSwitchAuth, "the switch did not answer"),
			unknown,
		}
	}
	out := []doctorResult{
		pass(DoctorCheckSwitchReachable, "the switch answers at "+d.cfg.ESLAddr),
		pass(DoctorCheckSwitchAuth, "the switch accepted AICC_ESL_PASSWORD"),
	}
	profiles, err := sw.Profiles()
	if err != nil {
		return switchProfiles{reader: sw, err: err}, closeSwitch, append(out,
			fail(DoctorCheckSIPProfiles, "cannot list the switch's SIP profiles: "+oneLine(err.Error()),
				"check `docker compose logs freeswitch`"))
	}
	var down []string
	for _, name := range requiredProfiles {
		i := slices.IndexFunc(profiles, func(p telephony.SwitchProfile) bool { return p.Name == name })
		if i < 0 || !profiles[i].IsRunning {
			down = append(down, name)
		}
	}
	if len(down) > 0 {
		out = append(out, fail(DoctorCheckSIPProfiles,
			"SIP profile not running: "+strings.Join(down, ", "),
			"a profile that fails to start is usually a port already taken on the host or an address it cannot bind; check `docker compose logs freeswitch`"))
	} else {
		out = append(out, pass(DoctorCheckSIPProfiles, "SIP profiles internal and external are running"))
	}
	return switchProfiles{reader: sw, profiles: profiles}, closeSwitch, out
}

// botGatewayGrace bounds how long --wait keeps asking about the bot gateway
// after the first DOWN. The switch probes aicc_bot every 30 s
// (freeswitch/conf/sip_profiles/external/aicc_bot.xml), so an application
// recreated seconds ago can read DOWN until its next OPTIONS is answered. The
// bound keeps a real misconfiguration a fast failure.
const botGatewayGrace = 45 * time.Second

func checkBotGateway(ctx context.Context, d doctorDeps, opts doctorOptions, sw switchReader) []doctorResult {
	if !d.cfg.IsBotEnabled {
		return []doctorResult{skip(DoctorCheckBotGateway, "the AI leg is off (AICC_BOT_ENABLED)")}
	}
	if sw == nil {
		return []doctorResult{skip(DoctorCheckBotGateway, "doctor could not connect to the switch")}
	}
	up, err := waitBotGateway(ctx, d, opts, sw)
	switch {
	case errors.Is(err, telephony.ErrUnknownGateway):
		return []doctorResult{fail(DoctorCheckBotGateway,
			"the switch has no gateway named "+botGatewayName,
			"the switch image defines it from AICC_BOT_HOST; recreate the freeswitch container from this release's image")}
	case err != nil:
		return []doctorResult{fail(DoctorCheckBotGateway,
			"cannot ask the switch about "+botGatewayName+": "+oneLine(err.Error()),
			"check `docker compose logs freeswitch`")}
	case !up:
		return []doctorResult{fail(DoctorCheckBotGateway,
			"the switch cannot reach the AI leg through "+botGatewayName,
			"the gateway must name an IP address the switch can send to (AICC_APP_IP in the stack), never loopback; check that the app is listening on AICC_BOT_SIP_PORT")}
	}
	return []doctorResult{pass(DoctorCheckBotGateway, "the switch reaches the AI leg through "+botGatewayName)}
}

// waitBotGateway asks the switch once and, under --wait, until the gateway
// answers UP or the grace is over. Structural failures (no such gateway, ESL
// errors) are not retried.
func waitBotGateway(ctx context.Context, d doctorDeps, opts doctorOptions, sw switchReader) (bool, error) {
	up, err := sw.GatewayUp(botGatewayName)
	if err != nil || up || opts.wait <= 0 {
		return up, err
	}
	deadline := d.now().Add(min(opts.wait, botGatewayGrace))
	for !up && d.now().Before(deadline) && d.sleep(ctx, time.Second) {
		if up, err = sw.GatewayUp(botGatewayName); err != nil {
			return up, err
		}
	}
	return up, nil
}

func checkExternalIP(d doctorDeps, opts doctorOptions, sw switchProfiles) []doctorResult {
	if opts.externalIP == "" {
		return []doctorResult{
			skip(DoctorCheckExternalIP, "neither FS_EXTERNAL_IP nor --external-ip is set"),
			skip(DoctorCheckExternalIPAdvertised, "neither FS_EXTERNAL_IP nor --external-ip is set"),
		}
	}
	want := net.ParseIP(opts.externalIP)

	var out []doctorResult
	switch {
	case want == nil:
		out = append(out, fail(DoctorCheckExternalIP,
			"FS_EXTERNAL_IP is not an IP address: "+opts.externalIP,
			"set FS_EXTERNAL_IP in deploy/.env to the host address phones reach, then `docker compose up -d`"))
	default:
		addrs, source, err := hostAddresses(d, opts)
		switch {
		case err != nil:
			out = append(out, skip(DoctorCheckExternalIP, "cannot list this host's addresses: "+oneLine(err.Error())))
		case slices.ContainsFunc(addrs, want.Equal):
			out = append(out, pass(DoctorCheckExternalIP, opts.externalIP+" is one of "+source))
		default:
			out = append(out, fail(DoctorCheckExternalIP,
				opts.externalIP+" is not one of "+source+": "+joinIPs(addrs),
				"set FS_EXTERNAL_IP to this host's own address (or pass --host-addrs when doctor cannot see the host's interfaces), then `docker compose up -d`"))
		}
	}

	switch {
	case sw.reader == nil || sw.err != nil:
		out = append(out, skip(DoctorCheckExternalIPAdvertised, "the switch's profiles are unknown"))
	case want == nil:
		out = append(out, skip(DoctorCheckExternalIPAdvertised, "FS_EXTERNAL_IP is not an IP address"))
	default:
		var stale []string
		for _, p := range sw.profiles {
			if !slices.Contains(requiredProfiles, p.Name) {
				continue
			}
			if got := net.ParseIP(p.AdvertisedMediaIP); got == nil || !got.Equal(want) {
				stale = append(stale, fmt.Sprintf("%s advertises %q", p.Name, p.AdvertisedMediaIP))
			}
		}
		if len(stale) > 0 {
			out = append(out, fail(DoctorCheckExternalIPAdvertised,
				strings.Join(stale, ", ")+", not "+opts.externalIP,
				"the switch reads FS_EXTERNAL_IP when its container is created: `docker compose up -d --force-recreate freeswitch`"))
		} else {
			out = append(out, pass(DoctorCheckExternalIPAdvertised, "the switch advertises "+opts.externalIP+" for media"))
		}
	}
	return out
}

// hostAddresses is --host-addrs when given, and this namespace's interfaces
// otherwise. Inside a container on a bridge network those are the container's
// own, which is why the installer passes the host's.
func hostAddresses(d doctorDeps, opts doctorOptions) ([]net.IP, string, error) {
	if len(opts.hostAddrs) > 0 {
		var ips []net.IP
		for _, a := range opts.hostAddrs {
			if ip := net.ParseIP(a); ip != nil {
				ips = append(ips, ip)
			}
		}
		return ips, "--host-addrs", nil
	}
	addrs, err := d.interfaceAddrs()
	if err != nil {
		return nil, "", err
	}
	var ips []net.IP
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			ips = append(ips, v.IP)
		case *net.IPAddr:
			ips = append(ips, v.IP)
		}
	}
	return ips, "this namespace's addresses", nil
}

func joinIPs(ips []net.IP) string {
	s := make([]string, len(ips))
	for i, ip := range ips {
		s[i] = ip.String()
	}
	return strings.Join(s, ", ")
}

func checkProvider(ctx context.Context, d doctorDeps, opts doctorOptions) []doctorResult {
	if !d.cfg.IsBotEnabled {
		return []doctorResult{
			skip(DoctorCheckProviderKey, "the AI leg is off (AICC_BOT_ENABLED)"),
			skip(DoctorCheckProviderSession, "the AI leg is off (AICC_BOT_ENABLED)"),
		}
	}
	profile, err := voiceProfile(d.cfg)
	if err != nil {
		// The app refuses to start on this, so the app check has failed too.
		msg := "AICC_PROVIDER: " + oneLine(err.Error())
		return []doctorResult{skip(DoctorCheckProviderKey, msg), skip(DoctorCheckProviderSession, msg)}
	}

	discard := slog.New(slog.DiscardHandler)
	if reportMissingProviderKey(discard, profile, d.getenv) {
		return []doctorResult{
			fail(DoctorCheckProviderKey, profile.APIKeyEnv+" is not set for provider "+profile.Name,
				"set "+profile.APIKeyEnv+" in deploy/.env and `docker compose up -d` (restart does not re-read .env)"),
			skip(DoctorCheckProviderSession, "no key to open a session with"),
		}
	}
	out := []doctorResult{pass(DoctorCheckProviderKey, "provider "+profile.Name+" has a key")}
	if opts.skipProvider {
		return append(out, skip(DoctorCheckProviderSession, "--skip-provider"))
	}

	ctx, cancel := context.WithTimeout(ctx, providerProbeTimeout)
	defer cancel()
	if err := probeProvider(ctx, d, profile, discard); err != nil {
		return append(out, fail(DoctorCheckProviderSession,
			"provider "+profile.Name+" ("+profile.Model+") did not open a session: "+oneLine(err.Error()),
			"check "+profile.APIKeyEnv+" and AICC_PROVIDER_ENDPOINT/AICC_PROVIDER_MODEL, and that this host can reach "+profile.Endpoint))
	}
	return append(out, pass(DoctorCheckProviderSession,
		"provider "+profile.Name+" ("+profile.Model+") opened a session"))
}

// probeProvider opens one session the way a call would, and closes it as soon
// as the provider says it is ready. That is the one proof a key, a model name
// and an endpoint work together: a WebSocket handshake alone is not, because
// some providers accept it and reject the first frame.
func probeProvider(ctx context.Context, d doctorDeps, profile provider.Profile, log *slog.Logger) error {
	session, err := d.sessions(profile, log)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()
	// Nobody plays the greeting; draining keeps a client from blocking on it.
	go func() {
		for range session.Events() {
		}
	}()
	in, out := profile.FormatsFor(media.LawMu)
	return session.Start(ctx, provider.SessionConfig{
		Instructions: "You are a connectivity check. Say nothing.",
		Language:     "en",
		Turn:         provider.DefaultTurnDetection(),
		InputFormat:  in,
		OutputFormat: out,
	})
}

func printDoctor(w io.Writer, results []doctorResult, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Results []doctorResult `json:"results"`
		}{results})
	}
	width := 0
	for _, c := range doctorChecks {
		width = max(width, len(c.check))
	}
	for _, r := range results {
		line := fmt.Sprintf("%-4s  %-*s  %s", r.State, width, r.Check, r.Message)
		if r.State == DoctorStateFail {
			line = fmt.Sprintf("%-4s  %-*s  %s  %s", r.State, width, r.Check, r.Code, r.Message)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if r.State == DoctorStateFail && r.Fix != "" {
			if _, err := fmt.Fprintf(w, "      fix: %s\n", r.Fix); err != nil {
				return err
			}
		}
	}
	return nil
}

func countFailed(results []doctorResult) int {
	n := 0
	for _, r := range results {
		if r.State == DoctorStateFail {
			n++
		}
	}
	return n
}

// oneLine keeps an error on the line it is printed on.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
