// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	metricsdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/doubao"
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

// The composition has two ways to be wrong and both are silent.
//
// Drop the retirement and every call leaves a goroutine and its mailbox behind
// for the life of the process — invisible until a long shift runs out of
// memory. Drop the predecessor and the CDR assembler stops being called, which
// costs a call record to free a goroutine: the worse trade of the two.
func TestRetiringATranscriptDoesNotDisplaceWhatAlreadyListens(t *testing.T) {
	var priorSaw []uuid.UUID
	prior := func(s telephony.Snapshot) { priorSaw = append(priorSaw, s.CallID) }
	transcripts := &retirer{}

	callID := uuid.New()
	retireTranscriptWithCall(prior, transcripts)(telephony.Snapshot{CallID: callID})

	if len(priorSaw) != 1 || priorSaw[0] != callID {
		t.Errorf("the existing hook saw %v, want the finished call — a CDR is not "+
			"something to lose in order to free a goroutine", priorSaw)
	}
	if len(transcripts.closed) != 1 || transcripts.closed[0] != callID {
		t.Errorf("retired %v, want the finished call", transcripts.closed)
	}
}

// Nothing has to be listening first.
func TestRetiringATranscriptWorksWithNoPredecessor(t *testing.T) {
	transcripts := &retirer{}
	callID := uuid.New()

	retireTranscriptWithCall(nil, transcripts)(telephony.Snapshot{CallID: callID})

	if len(transcripts.closed) != 1 || transcripts.closed[0] != callID {
		t.Errorf("retired %v, want the finished call", transcripts.closed)
	}
}

type detacher struct{ calls []uuid.UUID }

func (d *detacher) DetachCall(id uuid.UUID) { d.calls = append(d.calls, id) }

// Same two silent failures as the transcript hook, on the other resource.
//
// Drop the detach and mod_audio_stream keeps pumping a finished call's audio
// at an ingest whose transcript actor has been closed, for the life of the
// process. Drop the predecessor and whatever was already listening on call
// retirement stops running.
func TestDetachingTapsDoesNotDisplaceWhatAlreadyListens(t *testing.T) {
	var priorSaw []uuid.UUID
	prior := func(id uuid.UUID) { priorSaw = append(priorSaw, id) }
	taps := &detacher{}

	callID := uuid.New()
	detachTapsWithCall(prior, taps)(callID)

	if len(priorSaw) != 1 || priorSaw[0] != callID {
		t.Errorf("the existing hook saw %v, want the retired call", priorSaw)
	}
	if len(taps.calls) != 1 || taps.calls[0] != callID {
		t.Errorf("detached %v, want the retired call", taps.calls)
	}
}

// Nothing has to be listening first — and nothing is, today.
func TestDetachingTapsWorksWithNoPredecessor(t *testing.T) {
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
// a turn, so none of them demands anything. Doubao cannot be, so every ending a
// call can stop at has to carry its own words — and a deployment with the AI
// leg switched off demands nothing either, whatever provider its configuration
// names: there is no session to have shortcomings.
func TestWhatAPublishMustSatisfyFollowsFromWhatAnswersTheCalls(t *testing.T) {
	speaksOnDemand := provider.DoubaoProfile()

	for _, tc := range []struct {
		name         string
		isBotEnabled bool
		profile      provider.Profile
		wantRules    int
	}{
		{"a provider that can be cued asks for nothing", true, provider.OpenAIProfile(), 0},
		{"a provider that cannot needs every ending written", true, speaksOnDemand, 1},
		{"with no AI leg there is nothing to satisfy", false, speaksOnDemand, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
// Getting it wrong is silent in every way that matters: both clients satisfy
// provider.VoiceSession, both are built without touching the network, and the
// process starts. A doubao deployment handed the Realtime client would dial the
// right address speaking the wrong protocol, and the first real call would be
// the first thing to notice.
func TestTheProviderNameChoosesTheClient(t *testing.T) {
	for _, keyEnv := range []string{
		"OPENAI_API_KEY", "ALIYUN_API_KEY", "REALTIME_API_KEY", "DOUBAO_API_KEY",
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

// Every session opened is counted, because the vendor's limit is on opening
// them. Doubao allows sixty a minute per application id, and nothing else this
// process measures would show that being approached: the live-call gauge counts
// how many are up, not how fast they were created, and a deployment can sit
// well inside its concurrency and still be turned away at the door.
func TestEveryProviderSessionOpenedIsCounted(t *testing.T) {
	t.Setenv("DOUBAO_API_KEY", "not-a-real-key")

	reader := metricsdk.NewManualReader()
	otel.SetMeterProvider(metricsdk.NewMeterProvider(metricsdk.WithReader(reader)))

	before := sessionsStarted(t, reader, provider.NameDoubao)
	for range 3 {
		if _, err := voiceSession(provider.DoubaoProfile(), nil); err != nil {
			t.Fatalf("voiceSession: %v", err)
		}
	}
	if got := sessionsStarted(t, reader, provider.NameDoubao) - before; got != 3 {
		t.Errorf("three sessions opened counted %d; a quota nobody can see being "+
			"spent is a quota that runs out during an incident", got)
	}
}

// sessionsStarted reads the counter back for one provider. Cumulative, so the
// callers take a difference and no test has to run first.
func sessionsStarted(t *testing.T, reader *metricsdk.ManualReader, name string) int64 {
	t.Helper()

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "aicc_provider_sessions_started_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is a %T, want an int64 sum", m.Name, m.Data)
			}
			for _, point := range sum.DataPoints {
				if value, found := point.Attributes.Value("provider"); found &&
					value.AsString() == name {
					return point.Value
				}
			}
		}
	}
	return 0
}
