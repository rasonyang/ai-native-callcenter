// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// AICC_TIME_ZONE is one IANA name for the whole deployment, checked at
// startup: a name the zone database does not know refuses to start rather
// than dating every exported call in UTC behind the operator's back.
func TestTimeZone(t *testing.T) {
	tests := []struct {
		name    string
		value   *string
		want    string
		wantErr bool
	}{
		{name: "unset is UTC", want: "UTC"},
		{name: "empty is unset", value: ptrTo(""), want: "UTC"},
		{name: "a deployment in China", value: ptrTo("Asia/Shanghai"), want: "Asia/Shanghai"},
		{name: "a zone with daylight saving", value: ptrTo("America/Los_Angeles"), want: "America/Los_Angeles"},
		{name: "UTC said out loud", value: ptrTo("UTC"), want: "UTC"},
		{name: "an unknown name is refused", value: ptrTo("Mars/Olympus"), wantErr: true},
		{name: "an offset is not a name", value: ptrTo("+08:00"), wantErr: true},
		{name: "Local is the host's zone, not a setting", value: ptrTo("Local"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_ENV", "dev")
			if tt.value == nil {
				t.Setenv("AICC_TIME_ZONE", "")
				os.Unsetenv("AICC_TIME_ZONE")
			} else {
				t.Setenv("AICC_TIME_ZONE", *tt.value)
			}
			c, err := Load()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "AICC_TIME_ZONE") {
					t.Fatalf("Load() error = %v, want one naming the setting", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := c.Location().String(); got != tt.want {
				t.Errorf("Location() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The zone is resolved, not just named: Shanghai is eight hours ahead of UTC
// all year, so the last seconds of a UTC afternoon are already the next day
// there.
func TestTheTimeZoneDatesACall(t *testing.T) {
	t.Setenv("AICC_ENV", "dev")
	t.Setenv("AICC_TIME_ZONE", "Asia/Shanghai")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	at := time.Date(2026, time.November, 30, 16, 0, 5, 0, time.UTC)
	if got := at.In(c.Location()).Format("2006-01-02 15:04:05"); got != "2026-12-01 00:00:05" {
		t.Errorf("16:00:05 UTC in Asia/Shanghai = %s, want 2026-12-01 00:00:05", got)
	}
}

// A Config built by hand, as the handler tests build theirs, has no zone and
// reads as UTC rather than panicking on a nil location.
func TestAConfigWithNoZoneIsUTC(t *testing.T) {
	t.Parallel()
	if got := (Config{}).Location(); got != time.UTC {
		t.Errorf("Location() = %v, want UTC", got)
	}
}
