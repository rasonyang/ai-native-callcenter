// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"slices"
	"testing"

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
