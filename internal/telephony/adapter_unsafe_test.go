// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// What the switch reads as syntax somewhere a bridge's variables travel: the
// api arguments, the inline action list, the expansion of the bridge's data,
// the {…} block, the dial string. Each one, in a value, refuses the command.
var unsafeInABridge = []struct{ name, value string }{
	{"a single quote, which ends the inline transfer's quoting", "en'"},
	{"a caret, the inline action separator", "en^hangup"},
	{"a comma, the variable separator", "zh,x=y"},
	{"a space", "zh hans"},
	{"a tab", "zh\thans"},
	{"a line feed", "en\nzh"},
	{"a carriage return", "en\rzh"},
	{"a NUL", "en\x00"},
	{"an escape character", "en\x1b"},
	{"an opening brace", "en{"},
	{"a closing brace", "en}"},
	{"a backslash", `en\`},
	{"a dollar, which expands", "${system(id)}"},
	{"a pipe, the failover separator", "en|x"},
	{"the enterprise originate separator", "en:_:x"},
}

// The language a call carries rides the bridge to the bot twice, as
// aicc_language and as X-AICC-Language. A value the switch would read as
// syntax refuses the bridge, and nothing reaches the switch.
func TestABridgeWithAnUnsafeVariableIsNeverSent(t *testing.T) {
	t.Parallel()
	party := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")
	for _, tt := range unsafeInABridge {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, c := newTestAdapter()
			err := a.BridgeToEndpoint("chan-1", party, "sofia/gateway/aicc_bot/95011", map[string]string{
				"aicc_language":         tt.value,
				"sip_h_X-AICC-Language": tt.value,
				"aicc_did":              "95011",
			})
			if !errors.Is(err, ErrUnsafeValue) {
				t.Fatalf("BridgeToEndpoint error = %v, want ErrUnsafeValue", err)
			}
			if len(c.sent) != 0 {
				t.Errorf("a refused bridge reached the switch: %q", c.sent)
			}
		})
	}
}

// The endpoint, the variable names and the channel are held to the same
// standard: none of them may smuggle in a second endpoint, a second action or
// a second argument.
func TestABridgeRefusesAnUnsafeEndpointNameOrChannel(t *testing.T) {
	t.Parallel()
	party := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")
	tests := []struct {
		name     string
		channel  string
		endpoint string
		vars     map[string]string
	}{
		{name: "a second endpoint", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/95011,loopback/9999"},
		{name: "a failover endpoint", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/95011|loopback/9999"},
		{name: "an expansion in the endpoint", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/${x}"},
		{name: "a quote in the endpoint", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/95011'"},
		{name: "a name with a comma", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/95011",
			vars: map[string]string{"a,execute_on_answer": "x"}},
		{name: "a name with an equals sign", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/95011",
			vars: map[string]string{"a=b": "c"}},
		{name: "an empty name", channel: "chan-1", endpoint: "sofia/gateway/aicc_bot/95011",
			vars: map[string]string{"": "c"}},
		{name: "a channel with a space", channel: "chan-1 7001 XML", endpoint: "sofia/gateway/aicc_bot/95011"},
		{name: "a channel with a quote", channel: "chan-1'", endpoint: "sofia/gateway/aicc_bot/95011"},
		{name: "a channel with a line break", channel: "chan-1\n\nbgapi status", endpoint: "sofia/gateway/aicc_bot/95011"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, c := newTestAdapter()
			err := a.BridgeToEndpoint(tt.channel, party, tt.endpoint, tt.vars)
			if !errors.Is(err, ErrUnsafeValue) {
				t.Fatalf("BridgeToEndpoint error = %v, want ErrUnsafeValue", err)
			}
			if len(c.sent) != 0 {
				t.Errorf("a refused bridge reached the switch: %q", c.sent)
			}
		})
	}
}

// What a real AI outbound bridge carries renders as it always did: the check
// is a refusal, never a rewrite.
func TestABridgeOfOrdinaryValuesIsUnchanged(t *testing.T) {
	t.Parallel()
	party := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")
	a, c := newTestAdapter()
	if err := a.BridgeToEndpoint("chan-1", party, "sofia/gateway/aicc_bot/95011", map[string]string{
		"aicc_language":          "zh-hans",
		"sip_h_X-AICC-Language":  "zh-hans",
		"sip_h_X-AICC-ANI":       "+8613800138000",
		"sip_h_X-AICC-Call-Type": "OUTBOUND",
	}); err != nil {
		t.Fatalf("BridgeToEndpoint error = %v", err)
	}
	const want = "uuid_transfer chan-1 'm:^:bridge:{aicc_language=zh-hans," +
		"origination_uuid=019ffa1d-0dc1-7b9e-b124-cffb41e90a3d,sip_h_X-AICC-ANI=+8613800138000," +
		"sip_h_X-AICC-Call-Type=OUTBOUND,sip_h_X-AICC-Language=zh-hans}sofia/gateway/aicc_bot/95011' inline"
	if c.last() != want {
		t.Errorf("command =\n  %s\nwant\n  %s", c.last(), want)
	}
}

// An originate's block can quote, so what quoting holds is still escaped as
// before. What it cannot hold is refused: a line break ends the command on
// the event socket, and :_: splits the dial string before the block is read.
func TestAnOriginateRefusesWhatQuotingCannotHold(t *testing.T) {
	t.Parallel()
	party := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	for _, tt := range []struct {
		name, endpoint string
		vars           map[string]string
	}{
		{"a line break in a value", "sofia/gateway/pstn/13912345678",
			map[string]string{"aicc_language": "en\n\nbgapi status"}},
		{"a carriage return in a value", "sofia/gateway/pstn/13912345678",
			map[string]string{"aicc_language": "en\r"}},
		{"the enterprise separator in a value", "sofia/gateway/pstn/13912345678",
			map[string]string{"aicc_language": "en:_:loopback/1"}},
		{"a comma in a name", "sofia/gateway/pstn/13912345678",
			map[string]string{"a,b": "c"}},
		{"a second endpoint", "sofia/gateway/pstn/13912345678,loopback/1", nil},
		{"a space in the endpoint", "sofia/gateway/pstn/139 &system(id)", nil},
		{"an expansion in the endpoint", "sofia/gateway/pstn/${x}", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, c := newTestAdapter()
			if _, err := a.Originate(party, tt.endpoint, tt.vars); !errors.Is(err, ErrUnsafeValue) {
				t.Fatalf("Originate error = %v, want ErrUnsafeValue", err)
			}
			if len(c.sent) != 0 {
				t.Errorf("a refused originate reached the switch: %q", c.sent)
			}
		})
	}
}

// A supervisor's monitoring leg is a raw originate built like a bridge: every
// value an identifier, joined bare. One that is not refuses it.
func TestEavesdropRefusesAnUnsafeValue(t *testing.T) {
	t.Parallel()
	party := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")
	callID := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3e")
	for _, tt := range []struct{ name, supervisor, target, mode, agent string }{
		{"an agent extension with a comma", "1099", "chan-target", "LISTEN", "1001,execute_on_answer=x"},
		{"a mode with a space", "1099", "chan-target", "LISTEN NOW", "1001"},
		{"a target that closes the application", "1099", "chan-target) &system(id", "LISTEN", "1001"},
		{"a supervisor extension with a comma", "1099,loopback/1", "chan-target", "LISTEN", "1001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, c := newTestAdapter()
			if _, err := a.Eavesdrop(party, tt.supervisor, tt.target, tt.mode, callID, tt.agent); !errors.Is(err, ErrUnsafeValue) {
				t.Fatalf("Eavesdrop error = %v, want ErrUnsafeValue", err)
			}
			if len(c.sent) != 0 {
				t.Errorf("a refused eavesdrop reached the switch: %q", c.sent)
			}
		})
	}
}

// The extension of a transfer becomes the dialplan's destination number: a
// space would let it name a dialplan of its own, and a $ would be expanded
// where the dialplan uses it.
func TestATransferToAnUnsafeExtensionIsNeverSent(t *testing.T) {
	t.Parallel()
	for _, ext := range []string{
		"system:id inline", "7001 XML default", "7001'", "${system(id)}", "7001,7002", "7001\n", `7001\s`,
	} {
		a, c := newTestAdapter()
		if err := a.TransferToExtension("chan-1", ext, ""); !errors.Is(err, ErrUnsafeValue) {
			t.Errorf("TransferToExtension(%q) error = %v, want ErrUnsafeValue", ext, err)
		}
		if len(c.sent) != 0 {
			t.Errorf("TransferToExtension(%q) reached the switch: %q", ext, c.sent)
		}
	}
	a, c := newTestAdapter()
	if err := a.TransferToExtension("chan-1", "7001", "inline"+" x"); !errors.Is(err, ErrUnsafeValue) {
		t.Errorf("a context with a space was accepted: %v", err)
	}
	if len(c.sent) != 0 {
		t.Errorf("a refused transfer reached the switch: %q", c.sent)
	}
}

// A channel variable's value is the rest of the line, taken whole: spaces,
// commas and quotes are the value's own. A line break is not, because it ends
// the command on the event socket.
func TestAVariableValueIsTakenWholeExceptForALineBreak(t *testing.T) {
	t.Parallel()
	a, c := newTestAdapter()
	if err := a.SetVariable("chan-1", "aicc_bot_summary", "wants a refund, it's the $40 one"); err != nil {
		t.Fatalf("SetVariable error = %v", err)
	}
	if want := "uuid_setvar chan-1 aicc_bot_summary wants a refund, it's the $40 one"; c.last() != want {
		t.Errorf("command = %q, want %q", c.last(), want)
	}

	for _, value := range []string{"wants a refund\n\nbgapi status", "wants a refund\r", "wants\x00"} {
		a, c := newTestAdapter()
		if err := a.SetVariable("chan-1", "aicc_bot_summary", value); !errors.Is(err, ErrUnsafeValue) {
			t.Errorf("SetVariable(%q) error = %v, want ErrUnsafeValue", value, err)
		}
		if len(c.sent) != 0 {
			t.Errorf("SetVariable(%q) reached the switch: %q", value, c.sent)
		}
	}
	a, c = newTestAdapter()
	if err := a.SetVariable("chan-1 other-chan", "x", "y"); !errors.Is(err, ErrUnsafeValue) || len(c.sent) != 0 {
		t.Errorf("a channel with a space was accepted: %v, sent %q", err, c.sent)
	}
}

// Every command string goes through the same check, not only the ones above:
// a bare argument holding a space, a quote, a backslash or a line break would
// be a different command.
func TestEveryBareArgumentIsChecked(t *testing.T) {
	t.Parallel()
	party := uuid.MustParse("019ffa1d-0dc1-7b9e-b124-cffb41e90a3d")
	tests := []struct {
		name string
		act  func(a *Adapter) error
	}{
		{"hangup", func(a *Adapter) error { return a.Hangup("chan-1 x", "") }},
		{"hangup cause", func(a *Adapter) error { return a.Hangup("chan-1", "NORMAL_CLEARING\n") }},
		{"answer", func(a *Adapter) error { return a.Answer("chan-1'") }},
		{"dtmf", func(a *Adapter) error { return a.SendDTMF("chan-1", "1 2") }},
		{"mute", func(a *Adapter) error { return a.MuteLeg(`chan-1\`) }},
		{"agent add", func(a *Adapter) error { return a.AddCallcenterAgent("agent-1 x") }},
		{"agent status", func(a *Adapter) error { return a.SetCallcenterAgentStatus("agent-1", "On Break'") }},
		{"agent contact", func(a *Adapter) error { return a.SetCallcenterAgentContact("agent-1", "1001,loopback/1", false) }},
		{"agent contact expansion", func(a *Adapter) error { return a.SetCallcenterAgentContact("agent-1", "${x}", true) }},
		{"tier add", func(a *Adapter) error { return a.AddCallcenterTier("support en", "agent-1", 1, 1) }},
		{"queue reload", func(a *Adapter) error { return a.ReloadQueue("support\n\napi status") }},
		{"recording", func(a *Adapter) error { return a.StartRecording("chan-1", "/tmp/a b.wav") }},
		{"call recording", func(a *Adapter) error { return a.StartCallRecording("chan-1 x", "k.wav") }},
		{"audio stream", func(a *Adapter) error { return a.StartAudioStream("chan-1", "ws://h/a b", 16000, "") }},
		{"audio stream metadata", func(a *Adapter) error { return a.StartAudioStream("chan-1", "ws://h/a", 16000, "m\n") }},
		{"flush", func(a *Adapter) error { return a.FlushRegistration("internal", "1001 x") }},
		{"bridge", func(a *Adapter) error {
			return a.BridgeToEndpoint("chan-1", party, "user/1001@x", map[string]string{"k": "v'"})
		}},
		{"exists", func(a *Adapter) error { _, err := a.ChannelExists("chan-1 x"); return err }},
		{"getvar", func(a *Adapter) error { _, err := a.ChannelVariable("chan-1", "a b"); return err }},
		{"dump", func(a *Adapter) error { _, err := a.DumpChannel("chan-1\n"); return err }},
		{"members", func(a *Adapter) error { _, err := a.ListQueueMembers("support en"); return err }},
		{"registrations", func(a *Adapter) error { _, err := a.Registrations("internal x"); return err }},
		{"gateway", func(a *Adapter) error { _, err := a.GatewayUp("pstn x"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, c := newTestAdapter()
			if err := tt.act(a); !errors.Is(err, ErrUnsafeValue) {
				t.Fatalf("error = %v, want ErrUnsafeValue", err)
			}
			// A call recording reads the switch's recordings directory first,
			// which carries none of the caller's values; nothing else may go.
			for _, cmd := range c.sent {
				if !strings.HasPrefix(cmd, "global_getvar ") {
					t.Errorf("a refused command reached the switch: %q", cmd)
				}
			}
		})
	}
}

// The refusal says which character, and quotes the value so a line break in
// it cannot forge a log line of its own.
func TestTheRefusalNamesTheCharacterAndQuotesTheValue(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdapter()
	err := a.SetVariable("chan-1", "aicc_bot_summary", "a\nb")
	if err == nil || strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("error = %q, want one line", err)
	}
	if !strings.Contains(err.Error(), `"\n"`) {
		t.Errorf("error = %q, want it to name the line break", err)
	}
}
