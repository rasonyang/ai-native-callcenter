// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// The one advertised state comes from two flags: a stored object outranks the
// expectation, an expectation outranks nothing (#538).
func TestRecordingStateDerivesFromTheTwoFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		has, expected bool
		want          string
	}{
		{"object stored", true, true, RecordingStateStored},
		{"stored even without the flag (row older than the column)", true, false, RecordingStateStored},
		{"started, not yet booked", false, true, RecordingStatePending},
		{"never started", false, false, RecordingStateNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordingStateOf(tc.has, tc.expected); got != tc.want {
				t.Errorf("recordingStateOf(%v, %v) = %q, want %q", tc.has, tc.expected, got, tc.want)
			}
		})
	}
}
