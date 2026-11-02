// SPDX-License-Identifier: Apache-2.0

package callidgate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's directory")
		}
		dir = parent
	}
}

// Every "callId" attribute in a non-test package of the module holds a
// uuid.UUID or the String() of one.
func TestEveryCallIdAttributeHoldsADomainCallID(t *testing.T) {
	offences, err := Scan(moduleRoot(t), "./...")
	if err != nil {
		t.Fatal(err)
	}
	if len(offences) > 0 {
		var lines []string
		for _, o := range offences {
			lines = append(lines, o.String())
		}
		t.Errorf(`"callId" is the domain call id (a uuid.UUID) and nothing else; use sipCallId for a SIP Call-ID, `+
			"channelId for a switch channel, legId/partyId for a leg, toolCallId for a provider tool call (#439):\n  %s",
			strings.Join(lines, "\n  "))
	}
}

func TestPlantedOffencesAreFoundOnTheirLines(t *testing.T) {
	got, err := Scan(moduleRoot(t), "./internal/callidgate/testdata/bad")
	if err != nil {
		t.Fatal(err)
	}
	type at struct {
		file string
		line int
	}
	var have []at
	for _, o := range got {
		have = append(have, at{o.File, o.Line})
	}
	f := filepath.Join("internal", "callidgate", "testdata", "bad", "bad.go")
	// plain, slog.String of a string field, []any literal, append, const key,
	// non-slog interface, trailing key, attribute.String, attribute.Key.
	want := []at{{f, 21}, {f, 22}, {f, 23}, {f, 25}, {f, 26}, {f, 27}, {f, 28}, {f, 29}, {f, 30}}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("offences = %v\nwant %v", got, want)
	}
	if len(got) > 6 && !strings.Contains(got[6].Reason, "no value") {
		t.Errorf("trailing key reason = %q", got[6].Reason)
	}
}

func TestAcceptedShapesAreNotFlagged(t *testing.T) {
	got, err := Scan(moduleRoot(t), "./internal/callidgate/testdata/good")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("flagged %v", got)
	}
}

func TestAPackageThatDoesNotTypeCheckFailsTheGate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/broken\n\ngo 1.22\n")
	write("x.go", "package x\n\nfunc f() { var s string = 1; _ = s }\n")
	if _, err := Scan(dir, "./..."); err == nil || !strings.Contains(err.Error(), "cannot type-check") {
		t.Errorf("err = %v, want a type-check failure", err)
	}
}
