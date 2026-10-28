// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"errors"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestExtensionValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		ext        Extension
		onCreate   bool
		wantErr    string
		wantAssert func(t *testing.T, e Extension)
	}{
		{
			name:     "digits are required",
			ext:      Extension{Number: "10a1", Password: "secret123"},
			onCreate: true,
			wantErr:  "digits",
		},
		{
			name:     "a new extension needs a usable password",
			ext:      Extension{Number: "1001", Password: "123"},
			onCreate: true,
			wantErr:  "password",
		},
		// #367: six characters as the contract counts them (code points), not
		// six bytes. Five Chinese characters are fifteen bytes and still five.
		{
			name:     "five Chinese characters are too short a password",
			ext:      Extension{Number: "1001", Password: strings.Repeat("话", 5)},
			onCreate: true,
			wantErr:  "password",
		},
		{
			name:     "six Chinese characters are a long enough password",
			ext:      Extension{Number: "1001", Password: strings.Repeat("话", 6)},
			onCreate: true,
		},
		{
			name:     "five emoji and Chinese characters are too short a password",
			ext:      Extension{Number: "1001", Password: "📞话📞话📞"},
			onCreate: true,
			wantErr:  "password",
		},
		{
			name:     "six emoji and Chinese characters are a long enough password",
			ext:      Extension{Number: "1001", Password: "📞话📞话📞话"},
			onCreate: true,
		},
		{
			name:     "an edit may leave the password alone",
			ext:      Extension{Number: "1001"},
			onCreate: false,
		},
		{
			name:     "an unnamed extension is named after its number",
			ext:      Extension{Number: "1001", Password: "secret123"},
			onCreate: true,
			wantAssert: func(t *testing.T, e Extension) {
				if e.DisplayName != "Extension 1001" {
					t.Errorf("displayName = %q", e.DisplayName)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ext := tt.ext
			err := ext.validate(tt.onCreate)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validate() = %v, want an error mentioning %q", err, tt.wantErr)
				}
				if !errors.Is(err, ErrValidation) {
					t.Errorf("error does not wrap ErrValidation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() = %v", err)
			}
			if tt.wantAssert != nil {
				tt.wantAssert(t, ext)
			}
		})
	}
}

func TestQueueValidation(t *testing.T) {
	t.Parallel()
	valid := Queue{Name: "support-en", ExtNumber: "7001"}

	tests := []struct {
		name    string
		mutate  func(q *Queue)
		wantErr string
	}{
		{name: "a plain queue is accepted"},
		{
			name:    "a name is required",
			mutate:  func(q *Queue) { q.Name = "  " },
			wantErr: "name is required",
		},
		{
			// The name reaches the switch as a queue identifier, and both the
			// tier read-back and the event normalizer cut one at the first "@"
			// to recognise names qualified before 2026-08-22. A name holding
			// one would be truncated to something else.
			name:    "the name cannot contain an at sign",
			mutate:  func(q *Queue) { q.Name = "support@default" },
			wantErr: "@",
		},
		{
			name:    "the name cannot contain a quote",
			mutate:  func(q *Queue) { q.Name = "it's" },
			wantErr: "quotes",
		},
		{
			name:    "the queue extension must be dialable",
			mutate:  func(q *Queue) { q.ExtNumber = "seven" },
			wantErr: "digits",
		},
		{
			name:    "unknown strategies are refused",
			mutate:  func(q *Queue) { q.Strategy = "SHORTEST_NAME" },
			wantErr: "strategy",
		},
		{
			name:    "forwarding overflow needs somewhere to forward to",
			mutate:  func(q *Queue) { q.Overflow = Overflow{Type: OverflowForward} },
			wantErr: "target",
		},
		{
			name:    "bot overflow needs a number to reach the bot on",
			mutate:  func(q *Queue) { q.Overflow = Overflow{Type: OverflowBotFlow} },
			wantErr: "target",
		},
		{
			name:    "weekdays are 0 to 6",
			mutate:  func(q *Queue) { q.Hours = []BusinessHours{{Weekday: 7, Open: "09:00", Close: "18:00"}} },
			wantErr: "weekday",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := valid
			if tt.mutate != nil {
				tt.mutate(&q)
			}
			err := q.validate()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validate() = %v, want an error mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() = %v", err)
			}
		})
	}
}

func TestQueueDefaults(t *testing.T) {
	t.Parallel()
	q := Queue{Name: "support-en", ExtNumber: "7001"}
	if err := q.validate(); err != nil {
		t.Fatal(err)
	}
	if q.Strategy != StrategyLongestIdle {
		t.Errorf("strategy = %q, want the longest-idle default", q.Strategy)
	}
	if q.Overflow.Type != OverflowAnnounceHangup {
		t.Errorf("overflow = %q, want an announcement by default", q.Overflow.Type)
	}
	if q.MohSound == "" {
		t.Error("a queue with no hold music would play silence to a waiting caller")
	}
	if q.DisplayName != "support-en" {
		t.Errorf("displayName = %q, want it derived from the name", q.DisplayName)
	}
	if q.Hours == nil {
		t.Error("hours must serialize as an empty list rather than null")
	}
}

func TestDIDValidation(t *testing.T) {
	t.Parallel()
	someFlow := uuid.New()
	tests := []struct {
		name    string
		did     DID
		wantErr string
	}{
		{name: "an inbound number with the flow it answers with",
			did: DID{Number: "95011", AllowInbound: true, FlowID: &someFlow}},
		// The one shape a flowless number is allowed to have: nobody calls it,
		// so there is nothing for it to answer.
		{name: "an outbound-only number needs no flow",
			did: DID{Number: "95011", AllowOutbound: true}},
		{name: "an inbound number without a flow is refused",
			did: DID{Number: "95011", AllowInbound: true}, wantErr: "flow"},
		{name: "a number calls cannot go through either way is refused",
			did: DID{Number: "95011"}, wantErr: "take calls"},
		{name: "a number that cannot dial out cannot be the default one",
			did:     DID{Number: "95011", AllowInbound: true, FlowID: &someFlow, IsDefaultOutbound: true},
			wantErr: "default"},
		{name: "letters are refused",
			did: DID{Number: "95-011", AllowInbound: true, FlowID: &someFlow}, wantErr: "digits"},
		{name: "a long language tag is refused",
			did:     DID{Number: "95011", Language: "english-uk", AllowInbound: true, FlowID: &someFlow},
			wantErr: "language"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := tt.did
			err := d.validate()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validate() = %v, want an error mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() = %v", err)
			}
			if d.Language != "en" {
				t.Errorf("language = %q, want the English default", d.Language)
			}
		})
	}
}

// A number's language rides every call on it to the switch and to the bot, so
// it is a lowercase BCP 47 tag of at most eight characters and nothing else.
// Checked as sent: a line break or a space is refused, not trimmed away.
func TestADIDLanguageIsALowercaseLanguageTag(t *testing.T) {
	t.Parallel()
	someFlow := uuid.New()
	for _, language := range []string{"en", "zh", "pt-br", "zh-hans", "yue", "es-419", "zh-hk"} {
		d := DID{Number: "95011", Language: language, AllowInbound: true, FlowID: &someFlow}
		if err := d.validate(); err != nil {
			t.Errorf("language %q was refused: %v", language, err)
		} else if d.Language != language {
			t.Errorf("language %q was stored as %q", language, d.Language)
		}
	}
	for _, language := range []string{
		"EN", "zh_CN", "en^hangup", "en'", "zh,x=y", "zh hans", "中文", "zh-hans-cn",
		"e", "zh-hans-x", "en\nzh", "en\n", " en", "en-", "en--us", "-en", "engl", "en.us",
		"en$", "en{", "en}", `en\`, "en|x", "en\t", "en\r",
	} {
		d := DID{Number: "95011", Language: language, AllowInbound: true, FlowID: &someFlow}
		err := d.validate()
		var invalid *ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("language %q was accepted (err %v)", language, err)
			continue
		}
		if invalid.Field != "language" || invalid.Rule != "LANGUAGE_SUBTAG" {
			t.Errorf("language %q: field/rule = %q/%q, want language/LANGUAGE_SUBTAG",
				language, invalid.Field, invalid.Rule)
		}
	}
}

// The values a queue sends to the switch end up in places the switch parses:
// the name is the callcenter application's data, the overflow target the end
// of a dial string or a transfer's destination, and the sounds are expanded
// before they play. Each refuses what the switch would read as syntax.
func TestAQueueRefusesWhatTheSwitchWouldReadAsSyntax(t *testing.T) {
	t.Parallel()
	valid := func() Queue {
		return Queue{Name: "support-en", ExtNumber: "7001",
			Overflow: Overflow{Type: OverflowBotFlow, Target: "95012"}}
	}
	tests := []struct {
		name        string
		mutate      func(q *Queue)
		field, rule string
	}{
		{"a name with a dollar", func(q *Queue) { q.Name = "q${system(id)}" }, "name", "NAME_CHARSET"},
		{"a name with a line break", func(q *Queue) { q.Name = "support\nen" }, "name", "NAME_CHARSET"},
		{"a name with a tab", func(q *Queue) { q.Name = "support\ten" }, "name", "NAME_CHARSET"},
		{"a name with a comma", func(q *Queue) { q.Name = "support,en" }, "name", "NAME_CHARSET"},
		{"a name with a caret", func(q *Queue) { q.Name = "support^en" }, "name", "NAME_CHARSET"},
		{"a name with a backslash", func(q *Queue) { q.Name = `support\en` }, "name", "NAME_CHARSET"},
		{"a name with a brace", func(q *Queue) { q.Name = "support{en}" }, "name", "NAME_CHARSET"},
		{"a target with a space", func(q *Queue) { q.Overflow.Target = "95012 inline" }, "overflow.target", "TARGET_CHARSET"},
		{"a target with a comma", func(q *Queue) { q.Overflow.Target = "95012,loopback/1" }, "overflow.target", "TARGET_CHARSET"},
		{"a target with a dollar", func(q *Queue) { q.Overflow.Target = "${system(id)}" }, "overflow.target", "TARGET_CHARSET"},
		{"a target with a pipe", func(q *Queue) { q.Overflow.Target = "95012|loopback/1" }, "overflow.target", "TARGET_CHARSET"},
		{"a target with the enterprise separator", func(q *Queue) {
			q.Overflow = Overflow{Type: OverflowForward, Target: "1001:_:1002"}
		}, "overflow.target", "TARGET_CHARSET"},
		{"a target with a quote", func(q *Queue) {
			q.Overflow = Overflow{Type: OverflowForward, Target: "1001'"}
		}, "overflow.target", "TARGET_CHARSET"},
		{"hold music that runs a command", func(q *Queue) { q.MohSound = "${system(id)}" }, "mohSound", "SOUND_EXPANSION"},
		{"an announcement that runs a command", func(q *Queue) { q.AnnounceSound = "${system id}" }, "announceSound", "SOUND_EXPANSION"},
		{"an overflow sound that runs a command", func(q *Queue) {
			q.Overflow = Overflow{Type: OverflowAnnounceHangup, Sound: "ivr/x.wav${bg_system(id)}"}
		}, "overflow.sound", "SOUND_EXPANSION"},
		{"an overflow sound with a line break", func(q *Queue) {
			q.Overflow = Overflow{Type: OverflowAnnounceHangup, Sound: "ivr/x.wav\n"}
		}, "overflow.sound", "SOUND_EXPANSION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := valid()
			tt.mutate(&q)
			err := q.validate()
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("validate() = %v, want a refusal", err)
			}
			if invalid.Field != tt.field || invalid.Rule != tt.rule {
				t.Errorf("field/rule = %q/%q, want %q/%q", invalid.Field, invalid.Rule, tt.field, tt.rule)
			}
		})
	}

	// What queues are actually configured with still passes: the stored
	// default, a stream, a file, a tone, and a name that is not ASCII.
	for _, mutate := range []func(q *Queue){
		func(q *Queue) { q.MohSound = "$${hold_music}" },
		func(q *Queue) { q.MohSound = "local_stream://moh" },
		func(q *Queue) { q.AnnounceSound = "${sounds_dir}/en/us/callie/ivr/8000/ivr-hold.wav" },
		func(q *Queue) {
			q.Overflow = Overflow{Type: OverflowAnnounceHangup, Sound: "tone_stream://%(250,0,500)"}
		},
		func(q *Queue) { q.Overflow = Overflow{Type: OverflowForward, Target: "+8613800138000"} },
		func(q *Queue) { q.Name = "售后.vip_queue-2" },
	} {
		q := valid()
		mutate(&q)
		if err := q.validate(); err != nil {
			t.Errorf("an ordinary queue was refused: %v (%+v)", err, q)
		}
	}
}

// The refusal names its field and rule, which is what lets a form highlight
// the input instead of printing a sentence at the bottom about nothing in
// particular (walkthrough step 11).
func TestAValidationFailureNamesItsFieldAndRule(t *testing.T) {
	t.Parallel()
	d := DID{Number: "95001", AllowInbound: false, AllowOutbound: false}
	err := d.validate()
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error is not a *ValidationError: %v", err)
	}
	if invalid.Field != "allowInbound" || invalid.Rule != "DIRECTION_REQUIRED" {
		t.Errorf("field/rule = %q/%q, want allowInbound/DIRECTION_REQUIRED", invalid.Field, invalid.Rule)
	}
	if !errors.Is(err, ErrValidation) {
		t.Error("a ValidationError must still be ErrValidation to every existing check")
	}
}
