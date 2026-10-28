// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
)

// #370: a limit the contract states, on a value the server stores in a bounded
// column, is checked by the server and refused with 422 naming the field.
// Before, the column was the only bound and a breach came back as a 5xx
// STORAGE_DOWN, which tells an integrator the platform is down and to retry.
// Worse, an API key name of 121 to 128 characters was valid by the contract
// and could never be stored.
//
// Both run through the real routes against a real database, so the column
// itself shows that nothing accepted is too wide for it. The fixtures (chars,
// hanChar, emojiChar) are in text_length_test.go; the account helpers
// (createUser, putUser, storedLocale, decodeUser) in user_locale_test.go.

// wantReplyRefused is wantFieldRefused for a reply from the running server.
func wantReplyRefused(t *testing.T, label string, got reply, field, rule string) {
	t.Helper()
	if got.status != http.StatusUnprocessableEntity || got.code != string(CodeValidationFailed) {
		t.Errorf("%s: status %d (%s), want 422 VALIDATION_FAILED: %.200s", label, got.status, got.code, got.body)
		return
	}
	var env struct {
		Error struct {
			Params map[string]any `json:"params"`
		} `json:"error"`
	}
	if err := json.Unmarshal(got.body, &env); err != nil {
		t.Fatalf("%s: decode the refusal: %v", label, err)
	}
	if got := env.Error.Params["field"]; got != field {
		t.Errorf("%s: params.field = %v, want %q", label, got, field)
	}
	if got := env.Error.Params["rule"]; got != rule {
		t.Errorf("%s: params.rule = %v, want %q", label, got, rule)
	}
}

// wantNo5xx holds the issue's first acceptance line on every reply.
func wantNo5xx(t *testing.T, label string, got reply) {
	t.Helper()
	if got.status >= 500 {
		t.Errorf("%s: status %d (%s): a request's own input answered as a server failure: %.200s",
			label, got.status, got.code, got.body)
	}
}

// An API key's name is at most 128 characters (APIKeyWrite.name and
// APIKeyUpdate.name), and the column now holds 128.
func TestAnAPIKeyNameIsCheckedAgainstTheContractsLimit(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()

	cases := []struct {
		name     string
		keyName  string
		accepted bool
	}{
		{"120 ASCII characters", chars("n", 120), true},
		{"128 ASCII characters", chars("n", 128), true},
		{"129 ASCII characters", chars("n", 129), false},
		{"128 Chinese characters, 384 bytes", chars(hanChar, 128), true},
		{"129 Chinese characters", chars(hanChar, 129), false},
		{"128 emoji and Chinese characters, 448 bytes", chars(emojiChar+hanChar, 128), true},
		{"129 emoji and Chinese characters", chars(emojiChar+hanChar, 129), false},
		{"whitespace around 128 characters is trimmed before counting", "  " + chars(hanChar, 128) + "\t", true},
		{"whitespace around 129 characters does not hide the extra one", "  " + chars(hanChar, 129) + "\t", false},
	}
	storedName := func(id string) string {
		t.Helper()
		var name string
		if err := f.st.Pool.QueryRow(context.Background(),
			`SELECT name FROM api_keys WHERE id = $1`, id).Scan(&name); err != nil {
			t.Fatalf("read key %s: %v", id, err)
		}
		return name
	}
	keyCount := func() int {
		t.Helper()
		var n int
		if err := f.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys`).Scan(&n); err != nil {
			t.Fatalf("count keys: %v", err)
		}
		return n
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	// Issuing a key.
	for _, tc := range cases {
		label := "create/" + tc.name
		before := keyCount()
		got := f.do(call{method: http.MethodPost, path: "/api-keys", cookie: admin,
			body: map[string]any{"name": tc.keyName, "scopes": []string{api.ScopeCallsReadAll}}})
		wantNo5xx(t, label, got)
		if !tc.accepted {
			wantReplyRefused(t, label, got, "name", "NAME_TOO_LONG")
			if after := keyCount(); after != before {
				t.Errorf("%s: a refused name issued a key (%d keys, were %d)", label, after, before)
			}
			continue
		}
		if got.status != http.StatusCreated {
			t.Errorf("%s: status %d (%s), want 201: %.200s", label, got.status, got.code, got.body)
			continue
		}
		if err := json.Unmarshal(got.body, &created); err != nil {
			t.Fatalf("%s: decode the key: %v", label, err)
		}
		want := strings.TrimSpace(tc.keyName)
		if created.Name != want {
			t.Errorf("%s: answered a name of %d characters, want %d", label, len([]rune(created.Name)), len([]rune(want)))
		}
		if stored := storedName(created.ID); stored != want {
			t.Errorf("%s: stored a name of %d characters, want %d", label, len([]rune(stored)), len([]rune(want)))
		}
	}

	// Renaming one.
	got := f.do(call{method: http.MethodPost, path: "/api-keys", cookie: admin,
		body: map[string]any{"name": "NovaNet CRM sync", "scopes": []string{api.ScopeCallsReadAll}}})
	if got.status != http.StatusCreated {
		t.Fatalf("issue the key to rename: status %d (%s): %.200s", got.status, got.code, got.body)
	}
	if err := json.Unmarshal(got.body, &created); err != nil {
		t.Fatalf("decode the key to rename: %v", err)
	}
	keyID := created.ID
	for _, tc := range cases {
		label := "update/" + tc.name
		before := storedName(keyID)
		got := f.do(call{method: http.MethodPatch, path: "/api-keys/" + keyID, cookie: admin,
			body: map[string]any{"name": tc.keyName}})
		wantNo5xx(t, label, got)
		if !tc.accepted {
			wantReplyRefused(t, label, got, "name", "NAME_TOO_LONG")
			if after := storedName(keyID); after != before {
				t.Errorf("%s: a refused rename changed the name from %d to %d characters",
					label, len([]rune(before)), len([]rune(after)))
			}
			continue
		}
		if got.status != http.StatusOK {
			t.Errorf("%s: status %d (%s), want 200: %.200s", label, got.status, got.code, got.body)
			continue
		}
		want := strings.TrimSpace(tc.keyName)
		if stored := storedName(keyID); stored != want {
			t.Errorf("%s: stored a name of %d characters, want %d", label, len([]rune(stored)), len([]rune(want)))
		}
	}
}

// A user's locale is at most 8 characters (UserCreate.locale and
// UserUpdate.locale), the width of users.locale. The contract names no set of
// locales and no pattern, so any 8 characters are taken, mixed case included
// (#289 stores zh-CN as sent).
func TestAUserLocaleIsCheckedAgainstTheContractsLimit(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1199")
	admin := f.seedAdmin()

	cases := []struct {
		name     string
		locale   string
		accepted bool
	}{
		{"zh", "zh", true},
		{"en", "en", true},
		{"zh-hans", "zh-hans", true},
		{"8 characters", "zh-Hans1", true},
		{"8 Chinese characters, 24 bytes", chars(hanChar, 8), true},
		{"9 characters", "zh-Hans-C", false},
		{"zh-Hans-CN-x", "zh-Hans-CN-x", false},
		{"zh-Hans-CN-x-private", "zh-Hans-CN-x-private", false},
		{"9 Chinese characters", chars(hanChar, 9), false},
	}
	accountExists := func(username string) bool {
		t.Helper()
		var n int
		if err := f.st.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM users WHERE username = $1`, username).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", username, err)
		}
		return n > 0
	}

	// Creating an account.
	for i, tc := range cases {
		label := "create/" + tc.name
		username := fmt.Sprintf("novanet.locale-%02d", i)
		got := f.do(call{method: http.MethodPost, path: "/users", cookie: admin,
			body: map[string]any{"username": username, "role": "AGENT", "password": seedPassword, "locale": tc.locale}})
		wantNo5xx(t, label, got)
		if !tc.accepted {
			wantReplyRefused(t, label, got, "locale", "LOCALE_TOO_LONG")
			if accountExists(username) {
				t.Errorf("%s: a refused locale created the account", label)
			}
			continue
		}
		if got.status != http.StatusCreated {
			t.Errorf("%s: status %d (%s), want 201: %.200s", label, got.status, got.code, got.body)
			continue
		}
		u := decodeUser(t, got.body)
		if *u.Locale != tc.locale {
			t.Errorf("%s: answered locale %q, want %q", label, *u.Locale, tc.locale)
		}
		if stored := f.storedLocale(u.UserID); !sameLocale(stored, &tc.locale) {
			t.Errorf("%s: stored locale %s, want %q", label, showLocale(stored), tc.locale)
		}
	}

	// Editing one, sent back whole as read with only the locale changed.
	account := f.createUser(admin, "novanet.li-na", "AGENT", strp("en"))
	for _, tc := range cases {
		label := "update/" + tc.name
		before := f.storedLocale(account.UserID)
		u := account
		u.Locale = strp(tc.locale)
		got := f.putUser(admin, u)
		wantNo5xx(t, label, got)
		if !tc.accepted {
			wantReplyRefused(t, label, got, "locale", "LOCALE_TOO_LONG")
			if after := f.storedLocale(account.UserID); !sameLocale(after, before) {
				t.Errorf("%s: a refused locale changed it from %s to %s", label, showLocale(before), showLocale(after))
			}
			continue
		}
		if got.status != http.StatusOK {
			t.Errorf("%s: status %d (%s), want 200: %.200s", label, got.status, got.code, got.body)
			continue
		}
		if stored := f.storedLocale(account.UserID); !sameLocale(stored, &tc.locale) {
			t.Errorf("%s: stored locale %s, want %q", label, showLocale(stored), tc.locale)
		}
	}
}
