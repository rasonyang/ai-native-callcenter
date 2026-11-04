// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// A catalog that also knows the queues, as the catalog service does.
type catalogWithQueues struct {
	dids      []catalog.DID
	queues    []catalog.Queue
	queuesErr error
}

func (c catalogWithQueues) DIDs(context.Context) ([]catalog.DID, error) { return c.dids, nil }
func (c catalogWithQueues) Queues(context.Context) ([]catalog.Queue, error) {
	return c.queues, c.queuesErr
}

var _ QueueSource = (*catalog.Service)(nil)

func fallbackService(t *testing.T, sw *fakeSwitch, dids DIDSource) *Service {
	t.Helper()
	return New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s"}, sw, dids, provisioned("7001"),
		func(context.Context, uuid.UUID) (bool, error) { return false, nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The command Go sends is the same whatever made the bot leg fail: a 503, a
// 486, a 480 and a gateway that is down all leave the switch with a bridge
// that never came up, and what it then does is the continuation Go chose
// before the bridge. So the fake switch needs no cause to reject with; what
// differs by cause is the CDR the switch writes (BOT_LEG_FAILED beside the
// switch's own hangup cause), and the attempt it finishes, covered in
// internal/campaign and internal/store.
func TestTheBridgeCarriesTheContinuationTheNumberCalls(t *testing.T) {
	t.Parallel()
	flowID := uuid.Must(uuid.NewV7())
	open := catalog.Queue{ID: uuid.Must(uuid.NewV7()), Name: "support", ExtNumber: "7001", IsEnabled: true}
	closed := catalog.Queue{ID: uuid.Must(uuid.NewV7()), Name: "after-hours", ExtNumber: "7002"}
	numbers := []catalog.DID{
		{Number: "95012", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, FallbackQueueID: &open.ID},
		{Number: "95013", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, FallbackQueueID: &closed.ID},
		{Number: "95014", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true},
		{Number: "95015", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, FallbackQueueID: &open.ID},
	}
	queues := []catalog.Queue{open, closed}
	campaign := AIDialRequest{CampaignID: uuid.Must(uuid.NewV7()), AttemptID: uuid.Must(uuid.NewV7())}

	tests := []struct {
		name   string
		did    string
		req    AIDialRequest
		cat    catalogWithQueues
		bridge telephony.BotFallback
	}{
		{"an enabled fallback queue", "95012", AIDialRequest{}, catalogWithQueues{numbers, queues, nil},
			telephony.FallbackQueue("7001", "zh")},
		{"a disabled fallback queue plays the prompt", "95013", AIDialRequest{}, catalogWithQueues{numbers, queues, nil},
			telephony.FallbackPrompt("zh")},
		{"no fallback queue plays the prompt", "95014", AIDialRequest{}, catalogWithQueues{numbers, queues, nil},
			telephony.FallbackPrompt("en")},
		{"a queue the catalog no longer has plays the prompt", "95012", AIDialRequest{}, catalogWithQueues{numbers, nil, nil},
			telephony.FallbackPrompt("zh")},
		{"a catalog error plays the prompt", "95012", AIDialRequest{}, catalogWithQueues{numbers, queues, errors.New("db down")},
			telephony.FallbackPrompt("zh")},
		{"a campaign call hangs up even with an enabled queue", "95015", campaign, catalogWithQueues{numbers, queues, nil},
			telephony.FallbackHangup()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sw := &fakeSwitch{}
			s := fallbackService(t, sw, tt.cat)
			req := tt.req
			req.To, req.DIDNumber = "13912345678", tt.did
			if _, err := s.PlaceAI(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			answer(s, sw.lastOriginate().partyID.String())
			waitBridges(t, sw, 1)
			if got := sw.lastBridge().fallback; got != tt.bridge {
				t.Errorf("fallback = %+v, want %+v", got, tt.bridge)
			}
		})
	}
}

// A DID source that cannot list queues (a fake, an old build) never sends a
// call to a person it cannot name: the prompt.
func TestWithoutAQueueSourceTheBridgePlaysThePrompt(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	flowID := uuid.Must(uuid.NewV7())
	queueID := uuid.Must(uuid.NewV7())
	s := fallbackService(t, sw, fakeDIDs{{Number: "95012", Language: "zh", FlowID: &flowID, IsEnabled: true,
		AllowOutbound: true, FallbackQueueID: &queueID}})
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"}); err != nil {
		t.Fatal(err)
	}
	answer(s, sw.lastOriginate().partyID.String())
	waitBridges(t, sw, 1)
	if got := sw.lastBridge().fallback; got != telephony.FallbackPrompt("zh") {
		t.Errorf("fallback = %+v", got)
	}
}
