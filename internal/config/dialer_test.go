// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

func TestTheDailyDialCapSetting(t *testing.T) {
	tests := []struct {
		name, value string
		want        int
		wantErr     bool
	}{
		{name: "default is three", want: 3},
		{name: "set", value: "5", want: 5},
		{name: "off", value: "0", want: 0},
		{name: "largest", value: "100", want: 100},
		{name: "negative", value: "-1", wantErr: true},
		{name: "too large", value: "101", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY", tt.value)
			c, err := Load()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY must be between 0 (off) and 100") {
					t.Fatalf("err = %v, want one stating the range", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.OutboundMaxDialsPerNumberPerDay != tt.want {
				t.Errorf("got %d, want %d", c.OutboundMaxDialsPerNumberPerDay, tt.want)
			}
		})
	}
}

func TestDialerSettings(t *testing.T) {
	tests := []struct {
		name, ringing, freq string
		wantRinging         int
		wantFreq            time.Duration
		wantErr             string
	}{
		{name: "defaults", wantRinging: 100, wantFreq: 24 * time.Hour},
		{name: "set", ringing: "7", freq: "6h", wantRinging: 7, wantFreq: 6 * time.Hour},
		{name: "frequency off", freq: "0", wantRinging: 100, wantFreq: 0},
		{name: "ringing zero", ringing: "0", wantErr: "AICC_OUTBOUND_MAX_RINGING"},
		{name: "ringing too large", ringing: "100001", wantErr: "AICC_OUTBOUND_MAX_RINGING"},
		{name: "frequency negative", freq: "-1h", wantErr: "AICC_CAMPAIGN_FREQUENCY_WINDOW"},
		{name: "frequency too long", freq: "721h", wantErr: "AICC_CAMPAIGN_FREQUENCY_WINDOW"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_OUTBOUND_MAX_RINGING", tt.ringing)
			t.Setenv("AICC_CAMPAIGN_FREQUENCY_WINDOW", tt.freq)
			c, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.OutboundMaxRinging != tt.wantRinging || c.CampaignFrequencyWindow != tt.wantFreq {
				t.Errorf("got %d/%s, want %d/%s", c.OutboundMaxRinging, c.CampaignFrequencyWindow, tt.wantRinging, tt.wantFreq)
			}
		})
	}
}
