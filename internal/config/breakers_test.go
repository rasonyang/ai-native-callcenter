// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestCampaignBreakerSettings(t *testing.T) {
	tests := []struct {
		name, trunk, provider string
		wantTrunk, wantProv   int
		wantErr               string
	}{
		{name: "defaults", wantTrunk: 20, wantProv: 5},
		{name: "set", trunk: "3", provider: "2", wantTrunk: 3, wantProv: 2},
		{name: "the largest", trunk: "1000", provider: "1000", wantTrunk: 1000, wantProv: 1000},
		{name: "trunk zero", trunk: "0", wantErr: "AICC_TRUNK_BREAKER_COUNT"},
		{name: "trunk too large", trunk: "1001", wantErr: "AICC_TRUNK_BREAKER_COUNT"},
		{name: "provider negative", provider: "-1", wantErr: "AICC_PROVIDER_BREAKER_COUNT"},
		{name: "provider zero", provider: "0", wantErr: "AICC_PROVIDER_BREAKER_COUNT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_TRUNK_BREAKER_COUNT", tt.trunk)
			t.Setenv("AICC_PROVIDER_BREAKER_COUNT", tt.provider)
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
			if c.TrunkBreakerCount != tt.wantTrunk || c.ProviderBreakerCount != tt.wantProv {
				t.Errorf("breakers = %d/%d, want %d/%d", c.TrunkBreakerCount, c.ProviderBreakerCount, tt.wantTrunk, tt.wantProv)
			}
		})
	}
}
