// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type progressStub map[uuid.UUID]Progress

func (p progressStub) Progress(id uuid.UUID) (Progress, bool) { v, ok := p[id]; return v, ok }

type observerStub struct{ updated, imported int }

func (o *observerStub) Updated(Campaign, Campaign) { o.updated++ }
func (o *observerStub) Imported(uuid.UUID)         { o.imported++ }

func TestOnlyARunningCampaignIsAnnotatedWithWhatTheDialerSays(t *testing.T) {
	f := newFixture(t, Env{})
	running := Campaign{ID: uuid.Must(uuid.NewV7()), Status: StatusRunning}
	paused := Campaign{ID: uuid.Must(uuid.NewV7()), Status: StatusPaused}
	f.repo.campaigns[running.ID] = running
	f.repo.campaigns[paused.ID] = paused
	f.svc.WithDialer(progressStub{
		running.ID: {Activity: ActivityDialing, InFlight: 2},
		paused.ID:  {Activity: ActivityDialing, InFlight: 9},
	}, nil)

	got, err := f.svc.Get(context.Background(), running.ID)
	if err != nil || got.Progress.Activity != ActivityDialing || got.Progress.InFlight != 2 {
		t.Errorf("running = %+v, %v", got.Progress, err)
	}
	got, _ = f.svc.Get(context.Background(), paused.ID)
	if got.Progress != (Progress{}) {
		t.Errorf("a paused campaign carries %+v", got.Progress)
	}
}

func TestTheObserverHearsAboutImportsAndPatches(t *testing.T) {
	f := newFixture(t, Env{})
	obs := &observerStub{}
	f.svc.WithDialer(nil, obs)
	c, err := f.svc.Create(context.Background(), f.validNew())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Update(context.Background(), c.ID, Patch{Name: ptr("Renamed")}); err != nil {
		t.Fatal(err)
	}
	if obs.updated != 1 {
		t.Errorf("observer heard %d patches, want 1", obs.updated)
	}
	if _, err := f.svc.ImportContacts(context.Background(), c.ID, []ImportRow{{Number: "+8613800138000"}}); err != nil {
		t.Fatal(err)
	}
	if obs.imported != 1 {
		t.Errorf("observer heard %d imports, want 1", obs.imported)
	}
	// A refused patch is not news.
	if _, err := f.svc.Update(context.Background(), c.ID, Patch{MaxConcurrent: ptr(0)}); err == nil {
		t.Fatal("a zero concurrency was accepted")
	}
	if obs.updated != 1 {
		t.Errorf("observer heard a patch that was refused")
	}
}

func TestListingAttemptsRefusesAnUnknownOutcomeAndAnUnknownCampaign(t *testing.T) {
	f := newFixture(t, Env{})
	if _, _, err := f.svc.ListAttempts(context.Background(), uuid.Must(uuid.NewV7()), AttemptFilter{Outcome: "NOPE"}); !errors.Is(err, ErrValidation) {
		t.Errorf("unknown outcome: %v", err)
	}
	if _, _, err := f.svc.ListAttempts(context.Background(), uuid.Must(uuid.NewV7()), AttemptFilter{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
}

func TestEveryActivityIsValidAndTheEmptyOneIsNot(t *testing.T) {
	for _, a := range []Activity{ActivityDialing, ActivityWaitingWindow, ActivityWaitingRetry, ActivityAtCapacity} {
		if !a.IsValid() {
			t.Errorf("%s should be valid", a)
		}
	}
	if Activity("").IsValid() || Activity("IDLE").IsValid() {
		t.Error("an unknown activity is valid")
	}
}
