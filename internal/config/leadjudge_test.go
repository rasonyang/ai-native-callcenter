// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// AICC_LEAD_JUDGE_INTENTS names the declared leads the lead judge reads. Unset
// it is HIGH alone; a list that names anything but HIGH and MEDIUM, names one
// twice or has an empty item refuses startup rather than falling back to the
// default behind the operator's back.
func TestLeadJudgeIntents(t *testing.T) {
	tests := []struct {
		name    string
		value   *string
		want    []string
		wantErr string
	}{
		{name: "unset is HIGH alone", want: []string{"HIGH"}},
		{name: "empty is unset", value: ptrTo(""), want: []string{"HIGH"}},
		{name: "HIGH", value: ptrTo("HIGH"), want: []string{"HIGH"}},
		{name: "MEDIUM alone", value: ptrTo("MEDIUM"), want: []string{"MEDIUM"}},
		{name: "both, as v0.9.2 judged", value: ptrTo("HIGH,MEDIUM"), want: []string{"HIGH", "MEDIUM"}},
		{name: "the order written is kept", value: ptrTo("MEDIUM,HIGH"), want: []string{"MEDIUM", "HIGH"}},
		{name: "whitespace around an item", value: ptrTo(" HIGH , MEDIUM "), want: []string{"HIGH", "MEDIUM"}},
		{name: "a duplicate", value: ptrTo("HIGH,HIGH"), wantErr: "twice"},
		{name: "a duplicate among both", value: ptrTo("HIGH,MEDIUM,HIGH"), wantErr: "twice"},
		{name: "a lead no judge reads", value: ptrTo("HIGH,LOW"), wantErr: `"LOW"`},
		{name: "an unknown value", value: ptrTo("URGENT"), wantErr: `"URGENT"`},
		{name: "lower case", value: ptrTo("high"), wantErr: `"high"`},
		{name: "only a separator", value: ptrTo(","), wantErr: "empty item"},
		{name: "only blanks", value: ptrTo("   "), wantErr: "AICC_LEAD_JUDGE_ENABLED=false"},
		{name: "a trailing comma", value: ptrTo("HIGH,"), wantErr: "empty item"},
		{name: "a leading comma", value: ptrTo(",MEDIUM"), wantErr: "empty item"},
		{name: "a doubled comma", value: ptrTo("HIGH,,MEDIUM"), wantErr: "empty item"},
		{name: "a blank item", value: ptrTo("HIGH, ,MEDIUM"), wantErr: "empty item"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_ENV", "dev")
			if tt.value == nil {
				t.Setenv("AICC_LEAD_JUDGE_INTENTS", "")
				os.Unsetenv("AICC_LEAD_JUDGE_INTENTS")
			} else {
				t.Setenv("AICC_LEAD_JUDGE_INTENTS", *tt.value)
			}
			c, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), "AICC_LEAD_JUDGE_INTENTS") ||
					!strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want one naming the setting and %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !slices.Equal(c.LeadJudgeIntents, tt.want) {
				t.Errorf("LeadJudgeIntents = %q, want %q", c.LeadJudgeIntents, tt.want)
			}
		})
	}
}

// A list that does not parse refuses startup with the judge off too: a typo
// is not to wait for the day the judge is turned on.
func TestABadLeadJudgeListIsRefusedWithTheJudgeOff(t *testing.T) {
	t.Setenv("AICC_LEAD_JUDGE_ENABLED", "false")
	t.Setenv("AICC_LEAD_JUDGE_INTENTS", "HIGH,MEDIUM,MEDIUM")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AICC_LEAD_JUDGE_INTENTS") {
		t.Fatalf("Load() error = %v, want one naming the setting", err)
	}
}

// The default parses, and is one of the accepted values.
func TestTheDefaultLeadJudgeListIsAccepted(t *testing.T) {
	got, err := ParseLeadJudgeIntents(DefaultLeadJudgeIntents)
	if err != nil || !slices.Equal(got, []string{"HIGH"}) {
		t.Fatalf("ParseLeadJudgeIntents(%q) = %q, %v", DefaultLeadJudgeIntents, got, err)
	}
	for _, v := range got {
		if !slices.Contains(LeadJudgeableIntents(), v) {
			t.Errorf("default %q is not an accepted value", v)
		}
	}
}
