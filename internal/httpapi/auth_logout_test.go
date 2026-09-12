// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/sipsession"
)

// orderedSignOut records the order a web sign-out touches presence and the
// phone credential. Both are written to one slice, because the order between
// them is the behaviour under test and two separate counters could not see it.
type orderedSignOut struct {
	calls     []string
	logoutErr error
	revokeErr error
}

type orderedAgents struct {
	stubAgents
	rec *orderedSignOut
}

func (a orderedAgents) Logout(context.Context, uuid.UUID) (agents.Presence, error) {
	a.rec.calls = append(a.rec.calls, "agents.Logout")
	return agents.Presence{}, a.rec.logoutErr
}

type orderedPhones struct{ rec *orderedSignOut }

func (p orderedPhones) Issue(context.Context, uuid.UUID, time.Time) (sipsession.Issued, error) {
	return sipsession.Issued{}, errors.New("this test never issues")
}

func (p orderedPhones) Revoke(context.Context, uuid.UUID) error {
	p.rec.calls = append(p.rec.calls, "sipSessions.Revoke")
	return p.rec.revokeErr
}

// webLogout signs the given subject out through the real handler.
func webLogout(t *testing.T, ac AuthContext, rec *orderedSignOut) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{
		cfg:         config.Config{SessionCookie: "aicc_session"},
		agents:      orderedAgents{rec: rec},
		sipSessions: orderedPhones{rec: rec},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.Logout(w, r)
	return w
}

// Presence goes out before the phone does, and the order is not cosmetic.
//
// Revoking the credential flushes the registration; the switch answers with
// sofia::unregister; and a READY agent whose phone disappears is moved to
// NOT_READY with reason DEVICE_LOST. That is the right story for a phone that
// died and the wrong one for a person who pressed Sign out — found on a live
// run. Signing presence out first leaves the unregister with nobody READY to
// report on.
func TestWebSignOutEndsPresenceBeforeThePhone(t *testing.T) {
	rec := &orderedSignOut{}
	ac := agentWithSession(time.Now().Add(time.Hour))

	if w := webLogout(t, ac, rec); w.Code != http.StatusNoContent {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}

	want := []string{"agents.Logout", "sipSessions.Revoke"}
	if !slices.Equal(rec.calls, want) {
		t.Errorf("sign-out did %v, want %v — a flush before the presence "+
			"sign-out lands the agent in NOT_READY/DEVICE_LOST", rec.calls, want)
	}
}

// An agent who is already signed out of presence is in the state this asks
// for. It must not stop the phone being revoked, or the credential outlives
// the person.
func TestWebSignOutOfAnAgentWhoIsNotSignedInStillRevokesThePhone(t *testing.T) {
	rec := &orderedSignOut{logoutErr: agents.ErrNotLoggedIn}
	ac := agentWithSession(time.Now().Add(time.Hour))

	if w := webLogout(t, ac, rec); w.Code != http.StatusNoContent {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if !slices.Contains(rec.calls, "sipSessions.Revoke") {
		t.Errorf("sign-out did %v; an already-signed-out agent kept their phone credential", rec.calls)
	}
}

// Neither half may keep somebody signed in. A person who cannot sign out of
// the page because a switch or a table is unreachable is worse than either
// state left behind, both of which expire on their own.
func TestWebSignOutSucceedsWhenEitherHalfFails(t *testing.T) {
	ac := agentWithSession(time.Now().Add(time.Hour))

	for name, rec := range map[string]*orderedSignOut{
		"presence fails": {logoutErr: errors.New("switch down")},
		"revoke fails":   {revokeErr: errors.New("storage down")},
		"both fail":      {logoutErr: errors.New("switch down"), revokeErr: errors.New("storage down")},
	} {
		t.Run(name, func(t *testing.T) {
			if w := webLogout(t, ac, rec); w.Code != http.StatusNoContent {
				t.Fatalf("http = %d: %s", w.Code, w.Body)
			}
			// And the second half is still attempted after the first failed.
			if !slices.Contains(rec.calls, "sipSessions.Revoke") {
				t.Errorf("sign-out stopped at %v", rec.calls)
			}
		})
	}
}

// A subject with no agent identity has no presence and no phone to end.
func TestWebSignOutOfANonAgentTouchesNeither(t *testing.T) {
	rec := &orderedSignOut{}
	supervisor := AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(), SubjectName: "priya",
		SessionExpiresAt: time.Now().Add(time.Hour),
	}

	if w := webLogout(t, supervisor, rec); w.Code != http.StatusNoContent {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if len(rec.calls) != 0 {
		t.Errorf("a subject with no agent identity caused %v", rec.calls)
	}
}
