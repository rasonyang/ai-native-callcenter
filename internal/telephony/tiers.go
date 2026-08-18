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

// bareQueueName is QueueName's inverse.
//
// The domain suffix is ours, not mod_callcenter's: aicc_xml.lua names every
// queue "<name>@<domain>" when it renders the configuration (aicc_xml.lua:156),
// aicc_queue.lua dials the same form (aicc_queue.lua:66), and QueueName applies
// it to every tier command. The switch appends nothing — it stores and reports
// literally what it was told. So the adapter owns the qualification in both
// directions and no caller above it needs to know queue names carry a domain.
//
// TrimSuffix rather than cutting at the first "@": a queue qualified by some
// other domain is not ours, and collapsing it to a bare name would let it
// masquerade as one.
func (a *Adapter) bareQueueName(qualified string) string {
	return strings.TrimSuffix(qualified, "@"+a.domain)
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
