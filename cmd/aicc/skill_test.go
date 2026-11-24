// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/docs"
)

// `aicc skill` prints exactly the file embedded at build time: the skill a
// coding agent registers is the one this binary carries, not a copy that
// drifted.
func TestSkillPrintsTheEmbeddedFile(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSkill(&buf); err != nil {
		t.Fatalf("writeSkill: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), docs.Skill) {
		t.Fatalf("aicc skill printed %d bytes; the embedded SKILL.md is %d", buf.Len(), len(docs.Skill))
	}
}

// The embedded file is an Agent Skills document: its frontmatter carries the
// name and description every skill loader routes on.
func TestSkillFrontmatterNamesTheSkill(t *testing.T) {
	fm, _, ok := strings.Cut(strings.TrimPrefix(string(docs.Skill), "---\n"), "\n---")
	if !ok {
		t.Fatal("SKILL.md does not open with a frontmatter block")
	}
	for _, field := range []string{"name", "description"} {
		found := false
		for _, line := range strings.Split(fm, "\n") {
			if key, _, ok := strings.Cut(line, ":"); ok && key == field {
				found = true
			}
		}
		if !found {
			t.Fatalf("SKILL.md frontmatter has no %s field", field)
		}
	}
}
