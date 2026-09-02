// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"slices"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

// grantedScopes turns a signed-in account's role into the scopes it holds.
//
// This map is a convenience for page login and nothing more. The model is the
// scope: a key's grant is written down when it is issued and owes nothing to
// this table, and no code below the authentication middleware asks what role
// anybody has. Deleting a role from the product would cost one entry here.
//
// The three rows are not opinion. They were derived mechanically from what
// each role could reach on 2026-08-31 — the role guards were a *lower bound*
// (auth.Role.AtLeast compares rank), so the input was reachability, not the
// guard's label — and the derivation is replayable: `python3
// docs/auth/scopemap.py` prints these rows and enumerates every widening and
// narrowing against that baseline. Nine widenings survive, all of them
// config:read on screens a supervisor already had a link to, and they were
// ruled deliberately when the change was made. Narrowings: none.
func grantedScopes(role auth.Role) []string {
	switch role {
	case auth.RoleAdmin:
		return slices.Clone(api.AllScopes)
	case auth.RoleSupervisor:
		return slices.Clone(supervisorScopes)
	case auth.RoleAgent:
		return slices.Clone(agentScopes)
	default:
		return nil
	}
}

// An agent takes calls: their own presence, their own calls, the customer
// record book they read from while talking, and their own history.
var agentScopes = sorted([]string{
	api.ScopeAgentAct,
	api.ScopeAgentRead,
	api.ScopeCallsControl,
	api.ScopeCallsCreate,
	api.ScopeCallsReadOwn,
	api.ScopeContactsRead,
	api.ScopeContactsWrite,
	api.ScopeHistoryReadOwn,
})

// A supervisor watches the floor: everything an agent does, plus the whole
// floor's live calls and history, monitoring, quality review, the aggregates,
// the roster — and reading the configuration those screens link to.
var supervisorScopes = sorted(append([]string{
	api.ScopeAgentManage,
	api.ScopeCallsCreateAI,
	api.ScopeCallsMonitor,
	api.ScopeCallsReadAll,
	api.ScopeConfigRead,
	api.ScopeHistoryReadAll,
	api.ScopeQualityReview,
	api.ScopeReportsRead,
}, agentScopes...))

func sorted(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}
