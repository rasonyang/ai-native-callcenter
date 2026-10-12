// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestOutboundHoursSetting(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        string
		wantErr     bool
	}{
		{name: "empty is unset", value: "", want: "08:00-21:00"},
		{name: "a value", value: "09:30-20:00", want: "09:30-20:00"},
		{name: "across midnight", value: "20:00-04:00", want: "20:00-04:00"},
		{name: "empty range is refused", value: "08:00-08:00", wantErr: true},
		{name: "a malformed value is refused, not read as the default", value: "8-21", wantErr: true},
		{name: "a non-time is refused", value: "morning", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_OUTBOUND_HOURS", tt.value)
			c, err := Load()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "AICC_OUTBOUND_HOURS") {
					t.Fatalf("Load() error = %v, want one naming the setting", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if c.OutboundHours.String() != tt.want {
				t.Errorf("OutboundHours = %s, want %s", c.OutboundHours, tt.want)
			}
		})
	}
}
