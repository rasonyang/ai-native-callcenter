// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// A number nobody has created yet is the ordinary case on a fresh deployment:
// after useradd, flowadd is the second and last command before the product can
// answer a call. Before this, it stopped at "no rows" and the operator had to
// reach for the API.
func TestAMissingNumberIsCreatedPointingAtTheFlow(t *testing.T) {
	flowID := uuid.Must(uuid.NewV7())

	plan, err := planDIDBinding("95001", "zh", flowID, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Update != nil {
		t.Fatal("a number that does not exist cannot be updated")
	}
	if plan.Create == nil {
		t.Fatal("a number that does not exist must be created")
	}
	got := *plan.Create
	if got.ID == uuid.Nil {
		t.Error("the new number has no id")
	}
	if got.Number != "95001" {
		t.Errorf("number = %q, want 95001", got.Number)
	}
	if got.Language != "zh" {
		t.Errorf("language = %q, want the one the operator asked for", got.Language)
	}
	if got.FlowID == nil || *got.FlowID != flowID {
		t.Errorf("flowId = %v, want the flow just published (%v)", got.FlowID, flowID)
	}
	// The row the API's own create would write for a body that says nothing
	// but the number and the flow (catalog.NewDID).
	if !got.IsEnabled || !got.AllowInbound || !got.IsRecordingEnabled {
		t.Errorf("a created number must be enabled, inbound and recorded: %+v", got)
	}
	if got.AllowOutbound || got.IsDefaultOutbound {
		t.Errorf("a number created to answer a flow does not dial out: %+v", got)
	}
	if got.FallbackQueueID != nil || got.Description != "" {
		t.Errorf("nothing was said about a fallback queue or a description: %+v", got)
	}
}

// An existing number keeps every column it had; only the flow moves. Writing
// the zero value of an omitted flag here would set both directions to false,
// which the dids_go_somewhere CHECK refuses.
func TestAnExistingNumberOnlyChangesItsFlow(t *testing.T) {
	flowID := uuid.Must(uuid.NewV7())
	queueID := uuid.Must(uuid.NewV7())
	oldFlow := uuid.Must(uuid.NewV7())
	existing := queries.Did{
		ID: uuid.Must(uuid.NewV7()), Number: "95002", Language: "zh",
		FlowID: &oldFlow, FallbackQueueID: &queueID,
		IsRecordingEnabled: false, Description: "support line",
		IsEnabled: true, AllowInbound: true, AllowOutbound: true,
		IsDefaultOutbound: true,
	}

	// -language names en, the default, and the existing number ignores it.
	plan, err := planDIDBinding("95002", "en", flowID, &existing)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Create != nil {
		t.Fatal("a number that exists must not be created again")
	}
	if plan.Update == nil {
		t.Fatal("a number that exists must be updated")
	}
	got := *plan.Update
	if got.ID != existing.ID {
		t.Errorf("id = %v, want the row that is already there (%v)", got.ID, existing.ID)
	}
	if got.FlowID == nil || *got.FlowID != flowID {
		t.Errorf("flowId = %v, want the flow just published (%v)", got.FlowID, flowID)
	}
	// Pointers to the same flow id from two places are equal in meaning, not
	// in address; the check above already compared what they point at.
	got.FlowID = &flowID
	want := queries.UpdateDIDParams{
		ID: existing.ID, Language: existing.Language, FlowID: &flowID,
		FallbackQueueID:    existing.FallbackQueueID,
		IsRecordingEnabled: existing.IsRecordingEnabled,
		Description:        existing.Description,
		IsEnabled:          existing.IsEnabled,
		AllowInbound:       existing.AllowInbound,
		AllowOutbound:      existing.AllowOutbound,
		IsDefaultOutbound:  existing.IsDefaultOutbound,
	}
	if got != want {
		t.Errorf("update = %+v, want every other column carried over: %+v", got, want)
	}
}

// The checks are the ones the HTTP CreateDID operation applies
// (catalog.DID.validate): digits only, and a short language subtag that
// defaults to en.
func TestTheNumberAndLanguageAreCheckedAsTheAPIChecksThem(t *testing.T) {
	flowID := uuid.Must(uuid.NewV7())

	for _, number := range []string{"", "  ", "95-001", "9500a", "+95001"} {
		if _, err := planDIDBinding(number, "en", flowID, nil); err == nil {
			t.Errorf("number %q was accepted; the API requires digits", number)
		}
	}
	if _, err := planDIDBinding("95001", "english-uk", flowID, nil); err == nil {
		t.Error("an eight-character-plus language was accepted; the API refuses it")
	}

	plan, err := planDIDBinding(" 95001 ", "  ", flowID, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Create.Number != "95001" {
		t.Errorf("number = %q, want it trimmed as the API trims it", plan.Create.Number)
	}
	if plan.Create.Language != "en" {
		t.Errorf("language = %q, want the English default", plan.Create.Language)
	}
}
