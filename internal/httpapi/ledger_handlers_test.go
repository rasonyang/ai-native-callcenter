// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// A missed reason crosses four layers under four spellings of the same
// vocabulary: the assembler decides it, the cdrs CHECK stores it, the contract
// names it, and the console translates it. Nothing joins them at compile time
// — the ledger group still writes store.CDR straight out, where the field is a
// plain string — so a sixth reason added to the assembler alone would ship,
// pass every other test, and arrive in a browser as a raw translation key.
//
// That is not hypothetical: AGENTS_DID_NOT_ANSWER did exactly that. It has
// been produced since the ledger was built and neither locale ever named it,
// so a supervisor asking why a call went unanswered read
// "cdr.missedReasons.AGENTS_DID_NOT_ANSWER" on the one report that answers it.
//
// This is the join. The contract is the far end because it is the promise made
// to clients; the locales are checked on their own side, in the web tests.
func TestEveryMissedReasonIsOneTheContractNames(t *testing.T) {
	for _, reason := range telephony.MissedReasons {
		if !api.MissedReason(reason).Valid() {
			t.Errorf("the assembler can decide %s, which the contract's MissedReason "+
				"enum does not name — clients cannot type it and the console "+
				"cannot translate it", reason)
		}
	}

	// And the other way: a value the contract promises but nothing decides is a
	// word two locales must carry for a call that never arrives. OUT_OF_HOURS
	// was one for as long as the schema allowed it.
	decided := make(map[string]bool, len(telephony.MissedReasons))
	for _, reason := range telephony.MissedReasons {
		decided[reason] = true
	}
	for _, promised := range []api.MissedReason{
		api.MissedReasonSHORTABANDONED, api.MissedReasonABANDONEDRINGING,
		api.MissedReasonABANDONEDWAITING, api.MissedReasonAGENTSDIDNOTANSWER,
		api.MissedReasonNOAVAILABLEAGENT,
	} {
		if !decided[string(promised)] {
			t.Errorf("the contract names %s but no call can be given it", promised)
		}
	}
}
