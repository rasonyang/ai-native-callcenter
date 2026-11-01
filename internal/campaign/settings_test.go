// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// A create announces the campaign exactly once, through the reporter's
// coalescing window; a create that fails announces nothing.
func TestACreateIsAnnouncedOnceAndAFailedCreateNeverIs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	clock := &fakeClock{}
	var mu sync.Mutex
	var got []Snapshot
	rep := NewReporter(f.svc.Get, func(s Snapshot) { mu.Lock(); got = append(got, s); mu.Unlock() }, clock, nil)
	f.svc.WithCreatedHook(rep.Touch)

	bad := f.validNew()
	bad.Name = " "
	if _, err := f.svc.Create(context.Background(), bad); err == nil {
		t.Fatal("an empty name was accepted")
	}
	clock.advance(3 * time.Second)
	if len(got) != 0 {
		t.Fatalf("a failed create announced %+v", got)
	}

	c, err := f.svc.Create(context.Background(), f.validNew())
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(ReportInterval / 2)
	if len(got) != 0 {
		t.Fatalf("announced inside the window: %+v", got)
	}
	clock.advance(2 * time.Second)
	if len(got) != 1 || got[0].CampaignID != c.ID || got[0].Status != StatusDraft || got[0].Contacts != (ContactCounts{}) {
		t.Fatalf("announcements = %+v, want one DRAFT with zero contacts", got)
	}
	clock.advance(10 * time.Second)
	if len(got) != 1 {
		t.Fatalf("announced again with nothing moved: %+v", got)
	}
}

func TestSettingsAreTheValuesTheValidatorsUse(t *testing.T) {
	t.Parallel()
	night, _ := dayrange.Parse("20:00-04:00")
	for _, tc := range []struct {
		name    string
		env     Env
		hours   string
		ringMax int
	}{
		{"defaults", Env{}, "08:00-21:00", 59},
		{"across midnight", Env{OutboundHours: night}, "20:00-04:00", 59},
		{"a short answer window", Env{AnswerWindow: 20 * time.Second}, "08:00-21:00", 19},
		{"a fractional answer window", Env{AnswerWindow: 20500 * time.Millisecond}, "08:00-21:00", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.env)
			s := f.svc.Settings()
			if s.OutboundHours.String() != tc.hours || s.RingTimeoutSec != (IntRange{Min: 5, Max: tc.ringMax}) ||
				s.MaxConcurrent != (IntRange{Min: 1, Max: 100000}) ||
				s.DefaultMaxConcurrent != DefaultMaxConcurrent || s.DefaultRingTimeoutSec != DefaultRingTimeoutSec {
				t.Fatalf("settings = %+v", s)
			}
			// The limits are the validator's edges, not a description of them.
			try := func(ring, conc int) error {
				in := f.validNew()
				if tc.env.OutboundHours != (dayrange.Range{}) {
					in.CallingWindow = CallingWindow{Days: []string{"MON"}, Ranges: []TimeRange{{Start: "22:00", End: "23:00"}}}
				}
				in.RingTimeoutSec, in.MaxConcurrent = &ring, &conc
				_, err := f.svc.Create(context.Background(), in)
				return err
			}
			if err := try(s.RingTimeoutSec.Max, s.MaxConcurrent.Max); err != nil {
				t.Errorf("the advertised maxima were refused: %v", err)
			}
			if err := try(s.RingTimeoutSec.Min, s.MaxConcurrent.Min); err != nil {
				t.Errorf("the advertised minima were refused: %v", err)
			}
			wantKind(t, try(s.RingTimeoutSec.Max+1, 1), ErrRingTimeoutTooLong, "")
			wantKind(t, try(s.RingTimeoutSec.Min-1, 1), ErrValidation, "OUT_OF_RANGE")
			wantKind(t, try(30, s.MaxConcurrent.Max+1), ErrValidation, "OUT_OF_RANGE")
			wantKind(t, try(30, s.MaxConcurrent.Min-1), ErrValidation, "OUT_OF_RANGE")
		})
	}
}

func TestReadyDIDsIsTheCreateRule(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	mk := func(enabled, outbound bool, spec []string, published bool) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		d := DIDFlow{ID: id, Number: id.String()[28:], IsEnabled: enabled, AllowOutbound: outbound}
		if published {
			d.Spec = testSpec(t, spec, nil)
		}
		f.repo.dids[id] = d
		return id
	}
	ids := []uuid.UUID{
		f.did,
		mk(true, true, []string{"hangup", "transfer_to_agent"}, true),
		mk(true, true, []string{"*"}, true),
		mk(true, true, nil, false),
		mk(false, true, []string{"hangup"}, true),
		mk(true, false, []string{"hangup"}, true),
		uuid.Must(uuid.NewV7()), // unknown
	}
	ready, err := f.svc.ReadyDIDs(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:6] {
		in := f.validNew()
		in.DIDID = id
		_, createErr := f.svc.Create(context.Background(), in)
		if ready[id] != (createErr == nil) {
			t.Errorf("DID %s: ready=%v but create err=%v", id, ready[id], createErr)
		}
	}
	if _, has := ready[ids[6]]; has {
		t.Error("an unknown DID got an answer")
	}
}
