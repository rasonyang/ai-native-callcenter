// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/aicall"
	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/doubao"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

type retirer struct {
	closed  []uuid.UUID
	started []uuid.UUID
}

func (r *retirer) Close(id uuid.UUID) { r.closed = append(r.closed, id) }

// For records that a call's actor was asked for. It returns nil because what
// the actor does once it exists is internal/transcript's to prove, not this
// file's; the composition only has to show that the registry is reachable from
// the coordinator, so that a call with a tap always has somewhere to write.
func (r *retirer) For(id uuid.UUID, _ events.CallType, _ time.Time) *transcript.Actor {
	r.started = append(r.started, id)
	return nil
}

// Nothing has to be listening first.
func TestRetiringATranscriptWorksWithNoPredecessor(t *testing.T) {
	t.Parallel()
	transcripts := &retirer{}
	callID := uuid.Must(uuid.NewV7())

	retireTranscriptWithCall(nil, transcripts)(telephony.Snapshot{CallID: callID})

	if len(transcripts.closed) != 1 || transcripts.closed[0] != callID {
		t.Errorf("retired %v, want the finished call", transcripts.closed)
	}
}

type detacher struct{ calls []uuid.UUID }

func (d *detacher) DetachCall(id uuid.UUID) { d.calls = append(d.calls, id) }

// Nothing has to be listening first — and nothing is, today.
func TestDetachingTapsWorksWithNoPredecessor(t *testing.T) {
	t.Parallel()
	taps := &detacher{}
	callID := uuid.Must(uuid.NewV7())

	detachTapsWithCall(nil, taps)(callID)

	if len(taps.calls) != 1 || taps.calls[0] != callID {
		t.Errorf("detached %v, want the retired call", taps.calls)
	}
}

type lineGiver struct{ ended []uuid.UUID }

func (l *lineGiver) CallEnded(id uuid.UUID) { l.ended = append(l.ended, id) }

// A line on a DID is given back on either ending signal, and the writer of the
// CDR before it still runs.
func TestAPlacedCallsLineIsReleasedOnFinishAndOnRetire(t *testing.T) {
	t.Parallel()
	lines := &lineGiver{}
	callID := uuid.Must(uuid.NewV7())
	var priorRan int

	releaseLinesOnFinish(func(telephony.Snapshot) { priorRan++ }, lines)(telephony.Snapshot{CallID: callID})
	releaseLinesOnRetire(func(uuid.UUID) { priorRan++ }, lines)(callID)
	releaseLinesOnFinish(nil, lines)(telephony.Snapshot{CallID: callID})

	if priorRan != 2 || len(lines.ended) != 3 || lines.ended[0] != callID {
		t.Errorf("prior ran %d times, lines released for %v", priorRan, lines.ended)
	}
}

// What a deployment demands of a flow follows from what answers its calls.
//
// Every deployment that answers calls demands that a flow's tool secrets are
// set and that its time limit lies inside AICC_BOT_MAX_CALL_DURATION (two
// rules, #637). The three engines reached over the Realtime protocol can all
// be prompted into a turn, so they demand nothing more. Doubao cannot be,
// so every ending a call can stop at has to carry its own words — and a
// deployment with the AI leg switched off demands nothing at all, whatever
// provider its configuration names: there is no session to have shortcomings.
func TestWhatAPublishMustSatisfyFollowsFromWhatAnswersTheCalls(t *testing.T) {
	t.Parallel()
	speaksOnDemand := provider.DoubaoProfile()

	for _, tc := range []struct {
		name         string
		isBotEnabled bool
		profile      provider.Profile
		wantRules    int
	}{
		{"a provider that can be cued asks only for the secrets and the time limit", true, provider.OpenAIProfile(), 2},
		{"a provider that cannot needs every ending written", true, speaksOnDemand, 3},
		{"with no AI leg there is nothing to satisfy", false, speaksOnDemand, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rules := flowPublishRules(config.Config{IsBotEnabled: tc.isBotEnabled}, tc.profile)
			if len(rules) != tc.wantRules {
				t.Errorf("%d publish rules, want %d", len(rules), tc.wantRules)
			}
		})
	}
}

// The time-limit rule a deployment publishes by is built from its own
// AICC_BOT_MAX_CALL_DURATION (#637): at the default 300 s a flow asking for
// 900 s, or for 0, is refused naming the setting; at 3600 s the 900 s flow
// publishes and 0 is still refused. A flow that leaves the field out publishes
// under either.
func TestThePublishedTimeLimitFollowsTheDeploymentsCap(t *testing.T) {
	t.Setenv("AICC_ENV", "dev")
	t.Setenv("AICC_BOT_MAX_CALL_DURATION", "")
	os.Unsetenv("AICC_BOT_MAX_CALL_DURATION")
	defaults, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	widest := defaults
	widest.BotMaxCallDuration = time.Hour

	seconds := func(n int) *int { return &n }
	for _, tc := range []struct {
		name        string
		cfg         config.Config
		maxDuration *int
		isRefused   bool
	}{
		{"900 at the default cap", defaults, seconds(900), true},
		{"0 at the default cap", defaults, seconds(0), true},
		{"300 at the default cap", defaults, seconds(300), false},
		{"left out at the default cap", defaults, nil, false},
		{"900 at the widest cap", widest, seconds(900), false},
		{"0 at the widest cap", widest, seconds(0), true},
		{"left out at the widest cap", widest, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &flow.Spec{ID: "support", Global: flow.Global{MaxDurationSec: tc.maxDuration}}
			var refusal error
			for _, rule := range flowPublishRules(tc.cfg, provider.OpenAIProfile()) {
				if err := rule(spec); err != nil {
					refusal = err
					break
				}
			}
			var invalid *flow.ValidationError
			switch {
			case !tc.isRefused && refusal != nil:
				t.Errorf("refused: %v", refusal)
			case tc.isRefused && (!errors.As(refusal, &invalid) ||
				!strings.Contains(refusal.Error(), "AICC_BOT_MAX_CALL_DURATION")):
				t.Errorf("refusal = %v, want a ValidationError naming AICC_BOT_MAX_CALL_DURATION", refusal)
			}
		})
	}
}

// A misspelled provider is a misspelling whether or not the AI leg is on. It
// surfaces at startup rather than the first time somebody turns the bot on,
// which would be during an incident.
func TestAnUnknownProviderNameIsAStartupFailure(t *testing.T) {
	t.Parallel()
	if _, err := voiceProfile(config.Config{Provider: "nonesuch"}); err == nil {
		t.Error("an unknown provider name was accepted")
	}
	// A deployment left over from the removed engine is refused with the
	// values that do run, not started broken.
	_, err := voiceProfile(config.Config{Provider: "gemini"})
	if err == nil {
		t.Fatal("the removed provider gemini was accepted")
	}
	for _, name := range []string{"openai", "qwen", "gateway", "doubao"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal for gemini does not name %q: %v", name, err)
		}
	}
	profile, err := voiceProfile(config.Config{
		Provider: "openai", ProviderEndpoint: "wss://gateway.internal/realtime",
	})
	if err != nil {
		t.Fatalf("voiceProfile: %v", err)
	}
	if profile.Endpoint != "wss://gateway.internal/realtime" {
		t.Errorf("endpoint = %q, want the deployment's own", profile.Endpoint)
	}
}

// A provider name selects a client as well as a profile, and only here.
//
// Getting it wrong is silent in every way that matters: every client satisfies
// provider.VoiceSession, all of them are built without touching the network, and
// the process starts. A doubao deployment handed the Realtime client would dial
// the right address speaking the wrong protocol, and the first real call would
// be the first thing to notice.
func TestTheProviderNameChoosesTheClient(t *testing.T) {
	for _, keyEnv := range []string{
		"OPENAI_API_KEY", "ALIYUN_API_KEY", "REALTIME_API_KEY", "DOUBAO_API_KEY", "XAI_API_KEY",
	} {
		t.Setenv(keyEnv, "not-a-real-key")
	}

	for _, tc := range []struct {
		profile provider.Profile
		want    provider.VoiceSession
	}{
		{provider.DoubaoProfile(), (*doubao.Session)(nil)},
		{provider.OpenAIProfile(), (*provider.Realtime)(nil)},
		{provider.QwenProfile(), (*provider.Realtime)(nil)},
		{provider.GrokProfile(), (*provider.Realtime)(nil)},
		{provider.GatewayProfile(), (*provider.Realtime)(nil)},
	} {
		t.Run(tc.profile.Name, func(t *testing.T) {
			session, err := voiceSessions(ratemeter.New(nil, ratemeter.Config{}))(tc.profile, nil)
			if err != nil {
				t.Fatalf("voiceSessions: %v", err)
			}
			if got, want := reflect.TypeOf(session), reflect.TypeOf(tc.want); got != want {
				t.Errorf("%s is answered by %v, want %v", tc.profile.Name, got, want)
			}
		})
	}
}

// The startup line and aicc doctor say each limit with its period and the
// setting it came from, and the per-minute gauge carries each limit scaled to
// a minute (#644).
func TestTheQuotaSummarySaysEachLimitWithItsPeriod(t *testing.T) {
	for _, k := range []string{"AICC_PROVIDER_SESSION_RPM", "AICC_PROVIDER_TOKEN_RPM",
		"AICC_PROVIDER_SESSION_LIMIT", "AICC_PROVIDER_SESSION_PERIOD", "AICC_PROVIDER_TOKEN_LIMIT", "AICC_PROVIDER_TOKEN_PERIOD"} {
		t.Setenv(k, "")
	}
	t.Setenv("AICC_PROVIDER", "qwen")
	t.Setenv("AICC_PROVIDER_SESSION_LIMIT", "3")
	t.Setenv("AICC_PROVIDER_SESSION_PERIOD", "1s")
	t.Setenv("AICC_PROVIDER_TOKEN_RPM", "300000")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := voiceProfile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "quota, session starts: 3 per 1s (AICC_PROVIDER_SESSION_LIMIT), tokens: 300000 per 60s (AICC_PROVIDER_TOKEN_RPM)"
	if got := providerQuotaSummary(cfg, profile); got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	for _, tt := range []struct {
		limit  int
		period time.Duration
		want   int
	}{{3, time.Second, 180}, {60000, 12 * time.Second, 300000}, {60, time.Minute, 60}, {1000, time.Hour, 16}, {0, time.Second, 0}} {
		if got := perMinute(tt.limit, tt.period); got != tt.want {
			t.Errorf("perMinute(%d, %s) = %d, want %d", tt.limit, tt.period, got, tt.want)
		}
	}
	if got := quotaPeriod(1500 * time.Millisecond); got != "1.5s" {
		t.Errorf("quotaPeriod(1.5s) = %q", got)
	}
}

// Every session start is counted on the rate meter once, by the orchestrator,
// before the session is made (#644: a campaign call's start waits there for
// the session window first). The factory, whichever client answers, counts
// none of its own, or each start would count twice; the orchestrator is wired
// to the same meter the sessions report their tokens and refusals to.
func TestEverySessionStartIsCountedOnceOnTheMeter(t *testing.T) {
	for _, keyEnv := range []string{"ALIYUN_API_KEY", "DOUBAO_API_KEY"} {
		t.Setenv(keyEnv, "not-a-real-key")
	}
	meter := ratemeter.New(nil, ratemeter.Config{})
	sessions := voiceSessions(meter)
	for _, profile := range []provider.Profile{provider.QwenProfile(), provider.DoubaoProfile()} {
		if _, err := sessions(profile, nil); err != nil {
			t.Fatalf("%s: %v", profile.Name, err)
		}
	}
	if got := meter.Read().Starts; got != 0 {
		t.Errorf("the factory counted %d starts on the meter, want none: the orchestrator counts them", got)
	}
	cfg := botConfig(botUAS(config.Config{}, nil), nil, nil, nil, nil, nil, nil, provider.Profile{},
		nil, nil, nil, nil, nil, false, 0, 0, 0, "", false, "", phonenum.FormE164, meter, nil)
	if cfg.SessionStarts != aicall.SessionStarts(meter) {
		t.Errorf("the orchestrator counts starts on %v, want the process's meter", cfg.SessionStarts)
	}
	// Doctor's factory has no meter and still works.
	if _, err := voiceSessions(nil)(provider.QwenProfile(), nil); err != nil {
		t.Errorf("a factory without a meter: %v", err)
	}
}

// The key the startup check asks for is the one the provider's client will
// read when a call arrives, for every provider this process can run — not a
// name written into the check. A deployment on doubao missing ALIYUN_API_KEY
// has nothing to be told.
func TestAMissingProviderKeyIsReportedByTheNameItsClientReads(t *testing.T) {
	t.Parallel()
	for _, name := range []string{provider.NameOpenAI, provider.NameGrok, provider.NameQwen, provider.NameGateway, provider.NameDoubao} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			profile, err := provider.ProfileFor(name, provider.Override{})
			if err != nil {
				t.Fatal(err)
			}
			if profile.APIKeyEnv == "" {
				t.Fatalf("profile %s names no credential variable", name)
			}

			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			if !reportMissingProviderKey(log, profile, func(string) string { return "" }) {
				t.Fatal("an unset key was not reported")
			}
			line := buf.String()
			for _, want := range []string{"level=ERROR", profile.APIKeyEnv} {
				if !strings.Contains(line, want) {
					t.Errorf("report %q does not mention %q", line, want)
				}
			}

			buf.Reset()
			set := func(k string) string {
				if k == profile.APIKeyEnv {
					return "sk-test"
				}
				return ""
			}
			if reportMissingProviderKey(log, profile, set) || buf.Len() != 0 {
				t.Errorf("a key that is set was reported: %q", buf.String())
			}
		})
	}
}

// A deployment left over from the removed Gemini provider names it in .env (with
// its key still set) or in the real environment. Neither may crash the loader:
// both come out as the same refusal at startup, naming the values that run.
func TestAGeminiDeploymentIsRefusedWithTheAcceptedValuesFromEnvOrDotEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"a real environment variable", func(t *testing.T) {
			t.Setenv("AICC_PROVIDER", "gemini")
		}},
		{"a .env file", func(t *testing.T) {
			dir := t.TempDir()
			dotEnv := "GEMINI_API_KEY=not-a-real-key\nAICC_PROVIDER=gemini\n"
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotEnv), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			// loadDotEnv lets the real environment win, so make sure it is unset.
			t.Setenv("AICC_PROVIDER", "")
			if err := os.Unsetenv("AICC_PROVIDER"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			if cfg.Provider != "gemini" {
				t.Fatalf("Provider = %q, want gemini", cfg.Provider)
			}
			_, err = voiceProfile(cfg)
			if err == nil {
				t.Fatal("the removed provider gemini was accepted")
			}
			for _, name := range []string{"openai", "qwen", "gateway", "doubao"} {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the refusal does not name %q: %v", name, err)
				}
			}
		})
	}
}

// The AI draft of a wrap-up is on only when every part of it is: the text model
// configured with a key, the feature not switched off, and live transcription
// enabled. Anything less attaches nothing, and after-call work is what it was.
func TestTheWrapUpDraftIsInertUnlessEverythingItNeedsIsConfigured(t *testing.T) {
	on := config.Config{
		TextLLMEndpoint: "https://llm.example/v1", TextLLMModel: "m",
		TextLLMTimeout: time.Second, TextLLMContextChars: 8000,
		IsWrapUpDraftEnabled: true, IsTranscriptionEnabled: true,
	}
	withKey := func(k string) string {
		if k == "TEXT_LLM_API_KEY" {
			return "secret"
		}
		return ""
	}
	noKey := func(string) string { return "" }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name   string
		mutate func(c *config.Config)
		getenv func(string) string
		want   bool
	}{
		{"everything configured", func(*config.Config) {}, withKey, true},
		{"no endpoint", func(c *config.Config) { c.TextLLMEndpoint = "" }, withKey, false},
		{"no model", func(c *config.Config) { c.TextLLMModel = "" }, withKey, false},
		{"no key", func(*config.Config) {}, noKey, false},
		{"switched off", func(c *config.Config) { c.IsWrapUpDraftEnabled = false }, withKey, false},
		{"transcription off", func(c *config.Config) { c.IsTranscriptionEnabled = false }, withKey, false},
	}
	for _, c := range cases {
		cfg := on
		c.mutate(&cfg)
		got := wrapUpDrafter(cfg, nil, nil, c.getenv, log)
		if (got != nil) != c.want {
			t.Errorf("%s: drafter present = %v, want %v", c.name, got != nil, c.want)
		}
	}
}

// The lead judge judges only when every part of it is there: the feature on,
// the text model configured with a key, and the caller's words transcribed by
// the conversation session. Live transcription of the human phase is not one
// of them. Anything less still builds a service, which only settles deadlines.
func TestTheLeadJudgeJudgesOnlyWhenEverythingItNeedsIsConfigured(t *testing.T) {
	on := config.Config{
		TextLLMEndpoint: "https://llm.example/v1", TextLLMModel: "m",
		TextLLMTimeout: time.Second, TextLLMContextChars: 8000,
		IsLeadJudgeEnabled: true, LeadJudgeIntents: []string{"HIGH"},
	}
	withKey := func(k string) string {
		if k == "TEXT_LLM_API_KEY" {
			return "secret"
		}
		return ""
	}
	noKey := func(string) string { return "" }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name   string
		mutate func(c *config.Config)
		getenv func(string) string
		want   bool
	}{
		{"everything configured", func(*config.Config) {}, withKey, true},
		{"live transcription off is not needed", func(c *config.Config) { c.IsTranscriptionEnabled = false }, withKey, true},
		{"a caller transcription model named", func(c *config.Config) { c.ProviderTranscribeModel = "gummy" }, withKey, true},
		{"no endpoint", func(c *config.Config) { c.TextLLMEndpoint = "" }, withKey, false},
		{"no model", func(c *config.Config) { c.TextLLMModel = "" }, withKey, false},
		{"no key", func(*config.Config) {}, noKey, false},
		{"switched off", func(c *config.Config) { c.IsLeadJudgeEnabled = false }, withKey, false},
		{"caller transcription off", func(c *config.Config) { c.ProviderTranscribeModel = "OFF" }, withKey, false},
		{"no lead listed", func(c *config.Config) { c.LeadJudgeIntents = nil }, withKey, false},
	}
	for _, c := range cases {
		cfg := on
		c.mutate(&cfg)
		got := leadJudge(cfg, nil, c.getenv, log)
		if got == nil {
			t.Fatalf("%s: no service; one that settles deadlines is always built", c.name)
		}
		if got.IsJudging() != c.want {
			t.Errorf("%s: judging = %v, want %v", c.name, got.IsJudging(), c.want)
		}
		if !c.want && got.JudgedIntents() != nil {
			t.Errorf("%s: the store would owe judgements for %v", c.name, got.JudgedIntents())
		}
	}
}

// The store owes a judgement for exactly the leads AICC_LEAD_JUDGE_INTENTS
// lists, and the startup line names them. Every value config accepts is a lead
// a judge can read.
func TestTheLeadJudgeJudgesTheListedIntents(t *testing.T) {
	withKey := func(k string) string {
		if k == "TEXT_LLM_API_KEY" {
			return "secret"
		}
		return ""
	}
	for _, list := range [][]string{{"HIGH"}, {"MEDIUM"}, {"HIGH", "MEDIUM"}, {"MEDIUM", "HIGH"}} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		cfg := config.Config{
			TextLLMEndpoint: "https://llm.example/v1", TextLLMModel: "m",
			TextLLMTimeout: time.Second, TextLLMContextChars: 8000,
			IsLeadJudgeEnabled: true, LeadJudgeIntents: list,
		}
		got := leadJudge(cfg, nil, withKey, log).JudgedIntents()
		want := leadJudgeIntents(list)
		if !slices.Equal(got, want) {
			t.Errorf("%v: judged %v, want %v", list, got, want)
		}
		for _, lead := range campaign.AllLeadIntents() {
			if got.IsJudged(lead) != slices.Contains(list, string(lead)) {
				t.Errorf("%v: IsJudged(%s) = %v", list, lead, got.IsJudged(lead))
			}
		}
		if line := buf.String(); !strings.Contains(line, "campaign leads are judged") ||
			!strings.Contains(line, "intents="+strings.Join(list, ",")) {
			t.Errorf("%v: the startup line does not name the intents:\n%s", list, line)
		}
	}
	for _, v := range config.LeadJudgeableIntents() {
		if !campaign.IsLeadJudgeable(campaign.LeadIntent(v)) {
			t.Errorf("config accepts %q, which no judge reads", v)
		}
	}
	for _, lead := range campaign.AllLeadIntents() {
		if campaign.IsLeadJudgeable(lead) && !slices.Contains(config.LeadJudgeableIntents(), string(lead)) {
			t.Errorf("config refuses %s, which a judge reads", lead)
		}
	}
}

func TestARemovedSettingStillSetIsWarnedAboutOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := map[string]string{"AICC_BOT_ADVERTISE_IP": "192.0.2.1"}
	warnRemovedSettings(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if got := strings.Count(buf.String(), "AICC_BOT_ADVERTISE_IP"); got != 1 {
		t.Errorf("warned %d times, want once:\n%s", got, buf.String())
	}

	buf.Reset()
	warnRemovedSettings(func(string) (string, bool) { return "", false })
	if buf.Len() != 0 {
		t.Errorf("warned with the setting unset:\n%s", buf.String())
	}
}
