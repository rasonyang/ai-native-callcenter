// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// The fixtures below use header names and values captured from the live
// development switch during the M0 spike (docs/design/m0-findings.md), so a
// mapping regression shows up as a test failure rather than in production.

func event(headers map[string]string) *esl.Event { return esl.NewEvent(headers, "") }

func TestNormalizeChannelLifecycle(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    SwitchEventKind
		check   func(t *testing.T, got SwitchEvent)
	}{
		{
			name: "channel create carries identity and direction",
			headers: map[string]string{
				"Event-Name":                "CHANNEL_CREATE",
				"Unique-ID":                 "019ff973-6b32-7557-b7c4-11b3fdb692f0",
				"Channel-Name":              "loopback/9664-a",
				"Call-Direction":            "outbound",
				"Caller-ANI":                "0000000000",
				"Caller-Destination-Number": "9664",
				"Caller-Context":            "default",
				"Event-Date-Timestamp":      "1786596518711944",
			},
			want: KindChannelCreate,
			check: func(t *testing.T, got SwitchEvent) {
				if got.ChannelID != "019ff973-6b32-7557-b7c4-11b3fdb692f0" {
					t.Errorf("ChannelID = %q", got.ChannelID)
				}
				if got.Direction != DirectionOutbound {
					t.Errorf("Direction = %q, want OUTBOUND", got.Direction)
				}
				if got.DestinationNumber != "9664" {
					t.Errorf("DestinationNumber = %q", got.DestinationNumber)
				}
				if got.OccurredAt.IsZero() {
					t.Error("OccurredAt is zero, want the switch timestamp")
				}
			},
		},
		{
			name: "park is recognised",
			headers: map[string]string{
				"Event-Name":                   "CHANNEL_PARK",
				"Unique-ID":                    "019ff973-6b32-7557-b7c4-11b3fdb692f0",
				"Caller-Destination-Number":    "9664",
				"variable_current_application": "park",
			},
			want: KindChannelPark,
		},
		{
			name: "bridge names both legs",
			headers: map[string]string{
				"Event-Name":          "CHANNEL_BRIDGE",
				"Unique-ID":           "019ff973-8afa-7c58-ad10-7cc2ea9faf08",
				"Bridge-A-Unique-ID":  "019ff973-8afa-7c58-ad10-7cc2ea9faf08",
				"Bridge-B-Unique-ID":  "019ff973-8b04-7494-b205-6881d9158350",
				"Other-Leg-Unique-ID": "019ff973-8b04-7494-b205-6881d9158350",
			},
			want: KindChannelBridge,
			check: func(t *testing.T, got SwitchEvent) {
				if got.ChannelID != "019ff973-8afa-7c58-ad10-7cc2ea9faf08" ||
					got.OtherChannelID != "019ff973-8b04-7494-b205-6881d9158350" {
					t.Errorf("bridge legs = %q / %q", got.ChannelID, got.OtherChannelID)
				}
			},
		},
		{
			name: "hangup carries cause and q850",
			headers: map[string]string{
				"Event-Name":                 "CHANNEL_HANGUP_COMPLETE",
				"Unique-ID":                  "019ff973-6b3b-7481-922b-cb52677a6cac",
				"Hangup-Cause":               "NORMAL_CLEARING",
				"variable_hangup_cause":      "NORMAL_CLEARING",
				"variable_hangup_cause_q850": "16",
			},
			want: KindChannelHangup,
			check: func(t *testing.T, got SwitchEvent) {
				if got.HangupCause != "NORMAL_CLEARING" || got.HangupCauseQ850 != 16 {
					t.Errorf("cause = %q q850 = %d", got.HangupCause, got.HangupCauseQ850)
				}
				if got.TransferredAway {
					t.Error("TransferredAway = true for a plain hangup")
				}
			},
		},
		{
			name: "blind transfer is not a lost call",
			headers: map[string]string{
				"Event-Name":                "CHANNEL_HANGUP_COMPLETE",
				"Unique-ID":                 "019ff973-6b32-7557-b7c4-11b3fdb692f0",
				"Hangup-Cause":              "NORMAL_CLEARING",
				"variable_transfer_history": "1786596523:019ff973-7f15:bl_xfer:9196/default/XML",
			},
			want: KindChannelHangup,
			check: func(t *testing.T, got SwitchEvent) {
				if !got.TransferredAway {
					t.Error("TransferredAway = false, want true: the call moved on")
				}
			},
		},
		{
			name: "refer disposition marks a transfer",
			headers: map[string]string{
				"Event-Name":                      "CHANNEL_HANGUP_COMPLETE",
				"Unique-ID":                       "u1",
				"Hangup-Cause":                    "NORMAL_CLEARING",
				"variable_sip_hangup_disposition": "recv_refer",
			},
			want: KindChannelHangup,
			check: func(t *testing.T, got SwitchEvent) {
				if !got.TransferredAway {
					t.Error("TransferredAway = false for recv_refer")
				}
			},
		},
		{
			name: "dtmf duration converts rtp ticks to milliseconds",
			headers: map[string]string{
				"Event-Name":    "DTMF",
				"Unique-ID":     "019ff973-6b3b-7481-922b-cb52677a6cac",
				"DTMF-Digit":    "1",
				"DTMF-Duration": "2000",
			},
			want: KindDTMF,
			check: func(t *testing.T, got SwitchEvent) {
				if got.Digit != "1" {
					t.Errorf("Digit = %q", got.Digit)
				}
				if got.DurationMs != 250 {
					t.Errorf("DurationMs = %d, want 250 (2000 ticks at 8 kHz)", got.DurationMs)
				}
			},
		},
		{
			name: "record start carries the file path",
			headers: map[string]string{
				"Event-Name":       "RECORD_START",
				"Unique-ID":        "019ff973-6b32-7557-b7c4-11b3fdb692f0",
				"Record-File-Path": "/recordings/2026/08/13/call.wav",
			},
			want: KindRecordStart,
			check: func(t *testing.T, got SwitchEvent) {
				if got.RecordingPath != "/recordings/2026/08/13/call.wav" {
					t.Errorf("RecordingPath = %q", got.RecordingPath)
				}
			},
		},
		{
			name:    "unconsumed events are dropped",
			headers: map[string]string{"Event-Name": "HEARTBEAT"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Normalize(event(tt.headers))
			if tt.want == "" {
				if ok {
					t.Fatalf("Normalize() accepted %q, want it dropped", tt.headers["Event-Name"])
				}
				return
			}
			if !ok {
				t.Fatalf("Normalize() dropped %q", tt.headers["Event-Name"])
			}
			if got.Kind != tt.want {
				t.Fatalf("Kind = %q, want %q", got.Kind, tt.want)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestNormalizeSofiaRegistration(t *testing.T) {
	// Captured from a live SIP.js registration through the WSS path.
	got, ok := Normalize(event(map[string]string{
		"Event-Name":     "CUSTOM",
		"Event-Subclass": "sofia::register",
		"profile-name":   "internal",
		"from-user":      "1008",
		"username":       "1008",
		"call-id":        "r0mdqg7o7hlq0vsbkei1",
		"network-ip":     "192.168.31.248",
		"user-agent":     "SIP.js/0.21.2",
		"status":         "Registered(WSS-NAT)",
	}))
	if !ok {
		t.Fatal("Normalize() dropped sofia::register")
	}
	if got.Kind != KindDeviceRegistered {
		t.Errorf("Kind = %q, want DEVICE_REGISTERED", got.Kind)
	}
	if got.Extension != "1008" {
		t.Errorf("Extension = %q, want 1008", got.Extension)
	}
	if !got.Registered {
		t.Error("Registered = false")
	}
	if got.UserAgent != "SIP.js/0.21.2" {
		t.Errorf("UserAgent = %q", got.UserAgent)
	}

	got, ok = Normalize(event(map[string]string{
		"Event-Name":     "CUSTOM",
		"Event-Subclass": "sofia::unregister",
		"username":       "1008",
	}))
	if !ok || got.Kind != KindDeviceUnregistered || got.Registered {
		t.Errorf("unregister mapped to %+v", got)
	}
}

func TestNormalizeDeviceStateFromOptionsPing(t *testing.T) {
	got, ok := Normalize(event(map[string]string{
		"Event-Name":     "CUSTOM",
		"Event-Subclass": "sofia::sip_user_state",
		"user":           "1008",
		"ping-status":    "DOWN",
	}))
	if !ok {
		t.Fatal("Normalize() dropped sip_user_state")
	}
	if got.Kind != KindDeviceState {
		t.Fatalf("Kind = %q", got.Kind)
	}
	if got.Registered {
		t.Error("Registered = true for a failed OPTIONS ping: this is the dead-phone signal")
	}
}

func TestNormalizeCallcenter(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    SwitchEventKind
		check   func(t *testing.T, got SwitchEvent)
	}{
		{
			name: "member joins a queue",
			headers: map[string]string{
				"Event-Name":             "CUSTOM",
				"Event-Subclass":         "callcenter::info",
				"CC-Action":              "member-queue-start",
				"CC-Queue":               "support@default",
				"CC-Member-Session-UUID": "019ff973-abcc-7349-bda2-b58e0e005324",
				"CC-Member-Joined-Time":  "1786596535",
			},
			want: KindQueueMemberJoined,
			check: func(t *testing.T, got SwitchEvent) {
				if got.Queue != "support@default" {
					t.Errorf("Queue = %q", got.Queue)
				}
				if got.ChannelID != "019ff973-abcc-7349-bda2-b58e0e005324" {
					t.Errorf("ChannelID = %q, want the member session uuid", got.ChannelID)
				}
				if got.JoinedAt.IsZero() {
					t.Error("JoinedAt is zero")
				}
			},
		},
		{
			name: "member leaves with a cause",
			headers: map[string]string{
				"Event-Name":             "CUSTOM",
				"Event-Subclass":         "callcenter::info",
				"CC-Action":              "member-queue-end",
				"CC-Queue":               "support@default",
				"CC-Cause":               "Cancel",
				"CC-Cancel-Reason":       "BREAK_OUT",
				"CC-Member-Leaving-Time": "1786596543",
			},
			want: KindQueueMemberLeft,
			check: func(t *testing.T, got SwitchEvent) {
				if got.Cause != "Cancel" || got.CancelReason != "BREAK_OUT" {
					t.Errorf("cause = %q reason = %q", got.Cause, got.CancelReason)
				}
			},
		},
		{
			name: "agent is offered a call",
			headers: map[string]string{
				"Event-Name":             "CUSTOM",
				"Event-Subclass":         "callcenter::info",
				"CC-Action":              "agent-offering",
				"CC-Queue":               "support@default",
				"CC-Agent":               "agent-1000",
				"CC-Member-Session-UUID": "019ff973-abcc-7349-bda2-b58e0e005324",
			},
			want: KindQueueAgentOffered,
			check: func(t *testing.T, got SwitchEvent) {
				if got.AgentName != "agent-1000" {
					t.Errorf("AgentName = %q", got.AgentName)
				}
			},
		},
		{
			name: "bridge failure carries the cause",
			headers: map[string]string{
				"Event-Name":      "CUSTOM",
				"Event-Subclass":  "callcenter::info",
				"CC-Action":       "bridge-agent-fail",
				"CC-Agent":        "agent-1000",
				"CC-Hangup-Cause": "USER_NOT_REGISTERED",
			},
			want: KindQueueBridgeFailed,
			check: func(t *testing.T, got SwitchEvent) {
				if got.HangupCause != "USER_NOT_REGISTERED" {
					t.Errorf("HangupCause = %q", got.HangupCause)
				}
			},
		},
		{
			name: "agent state change",
			headers: map[string]string{
				"Event-Name":     "CUSTOM",
				"Event-Subclass": "callcenter::info",
				"CC-Action":      "agent-state-change",
				"CC-Agent":       "agent-1000",
				"CC-Agent-State": "Receiving",
			},
			want: KindQueueAgentState,
			check: func(t *testing.T, got SwitchEvent) {
				if got.AgentState != "Receiving" {
					t.Errorf("AgentState = %q", got.AgentState)
				}
			},
		},
		{
			name: "waiting count",
			headers: map[string]string{
				"Event-Name":     "CUSTOM",
				"Event-Subclass": "callcenter::info",
				"CC-Action":      "members-count",
				"CC-Queue":       "support@default",
				"CC-Count":       "3",
			},
			want: KindQueueMembersCount,
			check: func(t *testing.T, got SwitchEvent) {
				if got.MemberCount != 3 {
					t.Errorf("MemberCount = %d, want 3", got.MemberCount)
				}
			},
		},
		{
			name: "administrative echoes are dropped",
			headers: map[string]string{
				"Event-Name":     "CUSTOM",
				"Event-Subclass": "callcenter::info",
				"CC-Action":      "agent-add",
				"CC-Agent":       "agent-1000",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Normalize(event(tt.headers))
			if tt.want == "" {
				if ok {
					t.Fatalf("Normalize() accepted %q", tt.headers["CC-Action"])
				}
				return
			}
			if !ok {
				t.Fatalf("Normalize() dropped %q", tt.headers["CC-Action"])
			}
			if got.Kind != tt.want {
				t.Fatalf("Kind = %q, want %q", got.Kind, tt.want)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestNormalizeHandlesNilAndUnknown(t *testing.T) {
	if _, ok := Normalize(nil); ok {
		t.Error("Normalize(nil) reported ok")
	}
	if _, ok := Normalize(event(map[string]string{
		"Event-Name":     "CUSTOM",
		"Event-Subclass": "conference::maintenance",
	})); ok {
		t.Error("unknown custom subclass was accepted")
	}
}
