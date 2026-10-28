// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// The values a language must be, and some it must not: wrong case, the wrong
// separator, syntax the switch would read, a name rather than a tag, too long,
// too short, and a line break.
var (
	acceptedLanguages = []string{"en", "zh", "pt-br", "zh-hans"}
	refusedLanguages  = []string{
		"EN", "zh_CN", "en^hangup", "en'", "zh,x=y", "zh hans", "中文", "zh-hans-cn", "e",
		"zh-hans-x", // nine characters
		"en\nzh", "en\n", " en", "en$", "${x}",
	}
)

// errorOf reads the error envelope's code and params.
func errorOf(t *testing.T, body []byte) (string, map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	return env.Error.Code, env.Error.Params
}

func callBody(t *testing.T, kind, language string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"kind": kind, "to": "18600000000", "language": language})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A call's language overrides the number's and rides the call to the switch
// and to the bot. It is a lowercase BCP 47 tag of at most eight characters,
// and anything else is refused before a call is placed.
func TestACallsLanguageIsALanguageTag(t *testing.T) {
	t.Parallel()
	for _, language := range acceptedLanguages {
		w, dialer := placeCall(t, callBody(t, "AI_OUTBOUND", language))
		if w.Code != http.StatusCreated {
			t.Errorf("language %q: http = %d: %s", language, w.Code, w.Body)
		}
		if dialer.got.Language != language {
			t.Errorf("language %q reached the dialer as %q", language, dialer.got.Language)
		}
	}

	// Empty or absent: the number's own language, as before.
	for _, body := range []string{
		callBody(t, "AI_OUTBOUND", ""),
		`{"kind":"AI_OUTBOUND","to":"18600000000"}`,
	} {
		w, dialer := placeCall(t, body)
		if w.Code != http.StatusCreated {
			t.Errorf("%s: http = %d: %s", body, w.Code, w.Body)
		}
		if dialer.got.Language != "" {
			t.Errorf("%s: language %q, want the DID's (empty)", body, dialer.got.Language)
		}
	}

	for _, kind := range []string{"AI_OUTBOUND", "AGENT_OUTBOUND"} {
		for _, language := range refusedLanguages {
			w, dialer := placeCall(t, callBody(t, kind, language))
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s language %q: http = %d, want 422: %s", kind, language, w.Code, w.Body)
				continue
			}
			code, params := errorOf(t, w.Body.Bytes())
			if code != string(CodeValidationFailed) || params["field"] != "language" ||
				params["rule"] != "LANGUAGE_SUBTAG" {
				t.Errorf("%s language %q: %s %v, want VALIDATION_FAILED language/LANGUAGE_SUBTAG",
					kind, language, code, params)
			}
			if dialer.got.To != "" {
				t.Errorf("%s language %q was dialled", kind, language)
			}
		}
	}
}

// refusingOutbound answers every AI dial with one error.
type refusingOutbound struct {
	stubOutbound
	err error
}

func (o refusingOutbound) DialAI(context.Context, outbound.AIDialRequest) (uuid.UUID, error) {
	return uuid.Nil, o.err
}

// A number whose stored language the switch could not carry refuses the call
// before anybody is rung, and says the number is what to fix.
func TestANumbersUnsafeStoredLanguageIsAConflict(t *testing.T) {
	t.Parallel()
	s := &Server{outbound: refusingOutbound{err: outbound.ErrBadLanguage}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls",
		strings.NewReader(`{"kind":"AI_OUTBOUND","to":"18600000000","did":"95011"}`))
	r = r.WithContext(contextWithIdentity(r.Context(), auth.Identity{UserID: uuid.New(), Role: auth.RoleSupervisor}))
	s.CreateCall(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("http = %d, want 409: %s", w.Code, w.Body)
	}
	if code, _ := errorOf(t, w.Body.Bytes()); code != string(CodeConflict) {
		t.Errorf("code = %q, want CONFLICT", code)
	}
}

// A transfer's destination becomes the switch's dialplan destination: an
// extension or a number, or a 422 naming the field.
func TestATransferToAnythingButAnExtensionOrANumberIsRefused(t *testing.T) {
	t.Parallel()
	agent := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), AgentID: uuid.New(),
		scopes: grantedScopes(auth.RoleAgent)}
	srv := &Server{calls: failingCalls{err: telephony.ErrInvalidDestination}, agents: dialerPresence{}}
	callID := uuid.New()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/transfer",
		strings.NewReader(`{"destination":"system:id inline"}`))
	r = r.WithContext(contextWithAuth(r.Context(), agent))
	w := httptest.NewRecorder()
	srv.TransferCall(w, r, callID)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("http = %d, want 422: %s", w.Code, w.Body)
	}
	code, params := errorOf(t, w.Body.Bytes())
	if code != string(CodeValidationFailed) || params["field"] != "destination" || params["rule"] != "DESTINATION_FORMAT" {
		t.Errorf("%s %v, want VALIDATION_FAILED destination/DESTINATION_FORMAT", code, params)
	}
}

// The same rule for a number's language, through the real catalog service
// and the real database, on create and on update.
func TestADIDsLanguageIsALanguageTagOverTheAPI(t *testing.T) {
	f := newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		d.Catalog = catalog.NewService(st.Catalog(), nil, st.Catalog())
	})
	admin := f.seedAdmin()

	number := 95300
	for _, language := range acceptedLanguages {
		number++
		r := f.do(call{method: http.MethodPost, path: "/dids", cookie: admin, body: map[string]any{
			"number": strconv.Itoa(number), "language": language,
			"allowInbound": false, "allowOutbound": true,
		}})
		if r.status != http.StatusCreated {
			t.Errorf("language %q: %d %s", language, r.status, r.body)
			continue
		}
		var got struct {
			Language string `json:"language"`
		}
		_ = json.Unmarshal(r.body, &got)
		if got.Language != language {
			t.Errorf("language %q was stored as %q", language, got.Language)
		}
	}

	r := f.do(call{method: http.MethodPost, path: "/dids", cookie: admin, body: map[string]any{
		"number": "95399", "allowInbound": false, "allowOutbound": true,
	}})
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	var created struct {
		ID       string `json:"id"`
		Language string `json:"language"`
	}
	_ = json.Unmarshal(r.body, &created)
	if created.Language != "en" {
		t.Errorf("an absent language was stored as %q, want en", created.Language)
	}

	for _, language := range refusedLanguages {
		for _, write := range []call{
			{method: http.MethodPost, path: "/dids", cookie: admin, body: map[string]any{
				"number": "95398", "language": language, "allowInbound": false, "allowOutbound": true,
			}},
			{method: http.MethodPut, path: "/dids/" + created.ID, cookie: admin, body: map[string]any{
				"number": "95399", "language": language, "allowInbound": false, "allowOutbound": true,
			}},
		} {
			r := f.do(write)
			if r.status != http.StatusUnprocessableEntity {
				t.Errorf("%s language %q: %d, want 422: %s", write.method, language, r.status, r.body)
				continue
			}
			code, params := errorOf(t, r.body)
			if code != string(CodeValidationFailed) || params["field"] != "language" ||
				params["rule"] != "LANGUAGE_SUBTAG" {
				t.Errorf("%s language %q: %s %v", write.method, language, code, params)
			}
		}
	}
}
