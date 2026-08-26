// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/google/uuid"
)

// The filter decides which finished calls a customer is told about, so getting
// it wrong is not a visible failure: the deliveries simply never happen and the
// first anyone knows is a customer reconciling a month and finding a gap.
func TestAFilterAsksForWhatItNamesAndNothingMore(t *testing.T) {
	support := uuid.New()
	sales := uuid.New()

	answered := CDR{
		CallID: uuid.New(), CallType: "OUTBOUND", DID: "95001",
		Status: "ANSWERED", QueueID: &support, IsContained: false,
	}

	for _, tc := range []struct {
		name   string
		filter WebhookFilter
		cdr    CDR
		want   bool
	}{
		{"the empty filter is every call", WebhookFilter{}, answered, true},
		{"a matching call type", WebhookFilter{CallType: []string{"OUTBOUND"}}, answered, true},
		{"a call type it did not ask for", WebhookFilter{CallType: []string{"INBOUND"}}, answered, false},
		{"one of several call types", WebhookFilter{CallType: []string{"INBOUND", "OUTBOUND"}}, answered, true},
		{"a matching number", WebhookFilter{DID: []string{"95001"}}, answered, true},
		{"another number", WebhookFilter{DID: []string{"95002"}}, answered, false},
		{"a matching status", WebhookFilter{Status: []string{"ANSWERED"}}, answered, true},
		{"a status it did not ask for", WebhookFilter{Status: []string{"NO_ANSWER"}}, answered, false},
		{"a matching queue", WebhookFilter{QueueID: []uuid.UUID{support}}, answered, true},
		{"another queue", WebhookFilter{QueueID: []uuid.UUID{sales}}, answered, false},
		{"isContained false, and it is", WebhookFilter{IsContained: []bool{false}}, answered, true},
		{"isContained true, and it is not", WebhookFilter{IsContained: []bool{true}}, answered, false},

		// Keys are ANDed: a subscription that names two things wants calls
		// that are both, not calls that are either.
		{"both keys match", WebhookFilter{
			CallType: []string{"OUTBOUND"}, Status: []string{"ANSWERED"}}, answered, true},
		{"one key matches and the other does not", WebhookFilter{
			CallType: []string{"OUTBOUND"}, Status: []string{"NO_ANSWER"}}, answered, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(tc.cdr); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// A call that never reached a queue matches no queue filter, and must not
// panic on the way to saying so: QueueID is a pointer and an internal call
// leaves it nil.
func TestACallWithNoQueueMatchesNoQueueFilter(t *testing.T) {
	internal := CDR{CallID: uuid.New(), CallType: "INTERNAL", Status: "ANSWERED"}

	if (WebhookFilter{QueueID: []uuid.UUID{uuid.New()}}).Matches(internal) {
		t.Error("a call that was never in a queue matched a queue filter")
	}
	// And a filter that says nothing about queues still wants it.
	if !(WebhookFilter{CallType: []string{"INTERNAL"}}).Matches(internal) {
		t.Error("a filter that says nothing about queues refused a call that has none")
	}
}
