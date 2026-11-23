// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"

	"github.com/rasonyang/ai-native-callcenter/docs"
)

// runSkill implements `aicc skill`: print the SKILL.md embedded at build
// time, so the skill a coding agent registers always matches the installed
// binary rather than a copy that drifted. It teaches use, not installation;
// docs/agent-install.md says where each agent tool reads skills from.
func runSkill([]string) error {
	return writeSkill(os.Stdout)
}

func writeSkill(w io.Writer) error {
	_, err := w.Write(docs.Skill)
	return err
}
