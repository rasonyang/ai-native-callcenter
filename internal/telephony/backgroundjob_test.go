// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

func TestParseJobResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body   string
		wantOK bool
		cause  string
	}{
		{"+OK 8f1c2b3a-0000-4000-8000-000000000001\n", true, ""},
		{"+OK", true, ""},
		{"-ERR USER_BUSY\n", false, "USER_BUSY"},
		{"-ERR USER_BUSY", false, "USER_BUSY"},
		{"  -ERR   GATEWAY_DOWN  \n\n", false, "GATEWAY_DOWN"},
		{"-ERR\tNO_ANSWER\r\n", false, "NO_ANSWER"},
		{"-ERR NORMAL_TEMPORARY_FAILURE (details follow)\nsecond line", false, "NORMAL_TEMPORARY_FAILURE"},
		{"-ERR Invalid Gateway\n", false, "Invalid Gateway"},
		{"-ERR\n", false, UnspecifiedJobCause},
		{"-ERR   \n", false, UnspecifiedJobCause},
		{"", false, ""},
		{"\n", false, ""},
		{"-USAGE: originate <call url>", false, ""},
		{"-ERRATIC", false, ""},
	}
	for _, c := range cases {
		gotOK, gotCause := ParseJobResult(c.body)
		if gotOK != c.wantOK || gotCause != c.cause {
			t.Errorf("ParseJobResult(%q) = %v, %q; want %v, %q", c.body, gotOK, gotCause, c.wantOK, c.cause)
		}
	}
}

// An originate's result names the channel it was asked to create, so a
// placement whose bgapi reply timed out can still be matched to it (#221).
func TestAnOriginateJobNamesTheChannelItWasAskedFor(t *testing.T) {
	t.Parallel()
	leg := uuid.Must(uuid.NewV7())
	a, cmd := newTestAdapter()
	if _, err := a.Originate(leg, "sofia/gateway/pstn/18688886660", map[string]string{
		"aicc_call_id": uuid.NewString(), "origination_caller_id_name": "Dial 1, 2",
	}); err != nil {
		t.Fatal(err)
	}
	arg := strings.TrimPrefix(cmd.last(), "originate ")
	cases := []struct {
		name, command, arg, want string
	}{
		{"as the adapter writes it", "originate", arg, leg.String()},
		{"not an originate", "uuid_transfer", arg, ""},
		{"no block", "originate", "sofia/gateway/pstn/1 &park()", ""},
		{"no uuid in the block", "originate", "{ignore_early_media=true}sofia/x &park()", ""},
		{"a malformed uuid", "originate", "{origination_uuid=not-a-uuid}sofia/x &park()", ""},
		{"an unterminated block", "originate", "{origination_uuid=" + leg.String(), ""},
	}
	for _, c := range cases {
		ev := esl.NewEvent(map[string]string{
			"Event-Name": "BACKGROUND_JOB", "Job-UUID": "job-1",
			"Job-Command": c.command, "Job-Command-Arg": c.arg,
		}, "-ERR GATEWAY_DOWN\n")
		got, ok := Normalize(ev)
		if !ok {
			t.Fatalf("%s: not normalised", c.name)
		}
		if got.JobChannelID != c.want {
			t.Errorf("%s: JobChannelID = %q, want %q", c.name, got.JobChannelID, c.want)
		}
	}
}

// A command the switch read but did not answer in time reaches the caller as
// esl.ErrTimeout, which is how the outbound path tells an unconfirmed
// placement from a refused one (#221).
func TestAnOriginateThatTimesOutSaysSo(t *testing.T) {
	t.Parallel()
	a, cmd := newTestAdapter()
	cmd.err = fmt.Errorf("%w: bgapi originate …", esl.ErrTimeout)
	if _, err := a.Originate(uuid.Must(uuid.NewV7()), "sofia/gateway/pstn/1", nil); !errors.Is(err, esl.ErrTimeout) {
		t.Fatalf("Originate error = %v, want esl.ErrTimeout", err)
	}
}

func TestBackgroundJobIsNormalisedAndSubscribed(t *testing.T) {
	t.Parallel()
	if !slices.Contains(Subscriptions, "BACKGROUND_JOB") {
		t.Fatal("BACKGROUND_JOB is understood by Normalize and never subscribed to")
	}
	ev := esl.NewEvent(map[string]string{
		"Event-Name": "BACKGROUND_JOB",
		"Job-UUID":   "abc-123",
		"Unique-ID":  "must-not-become-a-channel",
	}, "-ERR GATEWAY_DOWN\n")
	got, ok := Normalize(ev)
	if !ok || got.Kind != KindBackgroundJob {
		t.Fatalf("Normalize = %+v, %v", got, ok)
	}
	if got.JobID != "abc-123" || got.IsJobOK || got.JobCause != "GATEWAY_DOWN" || got.ChannelID != "" {
		t.Errorf("unexpected event: %+v", got)
	}

	if _, ok := Normalize(esl.NewEvent(map[string]string{"Event-Name": "BACKGROUND_JOB"}, "+OK x")); ok {
		t.Error("a job result with no Job-UUID cannot be matched to anything")
	}
}
