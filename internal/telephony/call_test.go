// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

var testTime = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

func TestPartyTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    PartyState
		trigger PartyTrigger
		want    PartyState
		wantErr bool
	}{
		{name: "dialing answers", from: PartyDialing, trigger: TriggerAnswer, want: PartyTalking},
		{name: "dialing releases", from: PartyDialing, trigger: TriggerRelease, want: PartyReleased},
		{name: "ringing answers", from: PartyRinging, trigger: TriggerAnswer, want: PartyTalking},
		{name: "ringing releases", from: PartyRinging, trigger: TriggerRelease, want: PartyReleased},
		{name: "talking holds", from: PartyTalking, trigger: TriggerHold, want: PartyHeld},
		{name: "held retrieves", from: PartyHeld, trigger: TriggerRetrieve, want: PartyTalking},
		{name: "held releases", from: PartyHeld, trigger: TriggerRelease, want: PartyReleased},
		{name: "duplicate answer is absorbed", from: PartyTalking, trigger: TriggerAnswer, want: PartyTalking},

		// Forbidden edges.
		{name: "dialing cannot ring", from: PartyDialing, trigger: TriggerHold, wantErr: true},
		{name: "ringing cannot hold", from: PartyRinging, trigger: TriggerHold, wantErr: true},
		{name: "ringing cannot retrieve", from: PartyRinging, trigger: TriggerRetrieve, wantErr: true},
		{name: "talking cannot retrieve", from: PartyTalking, trigger: TriggerRetrieve, wantErr: true},
		{name: "held cannot hold again", from: PartyHeld, trigger: TriggerHold, wantErr: true},
		{name: "released is terminal for answer", from: PartyReleased, trigger: TriggerAnswer, wantErr: true},
		{name: "released is terminal for release", from: PartyReleased, trigger: TriggerRelease, wantErr: true},
		{name: "released is terminal for hold", from: PartyReleased, trigger: TriggerHold, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Party{State: tt.from}
			err := p.apply(tt.trigger, testTime)

			if tt.wantErr {
				var illegal ErrIllegalTransition
				if !errors.As(err, &illegal) {
					t.Fatalf("apply() error = %v, want ErrIllegalTransition", err)
				}
				if p.State != tt.from {
					t.Errorf("state changed to %s on a rejected transition", p.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("apply() error = %v", err)
			}
			if p.State != tt.want {
				t.Errorf("state = %s, want %s", p.State, tt.want)
			}
		})
	}
}

func TestPartyTimestamps(t *testing.T) {
	p := &Party{State: PartyRinging}
	if err := p.apply(TriggerAnswer, testTime); err != nil {
		t.Fatal(err)
	}
	if !p.AnsweredAt.Equal(testTime) {
		t.Errorf("AnsweredAt = %v, want %v", p.AnsweredAt, testTime)
	}

	// A hold and retrieve must not restamp the answer time.
	later := testTime.Add(time.Minute)
	if err := p.apply(TriggerHold, later); err != nil {
		t.Fatal(err)
	}
	if err := p.apply(TriggerRetrieve, later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !p.AnsweredAt.Equal(testTime) {
		t.Errorf("AnsweredAt moved to %v after hold/retrieve", p.AnsweredAt)
	}

	released := later.Add(time.Minute)
	if err := p.apply(TriggerRelease, released); err != nil {
		t.Fatal(err)
	}
	if !p.ReleasedAt.Equal(released) {
		t.Errorf("ReleasedAt = %v, want %v", p.ReleasedAt, released)
	}
}

func TestFirstPartyIsOriginator(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	if c.State != CallCreated {
		t.Errorf("new call state = %s, want CREATED", c.State)
	}

	first := c.AddParty("chan-a", "+8613800138000", testTime)
	if first.Role != RoleOriginator {
		t.Errorf("first party role = %s, want ORIGINATOR", first.Role)
	}
	if first.State != PartyDialing {
		t.Errorf("first party state = %s, want DIALING", first.State)
	}
	if c.State != CallRunning {
		t.Errorf("call state = %s, want RUNNING once a party exists", c.State)
	}

	second := c.AddParty("chan-b", "1001", testTime)
	if second.Role != RoleTarget {
		t.Errorf("second party role = %s, want TARGET", second.Role)
	}
	if second.State != PartyRinging {
		t.Errorf("second party state = %s, want RINGING", second.State)
	}

	if got := c.Originator(); got != first {
		t.Error("Originator() did not return the first party")
	}
	if got := c.PartyByChannel("chan-b"); got != second {
		t.Error("PartyByChannel() did not find the target leg")
	}
	if c.PartyByChannel("nope") != nil {
		t.Error("PartyByChannel() invented a party")
	}
}

func TestCallTypeIsStampedOnce(t *testing.T) {
	// The caller-perspective type must survive the whole call, including the
	// transfer that replaces every original party.
	c := NewCall(uuid.New(), events.CallTypeOutbound, testTime)
	c.AddParty("chan-a", "+8613800138000", testTime)
	bot := c.AddParty("chan-bot", "aicc-bot", testTime)

	if err := bot.apply(TriggerAnswer, testTime); err != nil {
		t.Fatal(err)
	}
	if err := bot.apply(TriggerRelease, testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	agent := c.AddParty("chan-agent", "1001", testTime.Add(time.Minute))
	if err := agent.apply(TriggerAnswer, testTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if c.CallType != events.CallTypeOutbound {
		t.Errorf("CallType = %s after transfer, want OUTBOUND to persist", c.CallType)
	}
}

func TestAnsweredAtIsTheEarliestAnswer(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	caller := c.AddParty("chan-a", "+86138", testTime)
	bot := c.AddParty("chan-bot", "bot", testTime)

	if !c.AnsweredAt().IsZero() {
		t.Error("AnsweredAt() non-zero before anyone answered")
	}
	if err := bot.apply(TriggerAnswer, testTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := caller.apply(TriggerAnswer, testTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := c.AnsweredAt(); !got.Equal(testTime.Add(time.Second)) {
		t.Errorf("AnsweredAt() = %v, want the earliest answer", got)
	}
}

func TestMergeUserData(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	c.MergeUserData(map[string]any{"ticketId": "T-1", "intent": "billing"})
	c.MergeUserData(map[string]any{"intent": "refund", "customerName": "Wei"})
	c.MergeUserData(map[string]any{"ticketId": nil})

	if _, exists := c.UserData["ticketId"]; exists {
		t.Error("a null patch value did not remove the key")
	}
	if c.UserData["intent"] != "refund" {
		t.Errorf("intent = %v, want refund", c.UserData["intent"])
	}
	if c.UserData["customerName"] != "Wei" {
		t.Errorf("customerName = %v", c.UserData["customerName"])
	}
}

// What a merge reports is what a screen will be told, so it has to be movement
// and not merely a patch having arrived.
//
// The case that matters is the transfer merge: two calls become one and the
// data of the absorbed half is merged into the kept half, which on a
// consultation transfer is very often the same data it already holds. Reported
// as changes, every one of those would announce a change nobody made.
func TestAMergeReportsOnlyWhatMoved(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	c.MergeUserData(map[string]any{"ticketId": "T-1", "intent": "billing"})

	same := c.MergeUserData(map[string]any{"ticketId": "T-1"})
	if !same.IsEmpty() {
		t.Errorf("setting a key to the value it already holds reported %+v", same)
	}
	absent := c.MergeUserData(map[string]any{"neverThere": nil})
	if !absent.IsEmpty() {
		t.Errorf("deleting a key that was not there reported %+v", absent)
	}

	moved := c.MergeUserData(map[string]any{
		"intent":   "refund", // a real replacement
		"orderId":  "A-4471", // an addition
		"ticketId": nil,      // a real deletion
	})
	if !slices.Equal(moved.Changed, []string{"intent", "orderId"}) {
		t.Errorf("Changed = %v, want [intent orderId]", moved.Changed)
	}
	if !slices.Equal(moved.Deleted, []string{"ticketId"}) {
		t.Errorf("Deleted = %v, want [ticketId]", moved.Deleted)
	}
	if len(moved.Dropped) != 0 {
		t.Errorf("Dropped = %v, want none", moved.Dropped)
	}
}

// A value over the bound is refused whole. Truncating would put half an order
// number on an agent's screen with nothing to say the other half existed.
func TestAnOversizeValueIsDroppedAndTheOldOneStands(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	c.MergeUserData(map[string]any{"note": "the short one"})

	got := c.MergeUserData(map[string]any{
		"note":  strings.Repeat("x", UserDataMaxValueBytes+1),
		"other": strings.Repeat("y", UserDataMaxValueBytes),
	})
	if !slices.Equal(got.Dropped, []string{"note"}) {
		t.Errorf("Dropped = %v, want [note]", got.Dropped)
	}
	if c.UserData["note"] != "the short one" {
		t.Errorf("note = %v; a dropped replacement overwrote the value it could not replace", c.UserData["note"])
	}
	// Exactly at the bound is inside it.
	if !slices.Equal(got.Changed, []string{"other"}) {
		t.Errorf("Changed = %v, want [other] — the bound is inclusive", got.Changed)
	}

	// Bytes, not characters: four Chinese characters are twelve bytes.
	c2 := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	overInBytes := strings.Repeat("客", UserDataMaxValueBytes/3+1)
	if len([]rune(overInBytes)) > UserDataMaxValueBytes {
		t.Fatal("the fixture is over the bound in characters too, so it proves nothing")
	}
	if got := c2.MergeUserData(map[string]any{"note": overInBytes}); len(got.Dropped) != 1 {
		t.Errorf("a value inside the bound in characters but over it in bytes was accepted: %+v", got)
	}
}

// Which additions survive a full call has to be the same every run. Left to
// map iteration the same patch against the same call keeps a different pair
// each time, and nobody outside could tell why.
func TestTheKeysThatSurviveAFullCallAreAlwaysTheSameOnes(t *testing.T) {
	fill := func() *Call {
		c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
		seed := map[string]any{}
		for i := range UserDataMaxKeys - 1 {
			seed[fmt.Sprintf("seed%02d", i)] = "v"
		}
		c.MergeUserData(seed)
		return c
	}

	first := fill().MergeUserData(map[string]any{"zulu": "z", "alpha": "a", "mike": "m"})
	for range 20 {
		got := fill().MergeUserData(map[string]any{"zulu": "z", "alpha": "a", "mike": "m"})
		if !slices.Equal(got.Changed, first.Changed) || !slices.Equal(got.Dropped, first.Dropped) {
			t.Fatalf("the same patch kept %v and dropped %v, then %v and %v",
				first.Changed, first.Dropped, got.Changed, got.Dropped)
		}
	}
	if !slices.Equal(first.Changed, []string{"alpha"}) {
		t.Errorf("Changed = %v, want [alpha] — additions go in sorted order", first.Changed)
	}
	if !slices.Equal(first.Dropped, []string{"mike", "zulu"}) {
		t.Errorf("Dropped = %v, want [mike zulu]", first.Dropped)
	}
}

// Deleting is always free, and a key the call already carries is never evicted
// to make room for a new one: data a call has carried since it started is not
// a later patch's to displace.
func TestAFullCallStillTakesADeleteAndAReplacement(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	seed := map[string]any{}
	for i := range UserDataMaxKeys {
		seed[fmt.Sprintf("seed%02d", i)] = "v"
	}
	c.MergeUserData(seed)
	if len(c.UserData) != UserDataMaxKeys {
		t.Fatalf("the fixture holds %d keys, want %d", len(c.UserData), UserDataMaxKeys)
	}

	// Replacing in place needs no room.
	if got := c.MergeUserData(map[string]any{"seed00": "changed"}); !slices.Equal(got.Changed, []string{"seed00"}) {
		t.Errorf("a full call refused a replacement: %+v", got)
	}
	// A patch that frees a key and asks for one nets zero and fits.
	got := c.MergeUserData(map[string]any{"seed01": nil, "orderId": "A-4471"})
	if !slices.Equal(got.Deleted, []string{"seed01"}) || !slices.Equal(got.Changed, []string{"orderId"}) {
		t.Errorf("delete-then-add on a full call reported %+v", got)
	}
	if len(got.Dropped) != 0 {
		t.Errorf("Dropped = %v; deletions run first precisely so this fits", got.Dropped)
	}
}

func TestFinishRequiresAllPartiesReleased(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInbound, testTime)
	a := c.AddParty("chan-a", "+86138", testTime)
	b := c.AddParty("chan-b", "1001", testTime)

	if c.Finish(testTime) {
		t.Error("Finish() succeeded while parties were still up")
	}
	if err := a.apply(TriggerRelease, testTime); err != nil {
		t.Fatal(err)
	}
	if c.Finish(testTime) {
		t.Error("Finish() succeeded with one leg still up")
	}
	if err := b.apply(TriggerRelease, testTime); err != nil {
		t.Fatal(err)
	}

	if !c.Finish(testTime.Add(time.Second)) {
		t.Fatal("Finish() failed once every leg was released")
	}
	if c.State != CallEnded {
		t.Errorf("state = %s, want ENDED", c.State)
	}
	if c.Finish(testTime) {
		t.Error("Finish() reported a second finalization")
	}
}

func TestSnapshotIsADetachedCopy(t *testing.T) {
	c := NewCall(uuid.New(), events.CallTypeInternal, testTime)
	p := c.AddParty("chan-a", "1001", testTime)
	c.MergeUserData(map[string]any{"ticketId": "T-7"})
	if err := p.apply(TriggerAnswer, testTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	snap := c.Snapshot()
	if len(snap.Parties) != 1 || snap.Parties[0].State != PartyTalking {
		t.Fatalf("snapshot parties = %+v", snap.Parties)
	}
	if snap.Parties[0].AnsweredAt == nil {
		t.Error("snapshot lost the answer timestamp")
	}

	// Mutating the snapshot must not reach the live call.
	snap.UserData["ticketId"] = "changed"
	if c.UserData["ticketId"] != "T-7" {
		t.Error("snapshot shares its user data map with the call")
	}
}
