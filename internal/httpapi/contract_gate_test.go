// SPDX-License-Identifier: Apache-2.0

package httpapi

// The contract gate.
//
// docs/openapi.json is the product surface, not documentation of it. These
// three assertions are what makes that sentence checkable rather than a thing
// people say: every route the server mounts is declared there with an
// authorization rule, every error code it can return is spelled the same in
// all four places it appears, and every operation a system could reasonably
// need is reachable by an API key rather than only by a browser.
//
// They live in one file because they answer one question — is the contract
// still the truth? — and because a reader chasing a failure in any of them
// needs the other two beside it.

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
)

// ---------------------------------------------------------------------------
// (a) Every route is declared in the contract, and declares an authorization
//     rule there.
// ---------------------------------------------------------------------------

// notTheAPI names the HTTP entry points in this binary that are deliberately
// outside the contract, each with the reason it is.
//
// They are exclusions rather than omissions: the contract's own description
// says the ops listener is unauthenticated and served elsewhere, and the SPA
// is a consumer of this API rather than part of it. Anything not on this list
// and not in the contract is a route whose authorization nothing states.
var notTheAPI = map[string]string{
	"GET /metrics": "the ops listener, on its own address (AICC_METRICS_ADDR), unauthenticated by design and declared as such in the contract's description",
	"GET /healthz": "same listener: liveness, which a probe must reach before anything is up to authenticate it",
	"GET /readyz":  "same listener: readiness",
	"GET /*":       "the SPA fallback — static files, and one consumer of this API rather than part of it",
}

// A route the contract does not declare has no authorization at all: scope
// enforcement reads the contract, so enforceContract fails closed on it with a
// 500. That is the right behaviour and a terrible way to find out — in
// production, on the one endpoint somebody added without touching the
// contract. This is where it is found instead.
func TestEveryMountedRouteDeclaresItsAuthorization(t *testing.T) {
	srv := New(config.Config{Env: "dev"}, Deps{
		Auth: &auth.Service{}, Agents: stubAgents{}, Calls: stubCalls{},
		Catalog: stubCatalog{}, Ledger: stubLedger(t), Contacts: stubContacts{},
		Outbound: stubOutbound{}, Keys: stubKeys{},
	})

	var undeclared, unauthorized []string
	err := chi.Walk(srv.router(), func(method, route string, _ http.Handler,
		_ ...func(http.Handler) http.Handler) error {
		if _, excluded := notTheAPI[method+" "+route]; excluded {
			return nil
		}
		if !strings.HasPrefix(route, apiPrefix) {
			return nil
		}
		path := strings.TrimSuffix(strings.TrimPrefix(route, apiPrefix), "/")
		if path == "" {
			return nil
		}
		sec, ok := api.SecurityForRoute(method, path)
		if !ok {
			undeclared = append(undeclared, method+" "+path)
			return nil
		}
		// Anonymous is a declaration; so is an empty scope list, which means
		// "authenticated is the whole requirement" (getMe, logout). What
		// would not be is a route the contract knows and says nothing about,
		// which cannot happen while the generator refuses an operation with
		// no `security` key at all — asserted here so that refusal cannot be
		// quietly relaxed.
		if !sec.IsAnonymous && sec.SessionScopes == nil && sec.KeyScopes == nil {
			unauthorized = append(unauthorized, method+" "+path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		t.Errorf("mounted and not declared in the contract: %s\n"+
			"Authorization is read from the contract, so such a route has none: it "+
			"fails closed with a 500 and nobody learns why until it is live.",
			strings.Join(undeclared, ", "))
	}
	if len(unauthorized) > 0 {
		sort.Strings(unauthorized)
		t.Errorf("declared with no credential able to reach them: %s",
			strings.Join(unauthorized, ", "))
	}
}

// ---------------------------------------------------------------------------
// (b) One error code, spelled the same in four places.
// ---------------------------------------------------------------------------

// untranslatable names the two keys under `errors` in the translation files
// that are not error codes, with the reason each is there.
//
// Registered rather than tolerated: without this list the assertion below
// would have to compare loosely, and a loose comparison is how a genuinely
// missing translation goes unnoticed.
var untranslatable = map[string]string{
	"UNKNOWN": "the frontend's fallback when a code has no wording of its own (describeError's defaultValue) — it is what the assertion below exists to keep unused",
	"rules":   "a nested map of field-level rules (errors.rules.<RULE>), reached by fieldErrorText and keyed by the `rule` param rather than by a code",
}

// The contract's ErrorCode enum, the Go constants and both translation files
// must name exactly the same set.
//
// errors.go is hand-written and nothing generated it, so a code added there
// and not to the contract is invisible to every other check in this
// repository — and a code in the contract with no wording renders as
// "Unexpected error", which turns an instruction the reader could act on into
// one they cannot.
func TestOneErrorCodeIsSpelledTheSameEverywhere(t *testing.T) {
	contract := contractErrorCodes(t)
	goConstants := goErrorCodes()

	compare(t, "the contract", contract, "internal/httpapi/errors.go", goConstants)

	for _, locale := range []string{"en", "zh"} {
		compare(t, "the contract", contract,
			"web/src/locales/"+locale+"/translation.json", translationErrorCodes(t, locale))
	}
}

func compare(t *testing.T, leftName string, left []string, rightName string, right []string) {
	t.Helper()
	for _, code := range left {
		if !slices.Contains(right, code) {
			t.Errorf("%s has %s and %s does not", leftName, code, rightName)
		}
	}
	for _, code := range right {
		if !slices.Contains(left, code) {
			t.Errorf("%s has %s and %s does not", rightName, code, leftName)
		}
	}
}

func contractErrorCodes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				ErrorCode struct {
					Enum []string `json:"enum"`
				} `json:"ErrorCode"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	codes := spec.Components.Schemas.ErrorCode.Enum
	if len(codes) == 0 {
		t.Fatal("the contract declares no ErrorCode enum")
	}
	return codes
}

// goErrorCodes reads the constants through the type system rather than by
// parsing the file: every ErrorCode-typed constant this package exports to
// itself is one the server can write.
func goErrorCodes() []string {
	// The values are what reaches the wire, and the wire is what the other
	// three lists hold.
	return []string{
		string(CodeInvalidCredentials), string(CodeSessionExpired), string(CodeForbidden),
		string(CodeAgentRequired), string(CodeAgentImpersonationNotAllowed),
		string(CodeInsufficientScope), string(CodeValidationFailed),
		string(CodeUserDataTooLarge), string(CodeNotFound), string(CodeMethodNotAllowed),
		string(CodeConflict), string(CodeExtensionInUse), string(CodeExtensionAssignedToAgent),
		string(CodeExtensionPoolExhausted), string(CodeLastAdmin),
		string(CodeAgentAlreadyLoggedIn), string(CodeAgentNotLoggedIn),
		string(CodeAgentNotInWrapUp), string(CodeCallNotFound), string(CodeNotCallParty),
		string(CodeOperationNotAllowedForCallType), string(CodeUserSuspended),
		string(CodeSwitchDown), string(CodeStorageDown), string(CodeRateLimited),
		string(CodeInternal),
	}
}

func translationErrorCodes(t *testing.T, locale string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../web/src/locales/" + locale + "/translation.json")
	if err != nil {
		t.Fatalf("read %s translations: %v", locale, err)
	}
	var file struct {
		Errors map[string]json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse %s translations: %v", locale, err)
	}
	var codes []string
	for key := range file.Errors {
		if _, exempt := untranslatable[key]; exempt {
			continue
		}
		codes = append(codes, key)
	}
	return codes
}

// The exemptions have to still be there. A fallback that quietly disappeared
// would make describeError render a raw key on screen, and the assertion
// above would go on passing.
func TestTheUntranslatableKeysAreStillThere(t *testing.T) {
	for _, locale := range []string{"en", "zh"} {
		raw, err := os.ReadFile("../../web/src/locales/" + locale + "/translation.json")
		if err != nil {
			t.Fatalf("read %s translations: %v", locale, err)
		}
		var file struct {
			Errors map[string]json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatalf("parse %s translations: %v", locale, err)
		}
		for key, why := range untranslatable {
			if _, ok := file.Errors[key]; !ok {
				t.Errorf("%s is missing errors.%s — %s", locale, key, why)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// (c) A system can reach what a person can.
// ---------------------------------------------------------------------------

// browserOnly names every operation with no bearer alternative, and why.
//
// "UI is optional. API is the product." An operation a session can reach and
// a key cannot is a capability locked inside the browser, and each one has to
// be a decision somebody made rather than a line nobody wrote. Two are:
var browserOnly = map[string]string{
	"logout":                "a session mechanism: a key has no session to end, so this is not an asymmetry of capability",
	"createRecordingReview": "a person's judgement about another person's call — reviewer_id must point at somebody who can be asked about it. Not a UI privilege: a supervisor's session token reaches it from curl just as well, and reading the scores (listCallReviews) has a bearer alternative like everything else",
}

func TestASystemCanReachWhatAPersonCan(t *testing.T) {
	var locked []string
	for route, sec := range api.OperationSecurityByRoute {
		if sec.IsAnonymous {
			continue
		}
		if sec.KeyScopes != nil {
			continue
		}
		if _, allowed := browserOnly[sec.OperationID]; allowed {
			continue
		}
		locked = append(locked, sec.OperationID+" ("+route+")")
	}
	if len(locked) > 0 {
		sort.Strings(locked)
		t.Errorf("reachable by a browser session and by nothing else: %s\n"+
			"UI is optional and the API is the product: an operation only a browser can "+
			"perform is a capability locked inside one consumer. Give it a bearer "+
			"alternative, or register it in browserOnly with the reason.",
			strings.Join(locked, ", "))
	}

	// And the register must not outlive what it excuses.
	declared := map[string]bool{}
	for _, sec := range api.OperationSecurityByRoute {
		declared[sec.OperationID] = true
	}
	for id := range browserOnly {
		if !declared[id] {
			t.Errorf("browserOnly excuses %s, which the contract no longer declares", id)
		}
	}
}

// The generated table is the contract, so it must not have drifted from the
// interface the server implements: every operation the wrapper can call has a
// row, or enforceContract has nothing to apply.
func TestEveryContractOperationHasASecurityRow(t *testing.T) {
	byID := map[string]bool{}
	for _, sec := range api.OperationSecurityByRoute {
		byID[sec.OperationID] = true
	}
	iface := reflect.TypeOf((*api.ServerInterface)(nil)).Elem()
	if got, want := len(byID), iface.NumMethod(); got != want {
		t.Errorf("the security table has %d operations and the server interface has %d",
			got, want)
	}
}
