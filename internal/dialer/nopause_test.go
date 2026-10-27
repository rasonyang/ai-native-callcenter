// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// flakyPause is a Store whose PauseFor fails its first failures calls.
type flakyPause struct {
	Store
	failures int32
	calls    atomic.Int32
}

func (f *flakyPause) PauseFor(ctx context.Context, id uuid.UUID, reason campaign.PauseReason) (bool, error) {
	if f.calls.Add(1) <= f.failures {
		return false, errors.New("injected: statement timeout")
	}
	return f.Store.PauseFor(ctx, id, reason)
}

// The refusal voids the attempt, which makes the contact due again. If the pause
// that should stop the campaign does not land (a lock or statement timeout is
// enough), the pass must still stop admitting: a pass that went on would refuse
// the same contacts for ever, hammer the database and starve every other
// campaign and command. The next pass tries the pause again (#293 review).
func TestAPauseThatDoesNotLandStopsThePassAndIsRetriedByTheNext(t *testing.T) {
	r := newRig(t, opts{})
	flaky := &flakyPause{Store: r.dep.Store, failures: 1}
	r.stopDialer()
	r.dep.Store = flaky
	r.startDialer()

	var refused atomic.Int32
	r.placer.setFail(func(outbound.AIDialRequest) error {
		refused.Add(1)
		return outbound.ErrNoOutboundEndpoint
	})
	c := r.newCampaign("Pause fails once", nil)
	r.importContacts(c, numbers("46", 20))
	r.start(c)

	// The first pass: one refusal, one failed pause, and it stops. Left to loop
	// this never returned.
	if n := refused.Load(); n != 1 {
		t.Errorf("%d refusals in the first pass, want 1: the pass kept admitting", n)
	}
	if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
		t.Fatalf("status %s after a failed pause, want RUNNING", got.Status)
	}
	if n := flaky.calls.Load(); n != 1 {
		t.Errorf("%d pause attempts in the first pass, want 1", n)
	}
	if n := len(r.attempts(c)); n != 0 {
		t.Errorf("%d attempt rows, want none", n)
	}

	// A later pass is not held back by the earlier failure: it dials, is refused
	// once more, and this time the pause lands.
	r.tick(stoppedRetry + time.Second)
	got := r.campaignNow(c)
	if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseOutboundNotConfigured {
		t.Fatalf("status %s/%s, want PAUSED/OUTBOUND_NOT_CONFIGURED", got.Status, got.PauseReason)
	}
	if n := refused.Load(); n != 2 {
		t.Errorf("%d refusals in all, want 2", n)
	}
	r.noViolations()
}
