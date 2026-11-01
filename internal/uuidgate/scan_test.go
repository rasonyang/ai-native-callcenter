// SPDX-License-Identifier: Apache-2.0

package uuidgate

import (
	"os"
	"path/filepath"
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

// No non-test Go file of the module mints a v4 id.
func TestNoCodeMintsAV4UUID(t *testing.T) {
	offences, err := ScanTree(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(offences) > 0 {
		var lines []string
		for _, o := range offences {
			lines = append(lines, o.String())
		}
		t.Errorf("these calls mint a v4 UUID; use uuid.NewV7 for an id (uuid.Must(uuid.NewV7())), "+
			"or crypto/rand for a token that is not an id (SIP Call-ID, tag, branch, nonce):\n  %s",
			strings.Join(lines, "\n  "))
	}
}

// A planted call is found in a regular file and ignored in a _test.go file
// and under testdata.
func TestScanTreeSkipsTestFilesAndTestdata(t *testing.T) {
	dir := t.TempDir()
	bad := []byte("package x\nimport \"github.com/google/uuid\"\nfunc f() { _ = uuid.New() }\n")
	for path := range map[string]bool{
		"a/a.go": true, "a/a_test.go": false, "a/testdata/t.go": false,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, bad, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ScanTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != filepath.Join("a", "a.go") {
		t.Errorf("want exactly a/a.go flagged, got %v", got)
	}
}

func TestScanFlagsEveryBannedConstructor(t *testing.T) {
	src := `package x

import (
	u "github.com/google/uuid"
)

func f() {
	_ = u.New()
	_ = u.NewString()
	_, _ = u.NewRandom()
	_, _ = u.NewRandomFromReader(nil)
	_ = u.Must(u.NewV7())
}
`
	got, err := Scan("x.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 offences, got %v", got)
	}
	if got[0].Line != 8 || got[0].Call != "New" || got[1].Call != "NewString" {
		t.Errorf("unexpected offences: %v", got)
	}
}

func TestScanFollowsDotImportsAndIgnoresOtherPackages(t *testing.T) {
	dot := "package x\nimport . \"github.com/google/uuid\"\nfunc f() { _ = New() }\n"
	got, _ := Scan("d.go", []byte(dot))
	if len(got) != 1 {
		t.Errorf("dot import: want 1 offence, got %v", got)
	}
	other := "package x\nimport \"example.com/uuid\"\nfunc f() { _ = uuid.New() }\n"
	if got, _ := Scan("o.go", []byte(other)); len(got) != 0 {
		t.Errorf("another package named uuid flagged: %v", got)
	}
	plain := "package x\nimport \"github.com/google/uuid\"\nfunc f() { _ = uuid.New() }\n"
	if got, _ := Scan("p.go", []byte(plain)); len(got) != 1 {
		t.Errorf("plain import: want 1 offence, got %v", got)
	}
}
