// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type fakeCommander struct {
	sent  []string
	reply string
	// replyFor answers per command, for the callers that ask the switch more
	// than one question in a row. An empty result falls through to reply.
	replyFor func(cmd string) (string, error)
	err      error
	up       bool
}

func (f *fakeCommander) API(cmd string) (string, error) {
	f.sent = append(f.sent, cmd)
	if f.replyFor != nil {
		if out, err := f.replyFor(cmd); out != "" || err != nil {
			return out, err
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if f.reply == "" {
		return "+OK", nil
	}
	return f.reply, nil
}

func (f *fakeCommander) BgAPI(cmd string) (string, error) {
	f.sent = append(f.sent, cmd)
	if f.err != nil {
		return "", f.err
	}
	return "job-1", nil
}

func (f *fakeCommander) IsUp() bool { return f.up }

func (f *fakeCommander) last() string {
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

func newTestAdapter() (*Adapter, *fakeCommander) {
	c := &fakeCommander{up: true}
	return NewAdapter(c, "aicc.test"), c
}

// The exact command strings are the contract with FreeSWITCH: several of them
// encode failures that are silent at runtime, so they are asserted literally.
func TestCommandStrings(t *testing.T) {
	partyID := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")

	tests := []struct {
		name string
		act  func(a *Adapter) error
		want string
	}{
		{
			name: "queue names are the queue name, with no domain",
			act:  func(a *Adapter) error { return a.AddCallcenterTier("support-en", "agent-1001", 1, 1) },
			want: "callcenter_config tier add support-en agent-1001 1 1",
		},
		{
			name: "agents are added as callback so the switch originates to them",
			act:  func(a *Adapter) error { return a.AddCallcenterAgent("agent-1001") },
			want: "callcenter_config agent add agent-1001 callback",
		},
		{
			name: "contact is a registered endpoint",
			act:  func(a *Adapter) error { return a.SetCallcenterAgentContact("agent-1001", "1001", false) },
			// The ring is bounded on the agent's own dial string: the queue's
			// own setting for it does not work on this module (C41).
			want: "callcenter_config agent set contact agent-1001 '{leg_timeout=15}user/1001@aicc.test'",
		},
		{
			name: "auto answer rides as a channel variable on the contact",
			act:  func(a *Adapter) error { return a.SetCallcenterAgentContact("agent-1001", "1001", true) },
			want: "callcenter_config agent set contact agent-1001 '{leg_timeout=15,sip_auto_answer=true}user/1001@aicc.test'",
		},
		{
			name: "status mirrors our presence",
			act:  func(a *Adapter) error { return a.SetCallcenterAgentStatus("agent-1001", "Available") },
			want: "callcenter_config agent set status agent-1001 'Available'",
		},
		{
			// uuid_answer reports success while a browser phone does nothing;
			// remote control is the only thing that actually rings it.
			name: "answering an agent uses remote phone control",
			act:  func(a *Adapter) error { return a.Answer("chan-1") },
			want: "uuid_phone_event chan-1 talk",
		},
		{
			name: "hold uses remote phone control too",
			act:  func(a *Adapter) error { return a.Hold("chan-1") },
			want: "uuid_phone_event chan-1 hold",
		},
		{
			// An empty context means aicc's own, not the stock one: every
			// extension this switch transfers to is one aicc defines
			// (design 01 §7 D6a).
			name: "transfer to a queue extension goes through aicc's dialplan",
			act:  func(a *Adapter) error { return a.TransferToExtension("chan-1", "7001", "") },
			want: "uuid_transfer chan-1 7001 XML aicc",
		},
		{
			// Never originate plus uuid_bridge: bridging needs media up on one
			// leg, which a caller hearing ringback does not have, and the
			// failure is silent.
			name: "bridging is a transfer into an inline bridge",
			act: func(a *Adapter) error {
				return a.BridgeToEndpoint("chan-1", partyID, "user/1001@aicc.test", nil)
			},
			want: "uuid_transfer chan-1 'm:^:bridge:{origination_uuid=019ffa1d-0dc1-7b9e-b124-cffb41e90a3d}user/1001@aicc.test' inline",
		},
		{
			// The m:^: delimiter prefix is what lets a comma-joined variable
			// list survive the inline parser: with the default delimiter the
			// action list splits inside {…} and bridge receives a truncated
			// argument (found live).
			name: "bridge variables are sorted so the command is reproducible",
			act: func(a *Adapter) error {
				return a.BridgeToEndpoint("chan-1", partyID, "sofia/gateway/aicc_bot/95011",
					map[string]string{"sip_h_X-AICC-Call-ID": "abc", "absolute_codec_string": "PCMU"})
			},
			want: "uuid_transfer chan-1 'm:^:bridge:{absolute_codec_string=PCMU,origination_uuid=019ffa1d-0dc1-7b9e-b124-cffb41e90a3d,sip_h_X-AICC-Call-ID=abc}sofia/gateway/aicc_bot/95011' inline",
		},
		{
			name: "hangup defaults to a normal cause",
			act:  func(a *Adapter) error { return a.Hangup("chan-1", "") },
			want: "uuid_kill chan-1 NORMAL_CLEARING",
		},
		{
			name: "queue reload names the queue the switch knows",
			act:  func(a *Adapter) error { return a.ReloadQueue("support-en") },
			want: "callcenter_config queue reload support-en",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, c := newTestAdapter()
			if err := tt.act(a); err != nil {
				t.Fatalf("command error = %v", err)
			}
			if got := c.last(); got != tt.want {
				t.Errorf("command =\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}

func TestOriginateParksTheNewLeg(t *testing.T) {
	a, c := newTestAdapter()
	partyID := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")

	if _, err := a.Originate(partyID, "sofia/gateway/trunk/8613800138000",
		map[string]string{"origination_caller_id_number": "95011"}); err != nil {
		t.Fatalf("Originate() error = %v", err)
	}

	want := "originate {ignore_early_media=true,origination_caller_id_number=95011," +
		"origination_uuid=019ffa1d-0dc1-7b9e-b124-cffb41e90a3d}" +
		"sofia/gateway/trunk/8613800138000 &park()"
	if got := c.last(); got != want {
		t.Errorf("command =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEavesdropModes(t *testing.T) {
	partyID := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")

	tests := []struct {
		mode     string
		contains string
	}{
		{"LISTEN", "&eavesdrop(chan-target)"},
		{"WHISPER", "eavesdrop_whisper_bleg=true"},
		{"BARGE", "eavesdrop_bridge_aleg=true"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			a, c := newTestAdapter()
			if _, err := a.Eavesdrop(partyID, "1099", "chan-target", tt.mode); err != nil {
				t.Fatalf("Eavesdrop() error = %v", err)
			}
			if !strings.Contains(c.last(), tt.contains) {
				t.Errorf("command %q does not contain %q", c.last(), tt.contains)
			}
			if !strings.Contains(c.last(), "sip_auto_answer=true") {
				t.Error("a supervisor's own phone must answer without them picking up")
			}
		})
	}
}

func TestSwitchErrorRepliesBecomeErrors(t *testing.T) {
	a, c := newTestAdapter()
	c.reply = "-ERR No such channel!"

	err := a.Hangup("chan-gone", "NORMAL_CLEARING")
	if err == nil {
		t.Fatal("a -ERR reply was reported as success")
	}
	if !strings.Contains(err.Error(), "No such channel") {
		t.Errorf("error = %v, want the switch's own message", err)
	}
}

func TestTransportErrorsPropagate(t *testing.T) {
	a, c := newTestAdapter()
	c.err = errors.New("esl link down")

	if err := a.SetCallcenterAgentStatus("agent-1001", "Available"); err == nil {
		t.Fatal("a transport failure was reported as success")
	}
}

// renderVars joins values bare: quoting is not an option here, because the
// same block rides both raw originate lines and the single-quoted inline
// transfer, where an embedded quote hands the leftovers to the inline parser
// as an application ("Invalid Application 1007", found live). The contract is
// that callers supply token-clean values.
func TestRenderVarsJoinsBare(t *testing.T) {
	got := renderVars(map[string]string{"b": "2", "a": "1"})
	if got != "a=1,b=2" {
		t.Fatalf("renderVars = %q, want %q", got, "a=1,b=2")
	}
}
