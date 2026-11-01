// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recordingService serves two outbound numbers that differ only in whether
// they record. The flow id makes both AI numbers.
func recordingService(t *testing.T, sw *fakeSwitch) *Service {
	t.Helper()
	flowID := uuid.Must(uuid.NewV7())
	dids := fakeDIDs{
		{Number: "95201", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, IsRecordingEnabled: true},
		{Number: "95202", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, IsRecordingEnabled: false},
	}
	return New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s"}, sw, dids, provisioned(),
		func(context.Context, uuid.UUID) (bool, error) { return false, nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// An answered AI outbound leg on a recording number is recorded before it is
// bridged to the bot, under the key ingest looks for: the UTC date the call
// started, which is the registry's start and not the moment of the answer.
func TestAnAnsweredAICallIsRecordedBeforeTheBridge(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := recordingService(t, sw)
	// The call began just before midnight UTC and was answered after it.
	began := time.Date(2026, 10, 12, 23, 59, 58, 0, time.UTC)
	s.SetCallStart(func(uuid.UUID) (time.Time, bool) { return began, true })
	s.now = func() time.Time { return began.Add(5 * time.Second) }

	callID, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95201"})
	if err != nil {
		t.Fatal(err)
	}
	leg := sw.lastOriginate().partyID.String()
	answer(s, leg)
	waitBridges(t, sw, 1)

	sw.mu.Lock()
	defer sw.mu.Unlock()
	if want := []string{"record", "bridge"}; !slices.Equal(sw.order, want) {
		t.Fatalf("switch saw %v, want %v", sw.order, want)
	}
	want := recorded{leg, "2026/10/12/" + callID.String() + ".wav"}
	if len(sw.recordings) != 1 || sw.recordings[0] != want {
		t.Errorf("recordings = %v, want [%v]", sw.recordings, want)
	}
}

func TestARecordingDisabledNumberIsNotRecorded(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := recordingService(t, sw)

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95202"}); err != nil {
		t.Fatal(err)
	}
	answer(s, sw.lastOriginate().partyID.String())
	waitBridges(t, sw, 1)

	sw.mu.Lock()
	defer sw.mu.Unlock()
	if len(sw.recordings) != 0 {
		t.Errorf("recording started on a number that does not record: %v", sw.recordings)
	}
}

// A switch that refuses the recording does not cost the customer their call.
func TestARefusedRecordingStillBridgesToTheBot(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{recordErr: errors.New("-ERR no recordings directory")}
	s := recordingService(t, sw)

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95201"}); err != nil {
		t.Fatal(err)
	}
	answer(s, sw.lastOriginate().partyID.String())
	waitBridges(t, sw, 1)

	sw.mu.Lock()
	defer sw.mu.Unlock()
	if want := []string{"record", "bridge"}; !slices.Equal(sw.order, want) {
		t.Errorf("switch saw %v, want %v", sw.order, want)
	}
}
