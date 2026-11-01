// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The shapes a person or an integration really sends, plus the ones that must
// stay out of a command line. Wanted outcomes are written by hand, not derived
// from the code under test.
var dialableCases = []struct {
	in            string
	dest, isExten bool
}{
	{"+8613800138000", true, false},
	{"8613800138000", true, true},
	{"13800138000", true, true},
	{"+1 (415) 555-0100", false, false},
	{"+", false, false},
	{"++8613800138000", false, false},
	{"86+13800138000", false, false},
	{"+0123", true, false}, // the dial path only bounds the digits; E.164 shape is phonenum's job
	{"+12", false, false},
	{"+123", true, false},
	{"+" + strings.Repeat("1", 20), true, false},
	{"+" + strings.Repeat("1", 21), false, false},
	{strings.Repeat("1", 20), true, true},
	{strings.Repeat("1", 21), false, false},
	{"", false, false},
	{" ", false, false},
	{"+86 138 0013 8000", false, false},
	{"＋8613800138000", false, false}, // full-width plus
	{"+٨٦١٣٨٠٠", false, false},       // Arabic-Indic digits
	{"٨٦١٣٨٠٠", false, false},
	{"+8613800138000\n", false, false},
	{"8613800138000\n", false, false},
	{"1002", true, true},
	{"+1002", true, false},
	{"100", true, true},
	{"10", false, false},
	{"1002;x=y", false, false},
	{"1002,x=y", false, false},
	{"1002'}", false, false},
}

func TestIsDialableAndIsExtensionOnTheNamedCases(t *testing.T) {
	t.Parallel()
	for _, tt := range dialableCases {
		if got := isDialable(tt.in); got != tt.dest {
			t.Errorf("isDialable(%q) = %v, want %v", tt.in, got, tt.dest)
		}
		if got := isExtension(tt.in); got != tt.isExten {
			t.Errorf("isExtension(%q) = %v, want %v", tt.in, got, tt.isExten)
		}
	}
}

// 300 generated strings built from realistic parts and realistic damage. The
// oracle is a regular expression, so it shares no code with the predicate.
func TestIsDialableOnAGeneratedTable(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(159))
	destRe := regexp.MustCompile(`^\+?[0-9]{3,20}$`)
	extRe := regexp.MustCompile(`^[0-9]{3,20}$`)

	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.Intn(10))
		}
		return string(b)
	}
	damage := []func(string) string{
		func(s string) string { return s },
		func(s string) string { return "+" + s },
		func(s string) string { return "++" + s },
		func(s string) string { return s + "\n" },
		func(s string) string { return " " + s },
		func(s string) string { return s[:len(s)/2] + " " + s[len(s)/2:] },
		func(s string) string { return s[:len(s)/2] + "-" + s[len(s)/2:] },
		func(s string) string { return s[:len(s)/2] + "+" + s[len(s)/2:] },
		func(s string) string { return "(" + s[:3] + ")" + s[3:] },
		func(s string) string { return "00" + s },
		func(s string) string { return s + "x" },
		func(s string) string { return "＋" + s },
	}
	seen := map[bool]int{}
	for i := 0; i < 300; i++ {
		n := 3 + rng.Intn(22)
		in := damage[rng.Intn(len(damage))](digits(n))
		if i%25 == 0 {
			in = ""
		}
		wantDest, wantExt := destRe.MatchString(in), extRe.MatchString(in)
		if got := isDialable(in); got != wantDest {
			t.Errorf("isDialable(%q) = %v, want %v", in, got, wantDest)
		}
		if got := isExtension(in); got != wantExt {
			t.Errorf("isExtension(%q) = %v, want %v", in, got, wantExt)
		}
		seen[wantDest]++
	}
	if seen[true] < 30 || seen[false] < 30 {
		t.Errorf("table is lopsided: %v", seen)
	}
}

func TestDialAIKeepsThePlusOnTheOriginateEndpoint(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "+8613800138000", DIDNumber: "95012"}); err != nil {
		t.Fatal(err)
	}
	if got, want := sw.lastOriginate().endpoint, "sofia/gateway/pstn_gateway/+8613800138000"; got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
}

func TestDialAIRefusesWhatIsNotAnOptionalPlusAndDigits(t *testing.T) {
	t.Parallel()
	for _, to := range []string{"++8613800138000", "86+13800138000", "+", "+86 138", "＋8613800138000"} {
		s := testService(t, &fakeSwitch{}, nil)
		_, err := s.DialAI(context.Background(), AIDialRequest{To: to, DIDNumber: "95012"})
		if !errors.Is(err, ErrBadNumber) {
			t.Errorf("DialAI(%q) error = %v, want ErrBadNumber", to, err)
		}
	}
}

func TestClickToDialAcceptsAPlusDestinationButNotAPlusExtension(t *testing.T) {
	t.Parallel()
	s := testService(t, &fakeSwitch{}, nil)
	if _, err := s.Dial(context.Background(), AgentDialRequest{
		CallID: uuid.Must(uuid.NewV7()), AgentExtension: "+1002", To: "13912345678",
	}); !errors.Is(err, ErrBadNumber) {
		t.Errorf("a + extension: error = %v, want ErrBadNumber", err)
	}

	sw := &fakeSwitch{}
	s = testService(t, sw, nil)
	if _, err := s.Dial(context.Background(), AgentDialRequest{
		AgentExtension: "1002", To: "+8613800138000",
	}); err != nil {
		t.Fatalf("a + destination: %v", err)
	}
	if got := sw.lastOriginate().vars["aicc_destination"]; got != "+8613800138000" {
		t.Errorf("aicc_destination = %q, want the number as sent", got)
	}
}
