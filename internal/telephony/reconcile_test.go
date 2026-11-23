// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTheListingIsReadWhateverShapeTheSwitchGivesIt(t *testing.T) {
	t.Parallel()
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	cases := map[string]struct {
		payload string
		want    int
	}{
		"empty":               {`{"row_count":0}`, 0},
		"two rows":            {`{"row_count":2,"rows":[{"uuid":"` + a.String() + `","callstate":"RINGING"},{"uuid":"` + b.String() + `","callstate":"ACTIVE","created_epoch":"1760000000"}]}`, 2},
		"numeric epoch":       {`{"rows":[{"uuid":"` + a.String() + `","callstate":"active","created_epoch":1760000000}]}`, 1},
		"not json":            {`+OK nothing`, 0},
		"cut in the 2nd row":  {`{"row_count":2,"rows":[{"uuid":"` + a.String() + `","callstate":"RINGING"},{"uuid":"` + b.String() + `","cal`, 1},
		"cut before any rows": {`{"row_count":2,"ro`, 0},
		"unknown extra key":   {`{"extra":{"a":1},"rows":[{"uuid":"` + a.String() + `","callstate":"HELD"}]}`, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rows, _, _ := parseChannelList(tc.payload, quietLog())
			if got := len(rows); got != tc.want {
				t.Errorf("rows = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestADumpOfAVanishedChannelIsErrChannelGone(t *testing.T) {
	t.Parallel()
	a := NewAdapter(&fakeCommander{up: true, reply: "-ERR No such channel!\n"}, "aicc.test")
	if _, err := a.DumpChannel("x"); !errors.Is(err, ErrChannelGone) {
		t.Errorf("err = %v, want ErrChannelGone", err)
	}
	a = NewAdapter(&fakeCommander{up: true, reply: "-ERR something else"}, "aicc.test")
	if _, err := a.DumpChannel("x"); err == nil || errors.Is(err, ErrChannelGone) {
		t.Errorf("err = %v, want a plain failure", err)
	}
}

func TestTheListingFailingOutrightIsAnError(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nullPublisher{})
	t.Cleanup(reg.Shutdown)
	c := NewCoordinator(reg, NewAdapter(&fakeCommander{up: true, reply: "-ERR no\n"}, "d"), noAgents{}, nullPublisher{})
	if _, err := c.AdoptLiveChannels(t.Context()); err == nil {
		t.Error("an unreadable switch reported success, which would open the gate")
	}
}

// An originated leg is listed in callstate DOWN until the far end first sends
// progress. A leg of ours in that state must be read like any other, or a
// reconnect or restart in the first second of a dial forgets it.
func TestAYoungLegListedAsDownIsStillRead(t *testing.T) {
	t.Parallel()
	id, callID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7())
	dump := `{"Unique-ID":"` + id + `","Call-Direction":"outbound","Answer-State":"ringing",` +
		`"variable_aicc_call_id":"` + callID.String() + `","variable_aicc_placed_by":"DIAL_AI"}`
	cmd := &fakeCommander{up: true, replyFor: func(c string) (string, error) {
		switch {
		case c == "show channels as json":
			return `{"row_count":1,"rows":[{"uuid":"` + id + `","callstate":"DOWN","created_epoch":"1760000000"}]}`, nil
		case strings.HasPrefix(c, "uuid_dump "):
			return dump, nil
		}
		return "", nil
	}}
	reg := NewRegistry(nullPublisher{})
	t.Cleanup(reg.Shutdown)
	c := NewCoordinator(reg, NewAdapter(cmd, "aicc.test"), noAgents{}, nullPublisher{})

	live, err := c.AdoptLiveChannels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].CallID != callID || live[0].PlacedBy != PlacedByDialAI {
		t.Fatalf("live = %+v, want the DOWN leg read and reported", live)
	}
}

// A handed-over AI outbound call is adopted from a dump whose destination and
// direction no longer tell the truth: the transfer rewrote
// Caller-Destination-Number to the queue's extension, and a leg parked in the
// queue can read inbound. The leg still says who placed it and whom it was
// dialled at, and the adoption must take both from there — or the call is
// written with the extension as the customer, or flipped to INBOUND (#533).
func TestAnAdoptedPlacedLegKeepsItsNumberAndTypeAfterHandover(t *testing.T) {
	t.Parallel()

	dumpOf := func(id, callID, direction, context string) string {
		return `{"Unique-ID":"` + id + `","Call-Direction":"` + direction + `",` +
			`"Caller-Context":"` + context + `","Answer-State":"answered",` +
			`"Caller-Destination-Number":"7000",` +
			`"variable_aicc_call_id":"` + callID + `",` +
			`"variable_aicc_did":"95099",` +
			`"variable_aicc_placed_by":"DIAL_AI",` +
			`"variable_aicc_placed_to":"13800000014"}`
	}
	for name, tc := range map[string]struct {
		direction string
		context   string
	}{
		"talking at the restart": {direction: "outbound", context: "aicc_queue"},
		"queued at the restart":  {direction: "inbound", context: "public"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, callID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7())
			dump := dumpOf(id, callID.String(), tc.direction, tc.context)
			cmd := &fakeCommander{up: true, replyFor: func(c string) (string, error) {
				switch {
				case c == "show channels as json":
					return `{"row_count":1,"rows":[{"uuid":"` + id +
						`","callstate":"ACTIVE","created_epoch":"1760000000"}]}`, nil
				case strings.HasPrefix(c, "uuid_dump "):
					return dump, nil
				}
				return "", nil
			}}
			reg := NewRegistry(nullPublisher{})
			t.Cleanup(reg.Shutdown)
			c := NewCoordinator(reg, NewAdapter(cmd, "aicc.test"), noAgents{}, nullPublisher{})

			live, err := c.AdoptLiveChannels(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(live) != 1 || live[0].CallID != callID {
				t.Fatalf("live = %+v, want the adopted leg", live)
			}

			snap, err := reg.Snapshot(callID)
			if err != nil {
				t.Fatal(err)
			}
			if snap.CallType != events.CallTypeOutbound {
				t.Errorf("callType = %s, want OUTBOUND — the dump's %s direction "+
					"is the transfer's, not the call's", snap.CallType, tc.direction)
			}
			var originator *PartySnapshot
			for i := range snap.Parties {
				if snap.Parties[i].Role == RoleOriginator {
					originator = &snap.Parties[i]
				}
			}
			if originator == nil {
				t.Fatal("the adopted leg is nobody's originator")
			}
			if originator.Number != "13800000014" {
				t.Errorf("customer number = %q, want the dialled number — 7000 is "+
					"the queue the leg was transferred to", originator.Number)
			}
		})
	}
}
