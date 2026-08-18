// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"strconv"
	"strings"
)

// Tier is one agent's membership of one queue, as the switch currently holds
// it. Agents and tiers are both runtime state in mod_callcenter, so this is
// what the switch believes rather than what anyone configured.
type Tier struct {
	Queue    string
	Agent    string
	Level    int
	Position int
}

// CallcenterTiers asks the switch which agents staff which queues right now.
//
// Needed because staffing is reconciled rather than merely applied: a tier the
// switch still holds for a queue this system no longer staffs has to be
// removed, and there is no way to learn of it without asking.
func (a *Adapter) CallcenterTiers() ([]Tier, error) {
	out, err := a.cmd.API("callcenter_config tier list")
	if err != nil {
		return nil, fmt.Errorf("list tiers: %w", err)
	}
	return parseTiers(out), nil
}

// CallcenterQueuesForAgent reports the queues the switch believes one agent
// staffs. The caller wants names to compare against its own record, so the
// level and position the switch also holds are deliberately not returned:
// those are always the database's answer, and offering a second copy invites
// somebody to reconcile against the wrong one.
func (a *Adapter) CallcenterQueuesForAgent(agent string) ([]string, error) {
	tiers, err := a.CallcenterTiers()
	if err != nil {
		return nil, err
	}
	var queues []string
	for _, t := range tiers {
		if t.Agent == agent {
			queues = append(queues, t.Queue)
		}
	}
	return queues, nil
}

// parseTiers reads mod_callcenter's pipe-separated listing. The header line and
// the trailing +OK are not tiers, and anything that does not carry five fields
// with numeric level and position is skipped rather than guessed at.
func parseTiers(out string) []Tier {
	var tiers []Tier
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+OK") || strings.HasPrefix(line, "queue|") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}
		level, err := strconv.Atoi(parts[3])
		if err != nil {
			continue
		}
		position, err := strconv.Atoi(parts[4])
		if err != nil {
			continue
		}
		tiers = append(tiers, Tier{
			Queue: parts[0], Agent: parts[1], Level: level, Position: position,
		})
	}
	return tiers
}
