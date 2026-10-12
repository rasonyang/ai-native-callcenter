// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBotCapacityAndOverflowSettings(t *testing.T) {
	tests := []struct {
		name            string
		maxCalls        string
		reserve         string
		hold            string
		wantReserve     int
		wantHold        time.Duration
		wantErrContains string
	}{
		{name: "defaults", wantHold: 1500 * time.Millisecond},
		{name: "a reserve", maxCalls: "10", reserve: "2", wantReserve: 2, wantHold: 1500 * time.Millisecond},
		{name: "the largest reserve", maxCalls: "10", reserve: "9", wantReserve: 9, wantHold: 1500 * time.Millisecond},
		{name: "reserve equal to the cap", maxCalls: "10", reserve: "10", wantErrContains: "AICC_BOT_INBOUND_RESERVE"},
		{name: "reserve above the cap", maxCalls: "10", reserve: "11", wantErrContains: "AICC_BOT_INBOUND_RESERVE"},
		{name: "negative reserve", reserve: "-1", wantErrContains: "AICC_BOT_INBOUND_RESERVE"},
		{name: "a zero cap", maxCalls: "0", wantErrContains: "AICC_BOT_MAX_CALLS"},
		{name: "hold off", hold: "0", wantHold: 0},
		{name: "hold set", hold: "800ms", wantHold: 800 * time.Millisecond},
		{name: "hold too long", hold: "11s", wantErrContains: "AICC_OVERFLOW_HOLD"},
		{name: "hold negative", hold: "-1s", wantErrContains: "AICC_OVERFLOW_HOLD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, v := range map[string]string{
				"AICC_BOT_MAX_CALLS": tt.maxCalls, "AICC_BOT_INBOUND_RESERVE": tt.reserve, "AICC_OVERFLOW_HOLD": tt.hold,
			} {
				t.Setenv(key, v)
				if v == "" {
					os.Unsetenv(key)
				}
			}
			c, err := Load()
			if tt.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("Load() error = %v, want one naming %s", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if c.BotInboundReserve != tt.wantReserve || c.OverflowHold != tt.wantHold {
				t.Errorf("reserve %d hold %s, want %d and %s",
					c.BotInboundReserve, c.OverflowHold, tt.wantReserve, tt.wantHold)
			}
		})
	}
}
