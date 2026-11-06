// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// recordingSwitch is the switch behind a real outbound.Service: it records the
// originates and accepts everything else.
type recordingSwitch struct {
	mu         sync.Mutex
	originates []string // the endpoints, dial string included
}

func (s *recordingSwitch) Originate(_ uuid.UUID, endpoint string, _ map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.originates = append(s.originates, endpoint)
	return "job", nil
}
func (s *recordingSwitch) BridgeToBot(string, uuid.UUID, string, map[string]string, telephony.BotFallback) error {
	return nil
}
func (s *recordingSwitch) TransferToExtension(string, string, string) error { return nil }
func (s *recordingSwitch) Hangup(string, string) error                      { return nil }
func (s *recordingSwitch) SetVariable(string, string, string) error         { return nil }
func (s *recordingSwitch) ChannelExists(string) (bool, error)               { return true, nil }
func (s *recordingSwitch) StartCallRecording(string, string) error          { return nil }
func (s *recordingSwitch) Endpoint(ext string) string                       { return "user/" + ext }

func (s *recordingSwitch) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.originates...)
}

type oneDID []catalog.DID

func (d oneDID) DIDs(context.Context) ([]catalog.DID, error) { return d, nil }

type noInternalNumbers struct{}

func (noInternalNumbers) IsInternalNumber(context.Context, string) (bool, error) { return false, nil }

// listsBeforePlacing puts the numbers it is told to on the real do-not-call
// list at the moment the dialer hands the call over: after the contact was
// selected (Due) and its attempt written ahead, before the placer's own last
// check. It writes the table directly, so the dialer is not told (no
// DNCAdded): the placer's check is the only thing left to catch it.
type listsBeforePlacing struct {
	inner  Placer
	exec   func(ctx context.Context, number string) error
	listed map[string]bool // keyed by the stored canonical number
	t      *testing.T
}

func (l *listsBeforePlacing) PlaceAI(ctx context.Context, req outbound.AIDialRequest) (outbound.Placement, error) {
	if l.listed[req.CanonicalTo] {
		if err := l.exec(ctx, req.CanonicalTo); err != nil {
			l.t.Errorf("list %s: %v", req.CanonicalTo, err)
		}
	}
	return l.inner.PlaceAI(ctx, req)
}

// The last do-not-call check is the real one, on the number the contact is
// stored under, not on the trunk's dial string (#477). 1,000 contacts under
// E164_NO_PLUS (the form whose dial string the check used to mis-canonicalise);
// 100 join the list between selection and placement. Not one of those 100 is
// originated, each settles DNC_SKIPPED with its contact on the do-not-call
// list, and the other 900 are placed with the dial string unchanged.
func TestANumberListedBetweenSelectionAndPlacementIsNeverOriginated(t *testing.T) {
	const total, listedEvery = 1000, 10
	r := newRig(t, opts{})
	r.stopDialer()
	r.cfg.DialForm = phonenum.FormE164NoPlus
	r.cfg.DefaultCountryCode = "86"

	sw := &recordingSwitch{}
	flow := uuid.Must(uuid.NewV7())
	svc := outbound.New(
		outbound.Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s", RatePerSec: 1_000_000},
		sw,
		oneDID{{Number: r.didNum, IsEnabled: true, AllowOutbound: true, FlowID: &flow, Language: "en"}},
		noInternalNumbers{},
		func(context.Context, uuid.UUID) (bool, error) { return false, nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).WithDNC(r.st.DNC())

	specs := numbers("7", total)
	listed := map[string]bool{}
	for i, s := range specs {
		if i%listedEvery == 0 {
			listed[s.number] = true
		}
	}
	if len(listed) != 100 {
		t.Fatalf("test setup: %d listed numbers, want 100", len(listed))
	}
	r.dep.Placer = &listsBeforePlacing{
		inner: svc, t: t, listed: listed,
		exec: func(ctx context.Context, number string) error {
			_, err := r.st.Pool.Exec(ctx, `INSERT INTO dnc_numbers (number_e164, source) VALUES ($1, 'MANUAL')`, number)
			return err
		},
	}
	r.dep.IsLive = func(uuid.UUID) bool { return true }
	r.startDialer()

	c := r.newCampaign("Listed late", func(s *camSpec) { s.cc = "86"; s.max = total })
	r.importContacts(c, specs)
	r.start(c)

	sent := map[string]bool{}
	for _, endpoint := range sw.sent() {
		sent[strings.TrimPrefix(endpoint, "sofia/gateway/pstn_gateway/")] = true
	}
	if len(sent) != total-len(listed) {
		t.Errorf("%d numbers originated, want %d", len(sent), total-len(listed))
	}
	attempts := map[string]campaign.Attempt{}
	for _, a := range r.attempts(c) {
		attempts[a.Number] = a
	}
	contacts := map[string]campaign.Contact{}
	for offset := 0; ; offset += 1000 {
		page, _, err := r.st.Campaigns().ListContacts(context.Background(), c.ID, campaign.ContactFilter{Limit: 1000, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		for _, ct := range page {
			contacts[ct.Number] = ct
		}
		if len(page) < 1000 {
			break
		}
	}
	if len(contacts) != total || len(attempts) != total {
		t.Fatalf("%d contacts and %d attempts, want %d of each", len(contacts), len(attempts), total)
	}
	for _, s := range specs {
		dialled, _ := phonenum.DialString(s.number, phonenum.FormE164NoPlus, "86")
		a, ct := attempts[s.number], contacts[s.number]
		if listed[s.number] {
			if sent[dialled] {
				t.Errorf("%s is on the list and was originated", s.number)
			}
			if a.Outcome != campaign.OutcomeDNCSkipped || ct.Status != campaign.ContactDoNotCall {
				t.Errorf("%s: attempt %s, contact %s; want DNC_SKIPPED and the contact on the list", s.number, a.Outcome, ct.Status)
			}
			continue
		}
		if !sent[dialled] {
			t.Errorf("%s was not originated as %s", s.number, dialled)
		}
		if a.State != campaign.AttemptDialing {
			t.Errorf("%s: attempt state %s, want DIALING", s.number, a.State)
		}
	}
}
