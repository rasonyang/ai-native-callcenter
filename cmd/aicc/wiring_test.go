// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/doubao"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/gemini"
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
	callID := uuid.New()

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
	callID := uuid.New()

	detachTapsWithCall(nil, taps)(callID)

	if len(taps.calls) != 1 || taps.calls[0] != callID {
		t.Errorf("detached %v, want the retired call", taps.calls)
	}
}

// What a deployment demands of a flow follows from what answers its calls.
//
// The three engines reached over the Realtime protocol can all be prompted into
// a turn, and so can gemini, so none of them demands anything. Doubao cannot be,
// so every ending a call can stop at has to carry its own words — and a
// deployment with the AI leg switched off demands nothing either, whatever
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
		{"a provider that can be cued asks for nothing", true, provider.OpenAIProfile(), 0},
		{"and a third protocol that can be cued asks for nothing either", true,
			provider.GeminiProfile(), 0},
		{"a provider that cannot needs every ending written", true, speaksOnDemand, 1},
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

// A misspelled provider is a misspelling whether or not the AI leg is on. It
// surfaces at startup rather than the first time somebody turns the bot on,
// which would be during an incident.
func TestAnUnknownProviderNameIsAStartupFailure(t *testing.T) {
	t.Parallel()
	if _, err := voiceProfile(config.Config{Provider: "nonesuch"}); err == nil {
		t.Error("an unknown provider name was accepted")
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
		"OPENAI_API_KEY", "ALIYUN_API_KEY", "REALTIME_API_KEY", "DOUBAO_API_KEY",
		"GEMINI_API_KEY",
	} {
		t.Setenv(keyEnv, "not-a-real-key")
	}

	for _, tc := range []struct {
		profile provider.Profile
		want    provider.VoiceSession
	}{
		{provider.DoubaoProfile(), (*doubao.Session)(nil)},
		{provider.GeminiProfile(), (*gemini.Session)(nil)},
		{provider.OpenAIProfile(), (*provider.Realtime)(nil)},
		{provider.QwenProfile(), (*provider.Realtime)(nil)},
		{provider.GatewayProfile(), (*provider.Realtime)(nil)},
	} {
		t.Run(tc.profile.Name, func(t *testing.T) {
			session, err := voiceSession(tc.profile, nil)
			if err != nil {
				t.Fatalf("voiceSession: %v", err)
			}
			if got, want := reflect.TypeOf(session), reflect.TypeOf(tc.want); got != want {
				t.Errorf("%s is answered by %v, want %v", tc.profile.Name, got, want)
			}
		})
	}
}

// The key the startup check asks for is the one the provider's client will
// read when a call arrives, for every provider this process can run — not a
// name written into the check. A deployment on doubao missing ALIYUN_API_KEY
// has nothing to be told.
func TestAMissingProviderKeyIsReportedByTheNameItsClientReads(t *testing.T) {
	t.Parallel()
	for _, name := range []string{provider.NameOpenAI, provider.NameQwen, provider.NameGateway, provider.NameDoubao, provider.NameGemini} {
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
