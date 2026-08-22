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

// callcenterTierRows asks the switch which agents staff which queues right now.
//
// Needed because staffing is reconciled rather than merely applied: a tier the
// switch still holds for a queue this system no longer staffs has to be
// removed, and there is no way to learn of it without asking.
func (a *Adapter) callcenterTierRows() ([]Tier, error) {
	out, err := a.cmd.API("callcenter_config tier list")
	if err != nil {
		return nil, fmt.Errorf("list tiers: %w", err)
	}
	return parseTiers(out), nil
}

// CallcenterTiers reports what the switch believes, as agent to the queues
// they staff, named as this system names them.
//
// Names only: the level and position the switch also holds are deliberately
// dropped. Those are always the database's answer, and offering a second copy
// invites somebody to reconcile against the wrong one.
func (a *Adapter) CallcenterTiers() (map[string][]string, error) {
	tiers, err := a.callcenterTierRows()
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, t := range tiers {
		out[t.Agent] = append(out[t.Agent], a.bareQueueName(t.Queue))
	}
	return out, nil
}

// bareQueueName reads a queue name the switch reports back.
//
// Names written now carry no domain, so for those this returns them unchanged.
// It still cuts at the first "@" because rows written before 2026-08-22 are
// qualified by whatever the host's address was then, and those are the rows
// that most need recognising: an unrecognised tier is one converge can neither
// match nor remove, which is how a tier under this machine's previous address
// outlived it (C1). Reading them as the queue they always were lets converge
// reconcile them away rather than stare past them.
//
// The switch appends nothing of its own — it stores and reports literally what
// it was told — so anything qualified here was qualified by us.
func (a *Adapter) bareQueueName(qualified string) string {
	if at := strings.Index(qualified, "@"); at >= 0 {
		return qualified[:at]
	}
	return qualified
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
