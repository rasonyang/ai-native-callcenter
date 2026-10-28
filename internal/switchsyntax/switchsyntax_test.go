// SPDX-License-Identifier: Apache-2.0

package switchsyntax

import "testing"

// What our values legitimately are passes: identifiers, numbers, dial strings,
// language tags. None of them needs a character the switch reads as syntax.
func TestOrdinaryValuesAreSafe(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"", "en", "zh-hans", "pt-br", "95011", "+8613800138000", "OUTBOUND", "true", "PCMU",
		"019ffa1d-0dc1-7b9e-b124-cffb41e90a3d", "agent-wei.li_2", "sofia/gateway/aicc_bot/95011",
		"user/1001@aicc.test", "sofia/external/1001@10.0.0.5:5080", "a=b", "中文",
	} {
		if bad := Unsafe(v); bad != "" {
			t.Errorf("Unsafe(%q) = %q, want it safe", v, bad)
		}
	}
}

func TestEverySyntaxCharacterIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"en'", "'"},
		{"en^hangup", "^"},
		{"zh,x=y", ","},
		{"a|b", "|"},
		{"a{b", "{"},
		{"a}b", "}"},
		{"a$b", "$"},
		{`a\b`, `\`},
		{"zh hans", " "},
		{"zh\thans", "\t"},
		{"en\n", "\n"},
		{"en\r", "\r"},
		{"en\x00", "\x00"},
		{"en\u00a0", "\u00a0"}, // a no-break space is still a space
		{"en\u0085", "\u0085"}, // NEL is a C1 control
		{"95011:_:loopback/1", ":_:"},
	}
	for _, tt := range tests {
		if got := Unsafe(tt.in); got != tt.want {
			t.Errorf("Unsafe(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A colon and an underscore are ordinary on their own; only the three
// together split a dial string.
func TestOnlyTheWholeEnterpriseSeparatorIsRefused(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"sip:1001@host:5060", "aicc_bot", ":_", "_:", ":__:"} {
		if bad := Unsafe(v); bad != "" {
			t.Errorf("Unsafe(%q) = %q, want it safe", v, bad)
		}
	}
}

func TestASoundMayNameAVariableButNotRunACommand(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"$${hold_music}", "${sounds_dir}/music/8000", "local_stream://moh", "ivr/ivr-welcome.wav",
		"tone_stream://%(250,0,500)", "silence_stream://-1", "", "$5", "a$",
	} {
		if CallsCommand(v) {
			t.Errorf("CallsCommand(%q) = true, want a plain sound", v)
		}
	}
	for _, v := range []string{
		"${system(id)}", "${system id}", "$${system(id)}", "${}", "${unterminated",
		"${a}${b(c)}", "${a:1}", "${${inner}}",
	} {
		if !CallsCommand(v) {
			t.Errorf("CallsCommand(%q) = false, want it refused", v)
		}
	}
}
