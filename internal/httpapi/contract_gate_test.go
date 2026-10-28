// SPDX-License-Identifier: Apache-2.0

package httpapi

// The contract gate.
//
// docs/openapi.json is the product surface, not documentation of it. These
// assertions are what makes that sentence checkable rather than a thing people
// say: every route the server mounts is declared there with an authorization
// rule, every error code it can return is spelled the same in all four places
// it appears, every operation a system could reasonably need is reachable by
// an API key rather than only by a browser, and every status a call operation
// can answer is one that operation declares.
//
// They live in one file because they answer one question — is the contract
// still the truth? — and because a reader chasing a failure in any of them
// needs the others beside it.

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
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
	t.Parallel()
	srv := New(config.Config{Env: "dev"}, Deps{
		Auth: &auth.Service{}, Agents: stubAgents{}, Calls: stubCalls{},
		Catalog: stubCatalog{}, Ledger: stubLedger(t), Contacts: stubContacts{}, DNC: stubDNC{}, Campaigns: stubCampaigns{},
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
	t.Parallel()
	contract := contractErrorCodes(t)
	goConstants := goErrorCodes(t)

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

// readContract is docs/openapi.json as it is on disk now, which is what the
// next generation and the next release will be built from.
func readContract(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	return raw
}

func contractErrorCodes(t *testing.T) []string {
	t.Helper()
	raw := readContract(t)
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

// goErrorCodes reads every ErrorCode constant out of errors.go, by parsing it.
//
// Parsed rather than listed here. A hand-copied list would be a third place to
// forget a code, and it would fail in exactly the way this assertion exists to
// prevent: add CodeQuotaExceeded to errors.go, use it in a handler, forget the
// contract — the contract does not have it, this list does not have it, the
// translations do not have it, and every comparison passes while the wire
// emits a code the contract does not declare and the screen says "Unexpected
// error". errors.go is hand-written with no generator behind it, which is the
// whole reason ruling 4 asked for this assertion.
//
// The parsing approach is the one routes_test.go already uses on server.go.
func goErrorCodes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "errors.go", nil, 0)
	if err != nil {
		t.Fatalf("parse errors.go: %v", err)
	}

	var codes []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Only the block that declares ErrorCode values: the type is written
		// once, on the first spec, and carries down the block.
		isErrorCode := false
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); ok {
				isErrorCode = ident.Name == "ErrorCode"
			}
			if !isErrorCode {
				continue
			}
			for _, v := range value.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				codes = append(codes, unquoted)
			}
		}
	}
	if len(codes) == 0 {
		t.Fatal("errors.go declares no ErrorCode constants; the parse found nothing")
	}
	return codes
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// ---------------------------------------------------------------------------
// (d) Every status a call operation answers is one it declares.
// ---------------------------------------------------------------------------

// callFailures is every failure callResult gives an answer of its own, spelled
// as its switch spells it. TestCallResultsBranchesAreTheOnesTheGateDrives
// holds this list to that switch, so a branch added there and not here fails
// rather than going undriven.
var callFailures = map[string]error{
	"telephony.ErrCallNotFound":       telephony.ErrCallNotFound,
	"telephony.ErrNoAgentLeg":         telephony.ErrNoAgentLeg,
	"telephony.ErrNotCallParty":       telephony.ErrNotCallParty,
	"telephony.ErrNoExtensionLeg":     telephony.ErrNoExtensionLeg,
	"telephony.ErrNotForCallType":     telephony.ErrNotForCallType,
	"telephony.ErrInvalidDTMF":        telephony.ErrInvalidDTMF,
	"telephony.ErrInvalidDestination": telephony.ErrInvalidDestination,
	"esl.ErrDown":                     esl.ErrDown,
	"esl.ErrTimeout":                  esl.ErrTimeout,
	"telephony.ErrTransferInFlight":   telephony.ErrTransferInFlight,
}

// Every operation can also succeed, find the switch unreachable, go
// unanswered by a stalled switch, or have it refuse the command for a reason
// of its own (callResult's default branch).
var everyCallOutcome = []string{"", "esl.ErrDown", "esl.ErrTimeout", "the switch refused"}

// callOperation is one operation answered through callResult, and the
// failures the coordinator method behind it can actually return. The lists
// are read from internal/telephony/coordinator.go, not from what would be
// convenient: an operation is only held to a 409 it can produce.
type callOperation struct {
	operationID string
	serve       func(*Server, http.ResponseWriter, *http.Request, uuid.UUID)
	body        string
	// as is who asks: an agent on the call, a supervisor with a phone, or a
	// system with no leg of its own (hangupCall's other reading).
	as    AuthContext
	fails []string
}

func callOperations() []callOperation {
	agent := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), AgentID: uuid.New(),
		scopes: grantedScopes(auth.RoleAgent)}
	supervisor := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), AgentID: uuid.New(),
		scopes: grantedScopes(auth.RoleSupervisor)}
	system := AuthContext{Kind: SubjectKey, SubjectID: uuid.New()}
	// agentChannel: the call is gone, or the agent has no live leg on it.
	onlyTheirLeg := []string{"telephony.ErrCallNotFound", "telephony.ErrNoAgentLeg"}
	// handledChannel: agentChannel, refused first on an INTERNAL call.
	handled := []string{"telephony.ErrCallNotFound", "telephony.ErrNotForCallType", "telephony.ErrNoAgentLeg"}
	return []callOperation{
		{"answerCall", (*Server).AnswerCall, "", agent, onlyTheirLeg},
		{"holdCall", (*Server).HoldCall, "", agent, handled},
		{"retrieveCall", (*Server).RetrieveCall, "", agent, handled},
		{"muteCall", (*Server).MuteCall, "", agent, onlyTheirLeg},
		{"unmuteCall", (*Server).UnmuteCall, "", agent, onlyTheirLeg},
		// Hangup: agentLegAndCause, the same two as agentChannel.
		{"hangupCall", (*Server).HangupCall, "", agent, onlyTheirLeg},
		// EndCall: the call is gone, or nobody on it is at an extension.
		{"hangupCall", (*Server).HangupCall, "", system,
			[]string{"telephony.ErrCallNotFound", "telephony.ErrNoExtensionLeg"}},
		// Transfer checks the destination, then reads the call itself:
		// INTERNAL first, then whether the asker and a caller are both on it;
		// then whether a transfer of the caller is already on its way.
		{"transferCall", (*Server).TransferCall, `{"destination":"2001"}`, agent,
			[]string{"telephony.ErrInvalidDestination", "telephony.ErrCallNotFound", "telephony.ErrNotForCallType",
				"telephony.ErrNotCallParty", "telephony.ErrTransferInFlight"}},
		// SendDTMF: the digits, agentChannel, then a far end to send them to.
		{"sendCallDTMF", (*Server).SendCallDTMF, `{"digits":"1"}`, agent,
			[]string{"telephony.ErrInvalidDTMF", "telephony.ErrCallNotFound",
				"telephony.ErrNoAgentLeg", "telephony.ErrNotCallParty"}},
		// Monitor: the call is gone, or the agent named has no leg on it.
		{"monitorCall", (*Server).MonitorCall, `{"mode":"LISTEN","agentId":"` + uuid.NewString() + `"}`,
			supervisor, onlyTheirLeg},
	}
}

// failingCalls answers every call operation with one error.
type failingCalls struct {
	stubCalls
	err error
}

func (c failingCalls) Answer(context.Context, uuid.UUID, uuid.UUID) error   { return c.err }
func (c failingCalls) Hold(context.Context, uuid.UUID, uuid.UUID) error     { return c.err }
func (c failingCalls) Retrieve(context.Context, uuid.UUID, uuid.UUID) error { return c.err }
func (c failingCalls) Mute(context.Context, uuid.UUID, uuid.UUID) error     { return c.err }
func (c failingCalls) Unmute(context.Context, uuid.UUID, uuid.UUID) error   { return c.err }
func (c failingCalls) Hangup(context.Context, uuid.UUID, uuid.UUID) error   { return c.err }
func (c failingCalls) EndCall(context.Context, uuid.UUID) error             { return c.err }
func (c failingCalls) Transfer(context.Context, uuid.UUID, uuid.UUID, string) error {
	return c.err
}
func (c failingCalls) SendDTMF(context.Context, uuid.UUID, uuid.UUID, string) error {
	return c.err
}
func (c failingCalls) Monitor(context.Context, uuid.UUID, uuid.UUID, string, string) error {
	return c.err
}

// contractResponses is the status codes each operation declares, by
// operation id.
func contractResponses(t *testing.T) map[string][]string {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(readContract(t), &spec); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	declared := map[string][]string{}
	for _, item := range spec.Paths {
		for _, raw := range item {
			// A path item also holds `parameters`, which is not an operation.
			var op struct {
				OperationID string                     `json:"operationId"`
				Responses   map[string]json.RawMessage `json:"responses"`
			}
			if json.Unmarshal(raw, &op) != nil || op.OperationID == "" {
				continue
			}
			for code := range op.Responses {
				declared[op.OperationID] = append(declared[op.OperationID], code)
			}
			slices.Sort(declared[op.OperationID])
		}
	}
	return declared
}

// An INTERNAL call was refused with a 409 that holdCall, retrieveCall and
// transferCall described in prose and did not declare (#269), so a client
// generated from the contract had no case for the answer the server gave.
// Every outcome each operation can reach is driven through its handler here,
// and the status that comes back must be one the operation declares.
func TestEveryCallOperationDeclaresWhatItAnswers(t *testing.T) {
	t.Parallel()
	declared := contractResponses(t)
	reached := map[string]bool{}
	for _, op := range callOperations() {
		codes, ok := declared[op.operationID]
		if !ok {
			t.Errorf("%s is not an operation in the contract", op.operationID)
			continue
		}
		for _, outcome := range append(slices.Clone(everyCallOutcome), op.fails...) {
			reached[outcome] = true
			var err error
			switch outcome {
			case "":
			case "the switch refused":
				err = errors.New("-ERR no such channel")
			default:
				err = callFailures[outcome]
				if err == nil {
					t.Fatalf("%s lists %s, which callFailures does not name", op.operationID, outcome)
				}
			}

			srv := &Server{calls: failingCalls{err: err}, agents: dialerPresence{}}
			callID := uuid.New()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String(),
				strings.NewReader(op.body))
			r = r.WithContext(contextWithAuth(r.Context(), op.as))
			w := httptest.NewRecorder()
			op.serve(srv, w, r, callID)

			if !slices.Contains(codes, strconv.Itoa(w.Code)) {
				if outcome == "" {
					outcome = "success"
				}
				t.Errorf("%s answers %d on %s, and the contract declares only %v: %s",
					op.operationID, w.Code, outcome, codes, w.Body)
			}
		}
	}
	for name := range callFailures {
		if !reached[name] {
			t.Errorf("no operation can return %s, so its branch in callResult is never driven", name)
		}
	}
}

// The gate is only as good as its two lists, so both are held to the code
// they describe: callFailures to the errors callResult's switch names, and
// callOperations to the handlers that answer through callOp or callResult.
func TestCallResultsBranchesAreTheOnesTheGateDrives(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "call_handlers.go", nil, 0)
	if err != nil {
		t.Fatalf("parse call_handlers.go: %v", err)
	}

	var branches, handlers []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		switch fn.Name.Name {
		case "callResult":
			// Every pkg.ErrX a case of the switch names; not what the
			// branches then do.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				clause, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, expr := range clause.List {
					ast.Inspect(expr, func(n ast.Node) bool {
						sel, ok := n.(*ast.SelectorExpr)
						if !ok || !strings.HasPrefix(sel.Sel.Name, "Err") {
							return true
						}
						if pkg, ok := sel.X.(*ast.Ident); ok {
							branches = append(branches, pkg.Name+"."+sel.Sel.Name)
						}
						return true
					})
				}
				return false
			})
		case "callOp":
		default:
			// An exported handler that answers through callOp or callResult.
			if !fn.Name.IsExported() {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && (sel.Sel.Name == "callOp" || sel.Sel.Name == "callResult") {
					if !slices.Contains(handlers, fn.Name.Name) {
						handlers = append(handlers, fn.Name.Name)
					}
				}
				return true
			})
		}
	}

	named := make([]string, 0, len(callFailures))
	for name := range callFailures {
		named = append(named, name)
	}
	compare(t, "callResult", branches, "callFailures", named)

	// Operation ids are the handler names with a lower-case first letter.
	var driven []string
	for _, op := range callOperations() {
		handler := strings.ToUpper(op.operationID[:1]) + op.operationID[1:]
		if !slices.Contains(driven, handler) {
			driven = append(driven, handler)
		}
	}
	compare(t, "call_handlers.go", handlers, "callOperations", driven)
}
