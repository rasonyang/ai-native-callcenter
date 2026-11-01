// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
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
// status is 1 if and only if a check failed; a WARN is a deployment that works
// but will not do what its operator expects, and never changes the exit
// status; SKIP means the check could not be
// asked honestly here, never that it passed.

// DoctorState is the outcome of one check.
type DoctorState string

const (
	DoctorStatePass DoctorState = "PASS"
	DoctorStateFail DoctorState = "FAIL"
	// DoctorStateWarn is a finding that does not stop the deployment from
	// working. It carries a code and a fix like a failure, but the exit
	// status ignores it.
	DoctorStateWarn DoctorState = "WARN"
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
	DoctorCheckOutboundEndpoint     DoctorCheck = "outbound_endpoint"
	DoctorCheckOutboundGateway      DoctorCheck = "outbound_gateway"
	DoctorCheckDNCCountryCode       DoctorCheck = "dnc_country_code"
	DoctorCheckSwitchRTPRange       DoctorCheck = "switch_rtp_range"
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
	// DoctorCodeOutboundEndpointUnset and DoctorCodeOutboundGatewayMissing are
	// WARNs: a deployment nobody runs campaigns on works without them.
	DoctorCodeOutboundEndpointUnset  DoctorCode = "OUTBOUND_ENDPOINT_UNSET"
	DoctorCodeOutboundGatewayMissing DoctorCode = "OUTBOUND_GATEWAY_MISSING"
	// DoctorCodeDNCCountryCodeUnset is a WARN too: the upgrade hazard of the
	// built-in country code, never a failure.
	DoctorCodeDNCCountryCodeUnset DoctorCode = "DNC_COUNTRY_CODE_UNSET"
	// DoctorCodeSwitchRTPRangeInvalid is a FAIL: a range no call can use or one
	// the switch accepts and a firewall will not pass. DoctorCodeSwitchRTPRangeNarrow
	// is a WARN: a range too small for the bot's own calls. One check, two codes;
	// the table below lists the failing one.
	DoctorCodeSwitchRTPRangeInvalid DoctorCode = "SWITCH_RTP_RANGE_INVALID"
	DoctorCodeSwitchRTPRangeNarrow  DoctorCode = "SWITCH_RTP_RANGE_NARROW"
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
	{DoctorCheckOutboundEndpoint, DoctorCodeOutboundEndpointUnset},
	{DoctorCheckOutboundGateway, DoctorCodeOutboundGatewayMissing},
	{DoctorCheckExternalIP, DoctorCodeExternalIPNotOnHost},
	{DoctorCheckExternalIPAdvertised, DoctorCodeExternalIPStale},
	{DoctorCheckSwitchRTPRange, DoctorCodeSwitchRTPRangeInvalid},
	{DoctorCheckProviderKey, DoctorCodeProviderKeyMissing},
	{DoctorCheckProviderSession, DoctorCodeProviderSessionFailed},
	{DoctorCheckDNCCountryCode, DoctorCodeDNCCountryCodeUnset},
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
	// State is FAIL or WARN.
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
	// DNCCountOutsideCountryCode is how many numbers on the do-not-call list
	// are not in the given country calling code (digits, no "+").
	DNCCountOutsideCountryCode(ctx context.Context, countryCode string) (int64, error)
}

// switchReader is what doctor asks the switch.
type switchReader interface {
	Profiles() ([]telephony.SwitchProfile, error)
	GatewayUp(name string) (bool, error)
	// Trunks is every gateway the switch holds, with the state it is in: an
	// aborted registration (FAIL_WAIT) is a trunk that cannot place a call,
	// which a ping status alone does not say.
	Trunks() ([]telephony.Trunk, error)
	// SwitchConf is the switch's core configuration as XML.
	SwitchConf() (string, error)
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
	hostAddrs := fs.String("host-addrs", "", "comma-separated addresses of the host; an entry may be an IPv4 CIDR range such as 127.0.0.0/8, which Linux treats as all local (default: this namespace's interfaces)")
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
	out = append(out, checkOutbound(d, sw.reader)...)
	out = append(out, checkExternalIP(d, opts, sw)...)
	// The RTP range is read and judged (checkSwitchRTPRange); that the range is
	// reachable on the LAN is still verified by the release checklist, not by doctor.
	out = append(out, checkSwitchRTPRange(d, sw.reader)...)
	out = append(out, checkProvider(ctx, d, opts)...)
	return out
}

func pass(check DoctorCheck, msg string) doctorResult {
	return doctorResult{Check: check, State: DoctorStatePass, Message: msg}
}

func fail(check DoctorCheck, msg, fix string) doctorResult {
	return doctorResult{Check: check, State: DoctorStateFail, Message: msg, Code: codeFor(check), Fix: fix}
}

func warn(check DoctorCheck, msg, fix string) doctorResult {
	return doctorResult{Check: check, State: DoctorStateWarn, Message: msg, Code: codeFor(check), Fix: fix}
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
			skip(DoctorCheckDNCCountryCode, "the database did not answer"),
		}
	}
	defer closeStore()
	out := []doctorResult{pass(DoctorCheckDatabase, "the database answers")}

	m, err := st.MigrationStatus(ctx)
	if err != nil {
		return append(out,
			fail(DoctorCheckMigrations, "cannot read the schema version: "+oneLine(err.Error()),
				"check that AICC_DATABASE_URL's role can read goose_db_version"),
			skip(DoctorCheckSchema, "the schema version is unknown"),
			skip(DoctorCheckDNCCountryCode, "the database did not answer"))
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
	return append(out, checkDNCCountryCode(ctx, d, st))
}

// checkDNCCountryCode warns about the upgrade hazard of the built-in country
// code. AICC_DEFAULT_COUNTRY_CODE defaults to 1; a deployment that never set it
// and holds do-not-call numbers outside +1 (an older one, which then refused
// national numbers, or a stack whose .env blanks the code) now completes a
// national number to +1 for the do-not-call check, contact import and the bot's
// take_message callback numbers, where its list is in another country. A national
// 13800138000 no longer matches +8613800138000 and would be dialled. A WARN, never
// a failure: the deployment may genuinely be North American with a few foreign
// entries.
func checkDNCCountryCode(ctx context.Context, d doctorDeps, st migrationReader) doctorResult {
	if d.cfg.IsDefaultCountryCodeSet {
		return pass(DoctorCheckDNCCountryCode, "AICC_DEFAULT_COUNTRY_CODE is "+d.cfg.DefaultCountryCode)
	}
	n, err := st.DNCCountOutsideCountryCode(ctx, "1")
	if err != nil {
		return skip(DoctorCheckDNCCountryCode, "cannot read the do-not-call list: "+oneLine(err.Error()))
	}
	if n == 0 {
		return pass(DoctorCheckDNCCountryCode,
			"AICC_DEFAULT_COUNTRY_CODE is unset (+1 applies) and the do-not-call list holds no number outside +1")
	}
	return warn(DoctorCheckDNCCountryCode,
		fmt.Sprintf("AICC_DEFAULT_COUNTRY_CODE is unset, so national numbers are completed to +1 for the do-not-call check, "+
			"contact import and the bot's take_message callback numbers, but the do-not-call list holds %d number(s) outside +1", n),
		"set AICC_DEFAULT_COUNTRY_CODE (digits only, e.g. 86) in .env and restart the app; a stack's .env that blanks it "+
			"(AICC_DEFAULT_COUNTRY_CODE=) leaves it unset")
}

// outboundGatewayOf reads the gateway out of an outbound endpoint:
// "sofia/gateway/<gw>/%s" dials through <gw>, and <gw> may be qualified with the
// profile that holds it, "sofia/gateway/external::<gw>/%s", the form the switch
// itself prints. It returns the bare name and the profile ("" when the endpoint
// names none). A dial string that is not a gateway (a direct sip: endpoint, say)
// has none, and reports false.
func outboundGatewayOf(endpoint string) (name, profile string, ok bool) {
	rest, ok := strings.CutPrefix(endpoint, "sofia/gateway/")
	if !ok {
		return "", "", false
	}
	token, _, ok := strings.Cut(rest, "/")
	if !ok || token == "" {
		return "", "", false
	}
	if p, n, qualified := strings.Cut(token, "::"); qualified {
		if p == "" || n == "" {
			return "", "", false
		}
		return n, p, true
	}
	return token, "", true
}

// checkOutbound reports whether this deployment can place an AI outbound call
// at all, and whether the gateway it would dial through is one the switch holds
// and would dial through now.
//
// Both are WARNs. A deployment that only takes calls in works without either;
// one that runs campaigns does not, and the operator learns it from here
// instead of from a campaign that rings nothing.
func checkOutbound(d doctorDeps, sw switchReader) []doctorResult {
	endpoint := d.cfg.OutboundEndpoint
	if endpoint == "" {
		return []doctorResult{
			warn(DoctorCheckOutboundEndpoint,
				"AICC_OUTBOUND_ENDPOINT is empty: a campaign cannot place a call, and every attempt is refused inside the application",
				"set AICC_OUTBOUND_ENDPOINT to the dial string your trunk takes, such as "+
					"sofia/gateway/<gateway>/%s, in .env and recreate the app (deploy/carrier.md)"),
			skip(DoctorCheckOutboundGateway, "there is no outbound endpoint to read a gateway from"),
		}
	}
	out := []doctorResult{pass(DoctorCheckOutboundEndpoint, "AICC_OUTBOUND_ENDPOINT is "+endpoint)}

	name, profile, isGateway := outboundGatewayOf(endpoint)
	switch {
	case !isGateway:
		return append(out, skip(DoctorCheckOutboundGateway,
			"the endpoint does not dial through a gateway (sofia/gateway/<name>/…)"))
	case sw == nil:
		return append(out, skip(DoctorCheckOutboundGateway,
			"doctor could not connect to the switch"))
	}
	// Two readings of the switch, and a pass needs both. The trunk table says
	// whether the gateway is registered (a carrier gateway stuck in FAIL_WAIT
	// or UNREGED goes nowhere, whatever its ping says); IsUp is REGED or NOREG
	// and reads nothing else. The gateway's own status says whether it answers
	// OPTIONS: a NOREG gateway whose carrier stopped answering is still NOREG in
	// the table, but sofia refuses every originate through it (GATEWAY_DOWN).
	trunks, err := sw.Trunks()
	if err != nil {
		return append(out, warn(DoctorCheckOutboundGateway,
			"cannot ask the switch about "+name+": "+oneLine(err.Error()),
			"check `docker compose logs freeswitch`"))
	}
	names := make([]string, 0, len(trunks))
	var trunk *telephony.Trunk
	for i := range trunks {
		names = append(names, trunks[i].Name)
		// The bare name, and the profile too when the endpoint names one.
		if trunks[i].Name == name && (profile == "" || trunks[i].Profile == profile) && trunk == nil {
			trunk = &trunks[i]
		}
	}
	switch {
	case trunk == nil:
		return append(out, warn(DoctorCheckOutboundGateway,
			"the switch has no gateway named "+name+knownGateways(names),
			"check the name in AICC_OUTBOUND_ENDPOINT against the gateways in "+
				"freeswitch/conf/sip_profiles/external/ (deploy/carrier.md), then recreate the freeswitch container"))
	case !trunk.IsUp:
		return append(out, warn(DoctorCheckOutboundGateway,
			"the switch's gateway "+name+" is "+trunk.State+", so a call through it would not be placed",
			"check the carrier's address and credentials in the gateway file, that its registration "+
				"succeeds if it registers, and `docker compose logs freeswitch`"))
	}
	// Asked the way the endpoint spells it: the originate resolves the gateway
	// by the same lookup, so a qualified endpoint is checked qualified and a
	// bare one bare (as the bot gateway is).
	query := name
	if profile != "" {
		query = profile + "::" + name
	}
	isPingUp, err := sw.GatewayUp(query)
	if err != nil {
		return append(out, warn(DoctorCheckOutboundGateway,
			"cannot read the ping status of the switch's gateway "+name+": "+oneLine(err.Error()),
			"check `docker compose logs freeswitch`"))
	}
	if !isPingUp {
		return append(out, warn(DoctorCheckOutboundGateway,
			"the switch's gateway "+name+" is "+trunk.State+" but its ping status is DOWN, so the switch would refuse a call through it",
			"check that the carrier answers the switch's OPTIONS pings at its address, "+
				"and `sofia status gateway "+name+"` on the switch (deploy/carrier.md)"))
	}
	return append(out, pass(DoctorCheckOutboundGateway, "the switch reaches "+name+" ("+trunk.State+", ping UP)"))
}

// knownGateways finishes the sentence "the switch has no gateway named X": it
// lists the names it does have, which is what makes a typo obvious.
func knownGateways(names []string) string {
	switch len(names) {
	case 0:
		return "; it has none — add one (deploy/carrier.md)"
	case 1:
		return "; it has " + names[0]
	}
	return "; it has " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
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
		var covering hostAddr
		var found bool
		if err == nil {
			covering, found = hostCovers(addrs, want)
		}
		switch {
		case err != nil:
			out = append(out, skip(DoctorCheckExternalIP, "cannot list this host's addresses: "+oneLine(err.Error())))
		case found:
			out = append(out, pass(DoctorCheckExternalIP, hostCoverMessage(opts.externalIP, covering, source)))
		default:
			out = append(out, fail(DoctorCheckExternalIP,
				opts.externalIP+" is not one of "+source+": "+joinHostAddrs(addrs),
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

// hostAddr is one thing the host answers to: a single address, or (net set) a
// whole range. Linux makes the entire prefix of an address on a loopback
// device local, so 127.0.0.2 is on the host though only 127.0.0.1/8 is listed.
type hostAddr struct {
	ip  net.IP
	net *net.IPNet
}

func (h hostAddr) String() string {
	if h.net != nil {
		return h.net.String()
	}
	return h.ip.String()
}

// hostCovers returns the entry that covers want.
func hostCovers(addrs []hostAddr, want net.IP) (hostAddr, bool) {
	for _, a := range addrs {
		if a.net != nil && a.net.Contains(want) {
			return a, true
		}
		if a.net == nil && a.ip.Equal(want) {
			return a, true
		}
	}
	return hostAddr{}, false
}

func hostCoverMessage(ip string, entry hostAddr, source string) string {
	if entry.net != nil {
		return ip + " is within " + entry.String() + " of " + source
	}
	return ip + " is one of " + source
}

func joinHostAddrs(addrs []hostAddr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// hostAddresses is --host-addrs when given, and this namespace's interfaces
// otherwise. Inside a container on a bridge network those are the container's
// own, which is why the installer passes the host's. A loopback network on an
// interface covers its whole range; any other network counts only its own
// address (a /24 on eth0 does not make the subnet local).
func hostAddresses(d doctorDeps, opts doctorOptions) ([]hostAddr, string, error) {
	if len(opts.hostAddrs) > 0 {
		var out []hostAddr
		for _, a := range opts.hostAddrs {
			if strings.Contains(a, "/") {
				if _, n, err := net.ParseCIDR(a); err == nil {
					out = append(out, hostAddr{net: n})
				}
				continue
			}
			if ip := net.ParseIP(a); ip != nil {
				out = append(out, hostAddr{ip: ip})
			}
		}
		return out, "--host-addrs", nil
	}
	addrs, err := d.interfaceAddrs()
	if err != nil {
		return nil, "", err
	}
	var out []hostAddr
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			if v.IP.IsLoopback() {
				out = append(out, hostAddr{ip: v.IP, net: &net.IPNet{IP: v.IP.Mask(v.Mask), Mask: v.Mask}})
			} else {
				out = append(out, hostAddr{ip: v.IP})
			}
		case *net.IPAddr:
			out = append(out, hostAddr{ip: v.IP})
		}
	}
	return out, "this namespace's addresses", nil
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
		// The app refuses to start on this: a configuration error, reported as
		// one rather than skipped (a .env left over from a removed provider
		// lands here).
		msg := "AICC_PROVIDER: " + oneLine(err.Error())
		return []doctorResult{
			fail(DoctorCheckProviderKey, msg,
				"set AICC_PROVIDER to one of the values listed above in .env and recreate the app container"),
			skip(DoctorCheckProviderSession, "no valid provider to open a session with"),
		}
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
		if r.State == DoctorStateFail || r.State == DoctorStateWarn {
			line = fmt.Sprintf("%-4s  %-*s  %s  %s", r.State, width, r.Check, r.Code, r.Message)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if (r.State == DoctorStateFail || r.State == DoctorStateWarn) && r.Fix != "" {
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

// FreeSWITCH's built-in RTP port range, used when switch.conf sets none (or 0).
const (
	defaultRTPStart = 16384
	defaultRTPEnd   = 32768
)

// rtpRange is the switch's RTP range as it will be allocated.
type rtpRange struct {
	start, end int
	// startSet and endSet say whether switch.conf names the value; a missing
	// one is the switch's built-in default.
	startSet, endSet bool
}

// evenStart and evenEnd are the ports the allocator really uses: only even
// ports carry RTP, an odd start is rounded up and an odd end down.
func (r rtpRange) evenStart() int { return r.start + r.start%2 }
func (r rtpRange) evenEnd() int   { return r.end - r.end%2 }

// legs is how many media legs the range holds, one even port each.
func (r rtpRange) legs() int {
	if r.evenEnd() < r.evenStart() {
		return 0
	}
	return (r.evenEnd()-r.evenStart())/2 + 1
}

// describe states the range as configured, the even ports it really uses where
// rounding applies, and how many legs that is.
func (r rtpRange) describe() string {
	var b strings.Builder
	switch {
	case !r.startSet && !r.endSet:
		fmt.Fprintf(&b, "the switch sets no RTP range, so it uses FreeSWITCH's built-in %d–%d", r.start, r.end)
	case !r.startSet:
		fmt.Fprintf(&b, "the switch's RTP range is %d–%d (rtp-start-port is unset: FreeSWITCH's built-in %d)", r.start, r.end, r.start)
	case !r.endSet:
		fmt.Fprintf(&b, "the switch's RTP range is %d–%d (rtp-end-port is unset: FreeSWITCH's built-in %d)", r.start, r.end, r.end)
	default:
		fmt.Fprintf(&b, "the switch's RTP range is %d–%d", r.start, r.end)
	}
	if r.legs() > 0 && (r.start%2 != 0 || r.end%2 != 0) {
		fmt.Fprintf(&b, ", RTP on the even ports %d–%d", r.evenStart(), r.evenEnd())
	}
	return b.String()
}

// withThousands writes 6808 as 6,808.
func withThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// legsText is what a leg count means: a call holds two legs.
func legsText(n int) string {
	return withThousands(n) + " media legs; a call holds two (the caller and the agent or the bot), a supervisor's eavesdrop adds one"
}

// parseSwitchRTPRange reads the range out of the XML `xml_locate` returns for
// switch.conf. A param that is missing, empty or 0 is the built-in default, as
// the switch itself reads it.
func parseSwitchRTPRange(conf string) (rtpRange, error) {
	var doc struct {
		XMLName xml.Name `xml:"configuration"`
		Name    string   `xml:"name,attr"`
		Params  []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"settings>param"`
	}
	if err := xml.Unmarshal([]byte(strings.TrimSpace(conf)), &doc); err != nil {
		return rtpRange{}, fmt.Errorf("the reply is not switch.conf XML: %w", err)
	}
	if doc.Name != "switch.conf" {
		return rtpRange{}, fmt.Errorf("the reply is configuration %q, not switch.conf", doc.Name)
	}
	r := rtpRange{start: defaultRTPStart, end: defaultRTPEnd}
	for _, p := range doc.Params {
		var target *int
		var set *bool
		switch p.Name {
		case "rtp-start-port":
			target, set = &r.start, &r.startSet
		case "rtp-end-port":
			target, set = &r.end, &r.endSet
		default:
			continue
		}
		v := strings.TrimSpace(p.Value)
		if v == "" || v == "0" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return rtpRange{}, rtpValueError{fmt.Sprintf("%s is %q, not a port between 1 and 65535", p.Name, v)}
		}
		*target, *set = n, true
	}
	return r, nil
}

// rtpValueError is a configured value that is not a usable port, as opposed to
// a reply doctor could not read at all.
type rtpValueError struct{ msg string }

func (e rtpValueError) Error() string { return e.msg }

// checkSwitchRTPRange prints the switch's RTP range and how many legs it
// carries, and judges it.
//
// xml_locate shows the configured text. The core reads the range once at
// startup, so a reloadxml can move what this says away from what the running
// allocator uses; a restart of the switch brings them together. Nothing over
// the event socket reports the allocator's own state.
func checkSwitchRTPRange(d doctorDeps, sw switchReader) []doctorResult {
	const check = DoctorCheckSwitchRTPRange
	if sw == nil {
		return []doctorResult{skip(check, "doctor could not connect to the switch")}
	}
	conf, err := sw.SwitchConf()
	if err != nil {
		return []doctorResult{warn(check, "cannot read the switch's RTP range: "+oneLine(err.Error()),
			"check `docker compose logs freeswitch`")}
	}
	r, err := parseSwitchRTPRange(conf)
	if err != nil {
		if errors.As(err, new(rtpValueError)) {
			return []doctorResult{fail(check, "the switch's RTP range is invalid: "+err.Error(),
				"set FS_RTP_START_PORT and FS_RTP_END_PORT on the switch image (RTP_START and RTP_END in deploy/.env) to ports between 1 and 65535, start below end, and publish the same range")}
		}
		return []doctorResult{warn(check, "cannot read the switch's RTP range: "+oneLine(err.Error()),
			"check that the switch has a switch.conf and `docker compose logs freeswitch`")}
	}

	switch {
	case r.end < r.start:
		return []doctorResult{fail(check,
			fmt.Sprintf("%s: the end is below the start. The switch does not refuse it but allocates ports anywhere in 1–65535, "+
				"which a firewall or a published range will not pass, and its memory grows", r.describe()),
			"set FS_RTP_END_PORT above FS_RTP_START_PORT on the switch image (RTP_START and RTP_END in deploy/.env) and publish the same range")}
	case r.legs() == 0:
		return []doctorResult{fail(check,
			fmt.Sprintf("%s: it holds no even port pair, so 0 media legs and every call fails", r.describe()),
			"widen the range: FS_RTP_START_PORT/FS_RTP_END_PORT on the switch image (RTP_START/RTP_END in deploy/.env), and publish the same range")}
	}

	msg := r.describe() + ", " + legsText(r.legs())
	if need := 2 * d.cfg.BotMaxCalls; d.cfg.IsBotEnabled && r.legs() < need {
		return []doctorResult{{Check: check, State: DoctorStateWarn, Code: DoctorCodeSwitchRTPRangeNarrow,
			Message: fmt.Sprintf("%s. The AI leg alone can hold %d calls (AICC_BOT_MAX_CALLS) at two legs each, %s legs, more than the range has",
				msg, d.cfg.BotMaxCalls, withThousands(need)),
			Fix: "widen RTP_START/RTP_END in deploy/.env (on macOS each published port costs a forwarder in the VM) " +
				"and the switch's FS_RTP_START_PORT/FS_RTP_END_PORT, or lower AICC_BOT_MAX_CALLS"}}
	}
	return []doctorResult{pass(check, msg)}
}
