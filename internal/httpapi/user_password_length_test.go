// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// #367: a password's minimum is counted in characters (code points), as the
// contract's minLength counts it. The fixtures and helpers (chars, hanChar,
// emojiChar, jsonBody, wantFieldRefused) are in text_length_test.go.

// refusingAccounts stands in for the account store and refuses every write, so
// a password that passed the length check is seen arriving here and nothing
// is stored.
type refusingAccounts struct{ reached bool }

func (a *refusingAccounts) List(context.Context) ([]store.Account, error) { return nil, nil }

func (a *refusingAccounts) Get(context.Context, uuid.UUID) (store.Account, error) {
	a.reached = true
	return store.Account{}, store.ErrNoSuchAccount
}

func (a *refusingAccounts) Provision(context.Context, store.NewAccount) (store.Account, error) {
	a.reached = true
	return store.Account{}, store.ErrUsernameTaken
}

func (a *refusingAccounts) EnsureAgentIdentity(context.Context, uuid.UUID, store.NewAccount) (store.Account, error) {
	a.reached = true
	return store.Account{}, store.ErrNoSuchAccount
}

func (a *refusingAccounts) Update(context.Context, uuid.UUID, store.Changes) (store.Account, error) {
	a.reached = true
	return store.Account{}, store.ErrNoSuchAccount
}

// An account password is at least 8 characters (UserCreate.password and
// PasswordReset.password). Counted in bytes, three Chinese characters passed
// as nine. A password is never trimmed: a space is one of its characters.
func TestAnAccountPasswordIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		password string
		accepted bool
	}{
		{"7 ASCII characters", chars("a", 7), false},
		{"8 ASCII characters", chars("a", 8), true},
		{"3 Chinese characters, 9 bytes", chars(hanChar, 3), false},
		{"7 Chinese characters, 21 bytes", chars(hanChar, 7), false},
		{"8 Chinese characters, 24 bytes", chars(hanChar, 8), true},
		{"7 emoji and Chinese characters, 25 bytes", chars(emojiChar+hanChar, 7), false},
		{"8 emoji and Chinese characters, 28 bytes", chars(emojiChar+hanChar, 8), true},
		{"7 characters and a space", chars(hanChar, 7) + " ", true},
	}
	writes := []struct {
		name string
		do   func(t *testing.T, s *Server, w http.ResponseWriter, password string)
	}{
		{"create an account", func(t *testing.T, s *Server, w http.ResponseWriter, password string) {
			s.CreateUser(w, post(jsonBody(t, map[string]any{
				"username": "novanet.agent", "role": "AGENT", "password": password,
			})))
		}},
		{"reset a password", func(t *testing.T, s *Server, w http.ResponseWriter, password string) {
			s.ResetUserPassword(w, post(jsonBody(t, map[string]any{"password": password})), uuid.New())
		}},
	}
	for _, write := range writes {
		for _, tc := range cases {
			t.Run(write.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				accounts := &refusingAccounts{}
				s := &Server{accounts: accounts, auth: &auth.Service{},
					cfg: config.Config{ExtensionRange: "1000-1999"}}
				rec := httptest.NewRecorder()

				write.do(t, s, rec, tc.password)

				if !tc.accepted {
					wantFieldRefused(t, rec, "password", "PASSWORD_MIN_8")
					if accounts.reached {
						t.Error("a refused password reached the account store")
					}
					return
				}
				if !accounts.reached {
					t.Fatalf("the password was refused: %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
