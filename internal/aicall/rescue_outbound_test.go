// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// A bot leg that answered and then could not start its media (#459). The UAS
// calls OnCallFailed on the goroutine that sends the BYE next, so what the
// orchestrator does here is done before the bot leg goes.

type rescueWorld struct {
	sw *fakeSwitch
	o  *Orchestrator
}

// newRescueWorld has three numbers: 95001 with an enabled fallback queue,
// 95002 with a disabled one, 95003 with none.
func newRescueWorld(t *testing.T) *rescueWorld {
	t.Helper()
	open := catalog.Queue{ID: uuid.Must(uuid.NewV7()), Name: "support", ExtNumber: "7001", IsEnabled: true}
	closed := catalog.Queue{ID: uuid.Must(uuid.NewV7()), Name: "after-hours", ExtNumber: "7002"}
	flowID := uuid.Must(uuid.NewV7())
	sw := &fakeSwitch{}
	o, err := NewOrchestrator(OrchestratorConfig{
		Catalog: &fakeCatalog{
			queues: []catalog.Queue{open, closed},
			dids: []catalog.DID{
				{Number: "95001", IsEnabled: true, FlowID: &flowID, FallbackQueueID: &open.ID},
				{Number: "95002", IsEnabled: true, FlowID: &flowID, FallbackQueueID: &closed.ID},
				{Number: "95003", IsEnabled: true, FlowID: &flowID},
			},
		},
		Flows:   fakeFlows{},
		Switch:  sw,
		Profile: provider.OpenAIProfile(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	return &rescueWorld{sw: sw, o: o}
}

// fail is the UAS's report of a leg whose media did not start. The dialog is
// not stopped yet, as in startCall; the hook records whether it still was when
// the switch was told something.
func (w *rescueWorld) fail(t *testing.T, headers map[string]string) (stoppedAtSwitch []bool, dialog *voice.Dialog) {
	t.Helper()
	dialog = &voice.Dialog{
		SIPCallID:     "sip-call-1",
		CustomHeaders: headers,
		Stopped:       make(chan struct{}),
		RTP:           voice.NewRTPSession(0, media.LawMu, 101, nil),
	}
	w.sw.onTransfer = func() { stoppedAtSwitch = append(stoppedAtSwitch, dialog.IsStopped()) }
	w.o.uas.OnCallFailed(dialog, context.DeadlineExceeded)
	if !dialog.IsStopped() {
		t.Error("the bot leg was left up")
	}
	return stoppedAtSwitch, dialog
}

func placed(did, language string, extra ...string) map[string]string {
	h := map[string]string{
		headerChannelID:        "customer-leg-1",
		headerDID:              did,
		headerLanguage:         language,
		headerPlacedToCustomer: "true",
		headerCallType:         "OUTBOUND",
	}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

func TestAMediaFailureOnAnOutboundCallRescuesTheCustomerBeforeTheBotLegEnds(t *testing.T) {
	t.Run("a single call goes to its number's enabled fallback queue", func(t *testing.T) {
		w := newRescueWorld(t)
		stopped, _ := w.fail(t, placed("95001", "zh"))
		if got, want := w.sw.recordedOps(), []string{"stamp customer-leg-1", "handOn customer-leg-1", "transfer customer-leg-1→7001"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want %v", got, want)
		}
		if len(stopped) != 1 || stopped[0] {
			t.Errorf("the bot leg had already ended when the transfer was sent: %v", stopped)
		}
		if got := w.sw.variable("aicc_bot_reason"); got != "BOT_LEG_FAILED" {
			t.Errorf("aicc_bot_reason = %q", got)
		}
	})

	t.Run("a disabled fallback queue plays the prompt in the call's language", func(t *testing.T) {
		w := newRescueWorld(t)
		stopped, _ := w.fail(t, placed("95002", "zh"))
		if got, want := w.sw.recordedOps(), []string{"stamp customer-leg-1", "prompt customer-leg-1 zh"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want %v", got, want)
		}
		if len(stopped) != 1 || stopped[0] {
			t.Errorf("the bot leg had already ended when the prompt was sent: %v", stopped)
		}
	})

	t.Run("no fallback queue plays the prompt", func(t *testing.T) {
		w := newRescueWorld(t)
		w.fail(t, placed("95003", "en"))
		if got, want := w.sw.recordedOps(), []string{"stamp customer-leg-1", "prompt customer-leg-1 en"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want %v", got, want)
		}
	})

	t.Run("an unknown number plays the prompt", func(t *testing.T) {
		w := newRescueWorld(t)
		w.fail(t, placed("95999", "fr"))
		if got, want := w.sw.recordedOps(), []string{"stamp customer-leg-1", "prompt customer-leg-1 fr"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want %v (the language map is the switch adapter's)", got, want)
		}
	})

	t.Run("a campaign call is stamped and never moved, even with an enabled queue", func(t *testing.T) {
		w := newRescueWorld(t)
		w.fail(t, placed("95001", "zh", headerCampaignID, uuid.NewString(), headerAttemptID, uuid.NewString()))
		if got, want := w.sw.recordedOps(), []string{"stamp customer-leg-1"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want only the stamp: ending the bot leg hangs the customer up", got)
		}
	})

	t.Run("without the customer's channel nothing is sent", func(t *testing.T) {
		w := newRescueWorld(t)
		h := placed("95001", "zh")
		delete(h, headerChannelID)
		w.fail(t, h)
		if got := w.sw.recordedOps(); len(got) != 0 {
			t.Errorf("ops = %v, want none", got)
		}
	})

	t.Run("an inbound call keeps today's rescue: the queue, no stamp, no prompt", func(t *testing.T) {
		w := newRescueWorld(t)
		w.fail(t, map[string]string{headerChannelID: "caller-1", headerDID: "95001"})
		if got, want := w.sw.recordedOps(), []string{"handOn caller-1", "transfer caller-1→7001"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want %v", got, want)
		}
	})

	t.Run("an inbound call on a disabled queue is still sent there (the dialplan reads the same column)", func(t *testing.T) {
		w := newRescueWorld(t)
		w.fail(t, map[string]string{headerChannelID: "caller-1", headerDID: "95002"})
		if got, want := w.sw.recordedOps(), []string{"handOn caller-1", "transfer caller-1→7002"}; !slices.Equal(got, want) {
			t.Errorf("ops = %v, want %v", got, want)
		}
	})

	t.Run("an inbound call with no queue gets nothing", func(t *testing.T) {
		w := newRescueWorld(t)
		w.fail(t, map[string]string{headerChannelID: "caller-1", headerDID: "95003"})
		if got := w.sw.recordedOps(); len(got) != 0 {
			t.Errorf("ops = %v, want none", got)
		}
	})
}

// The teardown rule a failed handover restores is the one the bridge set: off
// for an inbound call, whose dialplan owns the fallback, on for a call the
// platform placed, whose bridge continuation must not run after a bot that
// hangs up normally.
func TestAFailedHandoverRestoresTheRuleTheBridgeSet(t *testing.T) {
	for _, tt := range []struct {
		name     string
		facts    *callFacts
		wantRule string
	}{
		{"inbound", &callFacts{}, "false"},
		{"no facts", nil, "false"},
		{"placed to a customer", &callFacts{isPlacedToCustomer: true}, "true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sw := &fakeSwitch{}
			actions, _, _ := testActions(t, sw)
			actions.facts = tt.facts
			actions.handoverFailed(failedHandover{
				queue: catalog.Queue{Name: "support", ExtNumber: "7001"}, fromPhase: "main",
			}, true)
			if got := sw.variable("hangup_after_bridge"); got != tt.wantRule {
				t.Errorf("hangup_after_bridge = %q, want %q", got, tt.wantRule)
			}
		})
	}
}
