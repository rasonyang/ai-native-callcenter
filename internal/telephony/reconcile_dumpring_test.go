// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The fixture is a `uuid_dump <uuid> json` captured on the e2e stack from a
// call the application placed to the callee simulator (an AI_OUTBOUND to
// +12025550101, which rings then answers): the leg that came back carries
// Caller-Channel-Progress-Time beside Caller-Channel-Answered-Time, which is
// the evidence the reconcile path was written for — an adopted leg whose live
// CHANNEL_ANSWER was missed replays the answer from the dump and must keep
// the ringing that preceded it, or its CDR reports zero ring time (#591).
func dumpedFixture(t *testing.T) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/dumps/uuid_dump_answered_leg.json")
	if err != nil {
		t.Fatal(err)
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Unique-ID", "variable_aicc_call_id",
		"Caller-Channel-Progress-Time", "Caller-Channel-Answered-Time"} {
		if headers[k] == "" {
			t.Fatalf("the fixture lost %s, the field the test exists to read", k)
		}
	}
	return headers, string(raw)
}

func microsFromDump(t *testing.T, headers map[string]string, name string) time.Time {
	t.Helper()
	us, err := strconv.ParseInt(headers[name], 10, 64)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return time.UnixMicro(us).UTC()
}

func TestAnAnsweredLegReplayedFromItsDumpKeepsItsRingStart(t *testing.T) {
	t.Parallel()
	headers, dump := dumpedFixture(t)
	id, callID := headers["Unique-ID"], headers["variable_aicc_call_id"]

	listings := 0
	cmd := &fakeCommander{up: true, replyFor: func(c string) (string, error) {
		switch {
		case c == "show channels as json":
			listings++
			if listings == 1 {
				return `{"row_count":1,"rows":[{"uuid":"` + id + `","callstate":"ACTIVE","created_epoch":"` +
					headers["Caller-Channel-Created-Time"][:10] + `"}]}`, nil
			}
			return `{"row_count":0,"rows":[]}`, nil
		case strings.HasPrefix(c, "uuid_dump "):
			return dump, nil
		}
		return "", nil
	}}
	reg := NewRegistry(nullPublisher{})
	t.Cleanup(reg.Shutdown)
	ledger := &memoryLedger{}
	c := NewCoordinator(reg, NewAdapter(cmd, "aicc.test"), noAgents{}, nullPublisher{})
	c.AttachCDR(NewCDRAssembler(ledger, staticQueues{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))

	live, err := c.AdoptLiveChannels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live = %+v, want the dumped answered leg adopted", live)
	}

	var ringStart time.Time
	if err := reg.Do(uuid.MustParse(callID), func(call *Call) {
		if p := call.PartyByChannel(id); p != nil {
			ringStart = p.RingStartedAt
		}
	}); err != nil {
		t.Fatal(err)
	}
	wantRingStart := microsFromDump(t, headers, "Caller-Channel-Progress-Time")
	if !ringStart.Equal(wantRingStart) {
		t.Fatalf("RingStartedAt = %v, want the dump's progress time %v", ringStart, wantRingStart)
	}

	// The switch forgets the leg before the next listing; its close is
	// synthesized as vanished, and the ringing the dump kept is what the CDR
	// reports: first ring to answer, not zero.
	if _, err := c.AdoptLiveChannels(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !reg.Owns(id) })
	waitFor(t, func() bool {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		return len(ledger.cdrs) == 1
	})
	wantRingSec := int(microsFromDump(t, headers, "Caller-Channel-Answered-Time").
		Sub(wantRingStart).Seconds())
	if cdr := ledger.cdrs[0]; cdr.RingSec != wantRingSec {
		t.Fatalf("cdr.RingSec = %d, want %d (first ring to answer, from the dump)", cdr.RingSec, wantRingSec)
	}
}
