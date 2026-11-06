// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The trunk gets the form AICC_OUTBOUND_DIAL_FORM names; the contact and the
// attempt keep the canonical number, and a number from another country than
// the campaign's is dialled as E.164 with one warning for the campaign.
func TestTheDialFormChangesOnlyWhatTheTrunkIsSent(t *testing.T) {
	cases := []struct {
		form         phonenum.DialForm
		wantCN       string
		wantForeign  string
		wantWarnings int
	}{
		{"", "+8618688886666", "+14155550123", 0},
		{phonenum.FormE164NoPlus, "8618688886666", "14155550123", 0},
		{phonenum.FormNational, "18688886666", "+14155550123", 1},
	}
	for _, tc := range cases {
		t.Run(string(tc.form), func(t *testing.T) {
			r := newRig(t, opts{})
			buf := &lockedBuf{}
			r.stopDialer()
			r.cfg.DialForm = tc.form
			r.dep.Log = slog.New(slog.NewTextHandler(buf, nil))
			r.startDialer()

			c := r.newCampaign("Form", func(s *camSpec) { s.cc = "86" })
			r.importContacts(c, []contactSpec{
				{number: "+8618688886666"}, {number: "+14155550123"}, {number: "+14155550124"},
			})
			r.start(c)

			if n := r.placer.placedTo(tc.wantCN); n != 1 {
				t.Errorf("placed to %s %d times, want 1 (all: %+v)", tc.wantCN, n, r.placer.placedCalls())
			}
			if n := r.placer.placedTo(tc.wantForeign); n != 1 {
				t.Errorf("placed to %s %d times, want 1", tc.wantForeign, n)
			}
			// The placer is told both: To is what the trunk is sent, CanonicalTo
			// is the stored number the do-not-call list is checked against.
			wantCanonical := map[string]bool{"+8618688886666": true, "+14155550123": true, "+14155550124": true}
			for _, p := range r.placer.placedCalls() {
				if !wantCanonical[p.Req.CanonicalTo] {
					t.Errorf("placed to %s with CanonicalTo %q, want a stored canonical number", p.Req.To, p.Req.CanonicalTo)
				}
				if want, _ := phonenum.DialString(p.Req.CanonicalTo, tc.form, "86"); p.Req.To != want {
					t.Errorf("To = %q for %s, want the dial string %q", p.Req.To, p.Req.CanonicalTo, want)
				}
				delete(wantCanonical, p.Req.CanonicalTo)
			}
			if len(wantCanonical) != 0 {
				t.Errorf("no placement carried CanonicalTo for %v", wantCanonical)
			}
			ct := r.contactOf(c, "+8618688886666")
			if ct.Number != "+8618688886666" {
				t.Errorf("contact number = %s, want canonical", ct.Number)
			}
			for _, a := range r.attempts(c) {
				if !strings.HasPrefix(a.Number, "+") {
					t.Errorf("attempt number = %s, want canonical E.164", a.Number)
				}
			}
			if got := strings.Count(buf.String(), "outside the campaign's country code"); got != tc.wantWarnings {
				t.Errorf("%d warnings, want %d:\n%s", got, tc.wantWarnings, buf.String())
			}
		})
	}
}

// With no country code on the campaign, the deployment's default completes
// the NATIONAL form.
func TestNationalFormFallsBackToTheDeploymentCountryCode(t *testing.T) {
	r := newRig(t, opts{})
	r.stopDialer()
	r.cfg.DialForm = phonenum.FormNational
	r.cfg.DefaultCountryCode = "86"
	r.startDialer()
	c := r.newCampaign("Fallback", nil)
	r.importContacts(c, []contactSpec{{number: "+8618688886666"}})
	r.start(c)
	if r.placer.placedTo("18688886666") != 1 {
		t.Fatalf("calls = %+v, want one to 18688886666", r.placer.placedCalls())
	}
}
