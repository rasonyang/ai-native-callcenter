// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"errors"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestExtensionValidation(t *testing.T) {
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
			// The name travels to the switch inside a queue identifier of the
			// form name@domain, so these characters would make it ambiguous.
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

// The refusal names its field and rule, which is what lets a form highlight
// the input instead of printing a sentence at the bottom about nothing in
// particular (walkthrough step 11).
func TestAValidationFailureNamesItsFieldAndRule(t *testing.T) {
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
