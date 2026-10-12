// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTheListingIsReadWhateverShapeTheSwitchGivesIt(t *testing.T) {
	t.Parallel()
	a, b := uuid.New(), uuid.New()
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
	id, callID := uuid.New().String(), uuid.New()
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
