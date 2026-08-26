// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"slices"
	"testing"

	"github.com/google/uuid"

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

	// A registration that simply lapses is the same outcome and the commoner
	// one: a phone says goodbye by sending REGISTER with Expires: 0, and a
	// browser tab that is killed says nothing at all. Unsubscribed, the
	// registration axis went on believing that phone was there and only the
	// OPTIONS ping ever caught up — on the other axis, under another name.
	// The subclass carries from-user rather than username.
	got, ok = Normalize(event(map[string]string{
		"Event-Name":     "CUSTOM",
		"Event-Subclass": "sofia::expire",
		"from-user":      "1008",
	}))
	if !ok || got.Kind != KindDeviceUnregistered || got.Registered {
		t.Errorf("expire mapped to %+v, want an unregistration", got)
	}
	if got.Extension != "1008" {
		t.Errorf("Extension = %q, want the phone whose registration lapsed", got.Extension)
	}
	if !slices.Contains(Subscriptions, "sofia::expire") {
		t.Error("sofia::expire is normalized but never subscribed to, so it can never arrive")
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
				"CC-Member-CID-Number":   "13800138000",
				"CC-Member-CID-Name":     "Wei",
			},
			want: KindQueueMemberJoined,
			check: func(t *testing.T, got SwitchEvent) {
				// The queue's own copy of the caller: a member the call
				// registry has never seen has no other source of a number.
				if got.ANI != "13800138000" || got.CallerIDName != "Wei" {
					t.Errorf("caller = %q/%q, want the member CID the queue carries",
						got.ANI, got.CallerIDName)
				}
				// The switch domain is upstream vocabulary; it stops at the
				// boundary so the rest of the system matches queues by name.
				if got.Queue != "support" {
					t.Errorf("Queue = %q, want the domain stripped", got.Queue)
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

// The media tap's own account of itself. Until these were subscribed to, the
// module was something this application commanded and never heard from: a
// stream that never connected, one the far end dropped, and one that errored
// were indistinguishable from a working one — which is how a whole call's
// audio went nowhere with nothing anywhere reporting a fault.
func TestTheMediaTapsOwnEventsAreUnderstood(t *testing.T) {
	for _, tc := range []struct {
		subclass string
		want     SwitchEventKind
	}{
		{"mod_audio_stream::connect", KindAudioStreamConnected},
		{"mod_audio_stream::disconnect", KindAudioStreamDisconnected},
		{"mod_audio_stream::error", KindAudioStreamError},
	} {
		t.Run(tc.subclass, func(t *testing.T) {
			ev, ok := Normalize(event(map[string]string{
				"Event-Name":     "CUSTOM",
				"Event-Subclass": tc.subclass,
				"Unique-ID":      "chan-a",
				"Error":          "connection refused",
			}))
			if !ok {
				t.Fatalf("%s was not understood", tc.subclass)
			}
			if ev.Kind != tc.want {
				t.Errorf("kind = %s, want %s", ev.Kind, tc.want)
			}
			if ev.ChannelID != "chan-a" {
				t.Errorf("channelId = %q, want the tapped channel", ev.ChannelID)
			}
			if tc.want == KindAudioStreamError && ev.Cause != "connection refused" {
				t.Errorf("cause = %q, want the module's complaint", ev.Cause)
			}
		})
	}
}

// Where the module's complaint actually is. The reader took it from a header
// while its own comment said the module puts it in the body, so every tap
// failure was reported as error="" — the one field an operator would act on,
// empty on every occurrence (found live 2026-08-26, three click-to-dial calls).
func TestTheMediaTapsComplaintIsReadFromWhereTheModulePutsIt(t *testing.T) {
	t.Run("the body, which is what the module sends", func(t *testing.T) {
		ev, ok := Normalize(esl.NewEvent(map[string]string{
			"Event-Name":     "CUSTOM",
			"Event-Subclass": "mod_audio_stream::error",
			"Unique-ID":      "chan-a",
		}, "  websocket connect failed\n"))
		if !ok {
			t.Fatal("the error event was not understood")
		}
		if ev.Cause != "websocket connect failed" {
			t.Errorf("cause = %q, want the module's complaint from the body", ev.Cause)
		}
	})

	// A header still wins where one exists: a future subclass may grow one,
	// and the body is the fallback rather than the replacement.
	t.Run("a header outranks the body", func(t *testing.T) {
		ev, _ := Normalize(esl.NewEvent(map[string]string{
			"Event-Name":     "CUSTOM",
			"Event-Subclass": "mod_audio_stream::error",
			"Unique-ID":      "chan-a",
			"Error":          "connection refused",
		}, "something else"))
		if ev.Cause != "connection refused" {
			t.Errorf("cause = %q, want the header", ev.Cause)
		}
	})
}

// Subscribing is half of it: an event we understand but never asked for never
// arrives.
func TestTheMediaTapsEventsAreSubscribedTo(t *testing.T) {
	for _, want := range []string{
		"mod_audio_stream::connect", "mod_audio_stream::disconnect",
		"mod_audio_stream::error",
	} {
		if !slices.Contains(Subscriptions, want) {
			t.Errorf("%s is understood by Normalize and never subscribed to", want)
		}
	}
}

// A stamped call type outranks the channel's direction: the switch sees every
// leg we originate as outbound, whether it rings an extension or a carrier.
func TestCallTypeHintOutranksDirection(t *testing.T) {
	ev := SwitchEvent{Direction: DirectionOutbound, CallTypeHint: "INTERNAL"}
	if got := callTypeOf(ev); string(got) != "INTERNAL" {
		t.Errorf("callTypeOf = %q, want INTERNAL", got)
	}
	// Nonsense in the variable is ignored rather than trusted.
	ev.CallTypeHint = "BANANA"
	if got := callTypeOf(ev); string(got) != "OUTBOUND" {
		t.Errorf("callTypeOf with a bad hint = %q, want OUTBOUND", got)
	}
}

// A queue delivery leg carries the waiting caller's channel, which is the only
// thing tying the two together before they bridge. The member's own leg
// carries its own id in that variable and must not be read as a delivery.
func TestNormalizeReadsTheQueueDeliveryStamp(t *testing.T) {
	const (
		member = "019ff973-6b32-7557-b7c4-11b3fdb692f0"
		agent  = "019ff974-1a01-7000-9c3d-2b8e5f0a1c44"
	)

	delivery, ok := Normalize(event(map[string]string{
		"Event-Name":                      "CHANNEL_CREATE",
		"Unique-ID":                       agent,
		"Channel-Name":                    "sofia/internal/1008@192.168.31.55",
		"Call-Direction":                  "outbound",
		"Caller-Destination-Number":       "1008",
		"variable_cc_side":                "agent",
		"variable_cc_member_session_uuid": member,
		"variable_cc_queue":               "support-en",
	}))
	if !ok {
		t.Fatal("Normalize rejected a delivery leg's CHANNEL_CREATE")
	}
	if delivery.MemberChannelID != member {
		t.Errorf("MemberChannelID = %q, want the caller's channel %q", delivery.MemberChannelID, member)
	}

	caller, ok := Normalize(event(map[string]string{
		"Event-Name":                      "CHANNEL_CREATE",
		"Unique-ID":                       member,
		"Channel-Name":                    "sofia/external/18688886669@192.168.31.5",
		"Call-Direction":                  "inbound",
		"variable_cc_side":                "member",
		"variable_cc_member_session_uuid": member,
	}))
	if !ok {
		t.Fatal("Normalize rejected the member's CHANNEL_CREATE")
	}
	if caller.MemberChannelID != "" {
		t.Errorf("MemberChannelID = %q on the member's own leg, want empty", caller.MemberChannelID)
	}
}

// The dialplan exports the DID to the leg dialled towards the bot, and that
// leg hangs up the moment a transfer moves the caller on — long before the
// caller's own leg delivers what the bot actually did. Taking the first
// non-empty share whole let the DID-only half shut the rest out.
func TestBotShareMergesAcrossLegs(t *testing.T) {
	flow := uuid.New()

	// What the bot leg carries when it hangs up at the transfer.
	got := BotShare{DID: "95001"}
	// What the caller's leg carries a conversation later.
	got.Merge(BotShare{
		Sec: 42, FlowID: &flow, DID: "95001",
		Summary: "billing question", Reason: "AGENT_REQUESTED",
	})

	if got.Sec != 42 {
		t.Errorf("Sec = %d, want 42 — the bot's tally arrives on the caller's leg", got.Sec)
	}
	if got.FlowID == nil || *got.FlowID != flow {
		t.Errorf("FlowID = %v, want %v", got.FlowID, flow)
	}
	if got.Summary != "billing question" || got.Reason != "AGENT_REQUESTED" {
		t.Errorf("summary/reason = %q/%q, want them carried over", got.Summary, got.Reason)
	}
	if got.DID != "95001" {
		t.Errorf("DID = %q, want the value already held", got.DID)
	}

	// What is already known is never overwritten by a later, emptier leg.
	got.Merge(BotShare{})
	if got.Sec != 42 || got.FlowID == nil || got.DID != "95001" {
		t.Errorf("an empty share erased what was known: %+v", got)
	}
}

// mod_callcenter announces the bridge on the leg it dialled to reach the
// agent, not on the caller's. The facts it carries are the caller's, so the
// event has to reach the caller's call.
func TestQueueEventsRouteToTheWaitingCaller(t *testing.T) {
	const (
		member = "019ff973-6b32-7557-b7c4-11b3fdb692f0"
		agent  = "019ff974-1a01-7000-9c3d-2b8e5f0a1c44"
	)
	for _, action := range []string{
		"member-queue-start", "member-queue-end", "agent-offering",
		"bridge-agent-start", "bridge-agent-end",
	} {
		got, ok := Normalize(event(map[string]string{
			"Event-Name":             "CUSTOM",
			"Event-Subclass":         "callcenter::info",
			"Unique-ID":              agent,
			"CC-Action":              action,
			"CC-Queue":               "support-en@192.168.31.55",
			"CC-Agent":               "agent-wei",
			"CC-Member-Session-UUID": member,
			"CC-Member-CID-Number":   "18688886669",
		}))
		if !ok {
			t.Fatalf("%s: Normalize rejected the event", action)
		}
		if got.ChannelID != member {
			t.Errorf("%s: ChannelID = %q, want the waiting caller's channel %q",
				action, got.ChannelID, member)
		}
	}

	// An agent's own state change is not about any one caller and keeps the
	// channel the switch raised it on.
	got, ok := Normalize(event(map[string]string{
		"Event-Name":     "CUSTOM",
		"Event-Subclass": "callcenter::info",
		"Unique-ID":      agent,
		"CC-Action":      "agent-state-change",
		"CC-Agent":       "agent-wei",
		"CC-Agent-State": "Waiting",
	}))
	if !ok {
		t.Fatal("Normalize rejected an agent state change")
	}
	if got.ChannelID != agent {
		t.Errorf("agent-state-change ChannelID = %q, want %q", got.ChannelID, agent)
	}
}

// The switch counts the answered seconds itself and puts the figure on the
// hangup. Carrying it is what lets the ledger's billing be checked rather than
// only trusted.
func TestNormalizeReadsTheSwitchesOwnBilledSeconds(t *testing.T) {
	got, ok := Normalize(event(map[string]string{
		"Event-Name":            "CHANNEL_HANGUP_COMPLETE",
		"Unique-ID":             "019ff973-6b32-7557-b7c4-11b3fdb692f0",
		"Hangup-Cause":          "NORMAL_CLEARING",
		"variable_billsec":      "104",
		"variable_billmsec":     "103570",
		"variable_answer_stamp": "2026-08-21 15:09:26",
		"variable_duration":     "104",
	}))
	if !ok {
		t.Fatal("Normalize rejected a hangup")
	}
	if got.BilledSec != 104 {
		t.Errorf("BilledSec = %d, want 104", got.BilledSec)
	}

	// A leg that never answered carries no billsec, and nothing is invented.
	quiet, ok := Normalize(event(map[string]string{
		"Event-Name":   "CHANNEL_HANGUP_COMPLETE",
		"Unique-ID":    "019ff974-1a01-7000-9c3d-2b8e5f0a1c44",
		"Hangup-Cause": "NO_ANSWER",
	}))
	if !ok {
		t.Fatal("Normalize rejected an unanswered hangup")
	}
	if quiet.BilledSec != 0 {
		t.Errorf("BilledSec = %d on a leg that never answered, want 0", quiet.BilledSec)
	}
}
