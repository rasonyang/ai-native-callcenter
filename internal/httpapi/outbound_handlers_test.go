// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// recordingOutbound keeps the dial request the handler built.
type recordingOutbound struct {
	stubOutbound
	got outbound.AIDialRequest
}

func (o *recordingOutbound) DialAI(_ context.Context, req outbound.AIDialRequest) (uuid.UUID, error) {
	o.got = req
	return uuid.New(), nil
}

func placeCall(t *testing.T, body string) (*httptest.ResponseRecorder, *recordingOutbound) {
	t.Helper()
	dialer := &recordingOutbound{}
	s := &Server{outbound: dialer}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	// Placing an AI call is an operations decision, and the check for it lives
	// in the handler rather than on the route.
	r = r.WithContext(contextWithIdentity(r.Context(), auth.Identity{
		UserID: uuid.New(), Role: auth.RoleSupervisor,
	}))
	s.CreateCall(w, r)
	return w, dialer
}

// Business data reaches the agent's screen and the ledger row, so what the
// request may attach is bounded where it arrives. Refused rather than
// truncated: a screen showing half a customer's details, with nothing to say
// the other half was sent, is worse than a request that failed.
func TestUserDataIsBoundedWhereItArrives(t *testing.T) {
	t.Parallel()
	t.Run("omitted attaches nothing", func(t *testing.T) {
		w, dialer := placeCall(t, `{"kind":"AI_OUTBOUND","to":"18600000000"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
		if len(dialer.got.UserData) != 0 {
			t.Errorf("userData = %v, want nothing", dialer.got.UserData)
		}
	})

	t.Run("an empty object is the same as omitted", func(t *testing.T) {
		w, dialer := placeCall(t, `{"kind":"AI_OUTBOUND","to":"18600000000","userData":{}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
		if len(dialer.got.UserData) != 0 {
			t.Errorf("userData = %v, want nothing", dialer.got.UserData)
		}
	})

	t.Run("what fits is carried through", func(t *testing.T) {
		w, dialer := placeCall(t,
			`{"kind":"AI_OUTBOUND","to":"18600000000","userData":{"ticketId":"T-1","tier":"VIP"}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
		if len(dialer.got.UserData) != 2 ||
			dialer.got.UserData["ticketId"] != "T-1" || dialer.got.UserData["tier"] != "VIP" {
			t.Errorf("userData = %v", dialer.got.UserData)
		}
	})

	// At the boundary, not far past it: a limit only ever tested at ten times
	// its value drifts without anybody noticing.
	t.Run("the last accepted key count and the first refused", func(t *testing.T) {
		if w, _ := placeCall(t, callWithKeys(userDataMaxKeys)); w.Code != http.StatusCreated {
			t.Errorf("%d keys was refused: %d %s", userDataMaxKeys, w.Code, w.Body)
		}
		w, _ := placeCall(t, callWithKeys(userDataMaxKeys+1))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%d keys returned %d", userDataMaxKeys+1, w.Code)
		}
		if code := errorCodeOf(t, w); code != string(CodeUserDataTooLarge) {
			t.Errorf("code = %q, want USER_DATA_TOO_LARGE", code)
		}
	})

	// Bytes, not characters, which is what is stored and shipped. The material
	// this deployment tests with is Chinese, where one character is three
	// bytes, and a limit that counted characters would let three times as much
	// through than it says. So the boundary is tried in every width.
	t.Run("the last accepted value size and the first refused, in every width", func(t *testing.T) {
		for _, c := range userDataValuesAtTheBound {
			w, dialer := placeCall(t, callWithValue(c.value))
			if !c.isAccepted {
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s (%d bytes) returned %d, want 400", c.name, len(c.value), w.Code)
					continue
				}
				if code := errorCodeOf(t, w); code != string(CodeUserDataTooLarge) {
					t.Errorf("%s: code = %q, want USER_DATA_TOO_LARGE", c.name, code)
				}
				continue
			}
			if w.Code != http.StatusCreated {
				t.Errorf("%s (%d bytes) was refused: %d %s", c.name, len(c.value), w.Code, w.Body)
				continue
			}
			if dialer.got.UserData["note"] != c.value {
				t.Errorf("%s reached the dialer as %d bytes, want it whole",
					c.name, len(dialer.got.UserData["note"]))
			}
		}
	})
}

// userDataValuesAtTheBound are the values on each side of the byte bound on
// a business-data value, in each width a UTF-8 character comes in: one byte
// (an order number), three (Chinese) and four (an emoji). In characters the
// bound falls somewhere different in each, which is why a test written only in
// ASCII cannot tell a byte limit from a character limit.
//
// Each accepted value is exactly userDataMaxValueBytes long and each refused
// one is a byte longer: with the bound at 1024, the Chinese one is 341
// characters and an ASCII byte, and the emoji takes bytes 1021 to 1024.
var userDataValuesAtTheBound = []struct {
	name       string
	value      string
	isAccepted bool
}{
	{"ASCII up to the bound", strings.Repeat("x", userDataMaxValueBytes), true},
	{"ASCII one byte past it", strings.Repeat("x", userDataMaxValueBytes+1), false},
	{"Chinese up to the bound",
		strings.Repeat("字", userDataMaxValueBytes/3) + strings.Repeat("x", userDataMaxValueBytes%3), true},
	{"Chinese one byte past it",
		strings.Repeat("字", userDataMaxValueBytes/3) + strings.Repeat("x", userDataMaxValueBytes%3+1), false},
	{"an emoji ending on the last byte", strings.Repeat("x", userDataMaxValueBytes-4) + "📞", true},
	{"an emoji ending one byte past it", strings.Repeat("x", userDataMaxValueBytes-3) + "📞", false},
}

func callWithKeys(n int) string {
	data := make(map[string]string, n)
	for i := range n {
		data[string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}
	body, _ := json.Marshal(map[string]any{
		"kind": "AI_OUTBOUND", "to": "18600000000", "userData": data,
	})
	return string(body)
}

func callWithValue(value string) string {
	body, _ := json.Marshal(map[string]any{
		"kind": "AI_OUTBOUND", "to": "18600000000",
		"userData": map[string]string{"note": value},
	})
	return string(body)
}

func errorCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body: %v (%s)", err, w.Body)
	}
	return env.Error.Code
}

// The same terms on the other door. A call the agent places from their own
// phone carries business data exactly as a placed AI call does — same shape,
// same limits, same carrier — because two ways of saying the same thing is
// how a second scheme starts.
func TestClickToDialCarriesTheSameBusinessData(t *testing.T) {
	t.Parallel()
	w, dialer := agentDial(t, agentSubject(),
		`{"kind":"AGENT_OUTBOUND","to":"18688886669","userData":{"orderId":"9999000000000000"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if len(dialer.got.UserData) == 0 {
		t.Fatal("the dial carried no business data at all")
	}
	if got := dialer.got.UserData["orderId"]; got != "9999000000000000" {
		t.Errorf("orderId = %q", got)
	}

	for _, c := range userDataValuesAtTheBound {
		body, _ := json.Marshal(map[string]any{
			"kind": "AGENT_OUTBOUND", "to": "18688886669",
			"userData": map[string]string{"note": c.value},
		})
		w, dialer := agentDial(t, agentSubject(), string(body))
		if c.isAccepted {
			if w.Code != http.StatusCreated || dialer.got.UserData["note"] != c.value {
				t.Errorf("%s (%d bytes): http = %d, want 201 with the value whole: %s",
					c.name, len(c.value), w.Code, w.Body)
			}
			continue
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s (%d bytes) returned %d, want 400", c.name, len(c.value), w.Code)
			continue
		}
		if code := errorCodeOf(t, w); code != string(CodeUserDataTooLarge) {
			t.Errorf("%s: code = %q, want USER_DATA_TOO_LARGE", c.name, code)
		}
	}
}

type recordingDialer struct {
	stubOutbound
	got outbound.AgentDialRequest
}

func (d *recordingDialer) Dial(_ context.Context, req outbound.AgentDialRequest) (uuid.UUID, error) {
	d.got = req
	return uuid.New(), nil
}

// agentSubject is a signed-in agent: an account with an agent identity and an
// agent's grant.
func agentSubject() AuthContext {
	return AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(), SubjectName: "mina",
		AgentID: uuid.New(), scopes: grantedScopes(auth.RoleAgent),
	}
}

// keySubject is a system holding a key: no agent identity of its own, and the
// scopes to place a call of either kind.
func keySubject() AuthContext {
	return AuthContext{
		Kind: SubjectKey, SubjectID: uuid.New(), SubjectName: "crm-integration",
		scopes: []string{api.ScopeCallsCreate, api.ScopeCallsCreateAI},
	}
}

// agentDial places a click-to-dial as whoever the identity says, against a
// stub where extension 1008 is the signed-in agent's phone and 1009 is a
// registered phone with nobody signed in at it.
func agentDial(t *testing.T, ac AuthContext, body string) (*httptest.ResponseRecorder, *recordingDialer) {
	t.Helper()
	dialer := &recordingDialer{}
	s := &Server{outbound: dialer, agents: dialerPresence{}, agentDir: dialerDirectory{}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.CreateCall(w, r)
	return w, dialer
}

// The handler is where the agent's switch-side name comes from, so this is
// where a dial that forgot it would go unnoticed.
func TestAClickToDialNamesTheAgentToTheSwitch(t *testing.T) {
	t.Parallel()
	w, dialer := agentDial(t, agentSubject(), `{"kind":"AGENT_OUTBOUND","to":"13912345678"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.CallcenterName != "agent-probe" {
		t.Errorf("dialled with callcenterName %q, want the switch's name for this agent — "+
			"without it mod_callcenter keeps offering them queue calls mid-conversation",
			dialer.got.CallcenterName)
	}
	if dialer.got.AgentExtension != "1008" {
		t.Errorf("raised %q, want the phone the agent signed in at", dialer.got.AgentExtension)
	}
}

// kind is optional: left out, the request is a click-to-dial. The default
// is the kind that needs no second scope, so omitting it can never reach
// further than naming it would have.
func TestAnOmittedKindIsAClickToDial(t *testing.T) {
	t.Parallel()
	w, dialer := agentDial(t, agentSubject(), `{"to":"13912345678"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.AgentExtension != "1008" || dialer.got.To != "13912345678" {
		t.Errorf("dialled %+v, want the agent's own phone raised for 13912345678", dialer.got)
	}
	if w, _ := agentDial(t, agentSubject(), `{"kind":"OUTBOUND","to":"13912345678"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: http = %d, want 400 — the default covers absence, not typos", w.Code)
	}
}

// The requirement this whole path exists for: a system places the call, the
// agent's phone is registered, and nobody has signed into this application.
// Presence has nothing to say about such an agent, so the phone is what is
// asked about.
func TestASystemDialsForAnAgentWhoNeverSignedIn(t *testing.T) {
	t.Parallel()
	w, dialer := agentDial(t, keySubject(),
		`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"1009"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.AgentExtension != "1009" {
		t.Errorf("raised %q, want the extension the request named", dialer.got.AgentExtension)
	}
	// Nobody is signed in there, so there is no queue membership to guard and
	// nothing for mod_callcenter to be told. Empty is the answer, not a gap.
	if dialer.got.CallcenterName != "" {
		t.Errorf("callcenterName = %q, want empty for a phone nobody is signed in at",
			dialer.got.CallcenterName)
	}
}

// Without an extension there is no phone to raise, and the API key has none of
// its own to fall back on. Refused rather than guessed.
func TestASystemMustSayWhichPhoneToRaise(t *testing.T) {
	t.Parallel()
	w, _ := agentDial(t, keySubject(), `{"kind":"AGENT_OUTBOUND","to":"13912345678"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
}

// A phone that cannot take the call is refused before it is rung, and the
// refusal says which way it cannot: an extension nobody has ever registered is
// a typo in the integration, a phone that is switched off is an operations
// problem, and an operator reading the error has to tell them apart.
func TestAPhoneThatCannotTakeTheCallIsRefusedBeforeItIsRung(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, extension, want string
	}{
		{"never seen", "1999", "no such phone"},
		{"not registered", "1010", "the phone is not registered"},
		{"not answering", "1011", "the phone is not answering"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, dialer := agentDial(t, keySubject(),
				`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"`+tc.extension+`"}`)
			if w.Code != http.StatusConflict {
				t.Fatalf("http = %d: %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("message = %s, want %q", w.Body, tc.want)
			}
			if dialer.got.AgentExtension != "" {
				t.Error("the phone was rung anyway")
			}
		})
	}
}

// An agent acts as themselves. A cockpit that sent somebody else's extension
// has a bug, and dialling from the right phone anyway would hide it.
func TestAnAgentMayNotDialFromAnotherAgentsPhone(t *testing.T) {
	t.Parallel()
	w, dialer := agentDial(t, agentSubject(),
		`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"1009"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.AgentExtension != "" {
		t.Error("somebody else's phone was rung")
	}
}

// Naming their own phone is not the same mistake, and is allowed: a client
// that fills the field in from its own presence is being explicit, not
// overreaching.
func TestAnAgentMayNameTheirOwnPhone(t *testing.T) {
	t.Parallel()
	w, dialer := agentDial(t, agentSubject(),
		`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"1008"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.AgentExtension != "1008" {
		t.Errorf("raised %q", dialer.got.AgentExtension)
	}
}

// Placing an AI call is an operations decision, and the route no longer guards
// it — one path serves two kinds with two different answers, so the check
// lives in the handler and this is what keeps it there.
//
// calls:create alone is not enough, which is the whole point: the contract can
// give this operation one scope, and an agent holds it for their own
// click-to-dial. Starting a bot on a number is not the same act.
func TestAnAgentMayNotPlaceAnAICall(t *testing.T) {
	t.Parallel()
	dialer := &recordingOutbound{}
	s := &Server{outbound: dialer, agents: dialerPresence{}, agentDir: dialerDirectory{}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls",
		strings.NewReader(`{"kind":"AI_OUTBOUND","to":"18600000000","did":"95012"}`))
	r = r.WithContext(contextWithAuth(r.Context(), agentSubject()))
	w := httptest.NewRecorder()
	s.CreateCall(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.To != "" {
		t.Error("the call went out anyway")
	}
	if !strings.Contains(w.Body.String(), `"requiredScope":"calls:create:ai"`) {
		t.Errorf("the refusal does not name calls:create:ai: %s", w.Body)
	}

	// The rule is the contract's x-body-scopes, not a literal here: losing it
	// from docs/openapi.json would open the bot to every agent, silently.
	if got := api.ScopesForBody("createCall", string(api.CreateCallRequestKindAIOUTBOUND)); !slices.Equal(got, []string{api.ScopeCallsCreateAI}) {
		t.Errorf("AI_OUTBOUND requires %v beyond calls:create, want [%s]", got, api.ScopeCallsCreateAI)
	}
	if got := api.ScopesForBody("createCall", string(api.CreateCallRequestKindAGENTOUTBOUND)); got != nil {
		t.Errorf("AGENT_OUTBOUND requires %v beyond calls:create, want nothing", got)
	}
}

// A retry from a system that timed out must not raise the agent's phone a
// second time while they are still talking on the first call, so the
// client-minted id is carried through to the service that keeps the record.
func TestAClickToDialCarriesTheClientMintedIDThroughToTheService(t *testing.T) {
	t.Parallel()
	callID := uuid.New()
	w, dialer := agentDial(t, keySubject(),
		`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"1009","callId":"`+
			callID.String()+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if dialer.got.CallID != callID {
		t.Errorf("callId = %v, want the one the client minted (%v) — without it a "+
			"retried dial rings the agent again", dialer.got.CallID, callID)
	}
}

type dialerDirectory struct{}

func (dialerDirectory) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (dialerDirectory) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// dialerPresence is stubAgents with a floor of phones: 1008 is where the
// signed-in agent sits, 1009 is registered with nobody signed in at it, 1010
// is known but unregistered and 1011 is registered but not answering.
type dialerPresence struct{ stubAgents }

func (dialerPresence) CallcenterNameFor(context.Context, uuid.UUID) string { return "agent-probe" }

// The phone bound in configuration, which is where a supervisor who staffs no
// queue is reached.
func (dialerPresence) BoundExtensionFor(context.Context, uuid.UUID) string { return "1009" }

func (dialerPresence) Presence(uuid.UUID) agents.Presence {
	return agents.Presence{ExtensionNumber: "1008"}
}

func (dialerPresence) DeviceAtExtension(extension string) (isRegistered, isInService, isKnown bool) {
	switch extension {
	case "1008", "1009":
		return true, true, true
	case "1010":
		return false, false, true
	case "1011":
		return true, false, true
	}
	return false, false, false
}

func (dialerPresence) AgentAtExtension(extension string) (uuid.UUID, bool) {
	if extension == "1008" {
		return uuid.New(), true
	}
	return uuid.Nil, false
}

// The bounds are written down twice — once in the contract, once as the Go
// constants the merge enforces — and nothing but this joins them.
//
// The contract states them per request schema: the key bound as
// `maxProperties`, the value bound in the description, because it is in bytes
// and JSON Schema's `maxLength` counts characters (#371).
// telephony.MergeUserData is where they are actually applied, to every source
// of business data and not only to these two endpoints. Change one and the
// other goes quietly out of step: a client told it may send 64 keys would have
// half of them dropped without being refused.
func TestTheUserDataBoundsAreTheOnesTheContractStates(t *testing.T) {
	t.Parallel()
	for _, c := range userDataValuesAtTheBound {
		if fits := len(c.value) <= telephony.UserDataMaxValueBytes; fits != c.isAccepted {
			t.Fatalf("fixture %q is %d bytes against a bound of %d, but isAccepted = %v",
				c.name, len(c.value), telephony.UserDataMaxValueBytes, c.isAccepted)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	// Walked generically rather than decoded into a shape: `additionalProperties`
	// is a schema on this field and a bare `true` on others (FlowSpec), and a
	// typed decode of the whole document fails on the ones this test is not
	// about.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	dig := func(v any, path ...string) any {
		for _, key := range path {
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		}
		return v
	}
	number := func(v any) (int, bool) {
		f, ok := v.(float64)
		return int(f), ok
	}

	// One definition each for the two shapes: what a call may be given, and
	// the patch that changes it. The request schemas $ref these rather than
	// restating them, which is what stops the numbers drifting apart between
	// the endpoints as well as away from Go.
	for _, name := range []string{"UserData", "UserDataPatch"} {
		userData := dig(doc, "components", "schemas", name)
		if userData == nil {
			t.Fatalf("%s is not a component in the contract", name)
		}
		keys, ok := number(dig(userData, "maxProperties"))
		if !ok {
			t.Errorf("%s states no maxProperties, so the contract promises no bound at all", name)
		} else if keys != telephony.UserDataMaxKeys {
			t.Errorf("%s allows %d keys, the merge allows %d", name, keys, telephony.UserDataMaxKeys)
		}

		// No JSON Schema keyword counts bytes, so the value schema cannot
		// state the bound exactly; what it must do is agree with it on both
		// sides. First, it accepts every value the server accepts, or a client
		// that validates against it refuses data the platform would take.
		value, ok := dig(userData, "additionalProperties").(map[string]any)
		if !ok {
			t.Fatalf("%s has no value schema", name)
		}
		for _, c := range userDataValuesAtTheBound {
			if c.isAccepted && !valueSchemaAdmits(t, value, c.value) {
				t.Errorf("%s refuses %s (%d bytes, %d characters), which the server accepts",
					name, c.name, len(c.value), utf8.RuneCountInString(c.value))
			}
		}
		// Second, it never admits a value the server refuses. A character is
		// up to four bytes, so a maxLength is safe only at a quarter of the
		// byte bound, and that would refuse most of what the server accepts.
		// The two checks together leave no maxLength as the only answer.
		if chars, ok := number(value["maxLength"]); ok && 4*chars > telephony.UserDataMaxValueBytes {
			t.Errorf("%s gives values maxLength %d, which counts characters: %d four-byte "+
				"characters are %d bytes, and the server refuses a value over %d",
				name, chars, chars, 4*chars, telephony.UserDataMaxValueBytes)
		}
		// With no keyword to carry it, the bound is the description's to say.
		want := fmt.Sprintf("%d bytes of UTF-8", telephony.UserDataMaxValueBytes)
		if desc, _ := dig(userData, "description").(string); !strings.Contains(desc, want) {
			t.Errorf("%s's description does not say %q, and nothing else in the contract states the value bound",
				name, want)
		}
	}

	// And the endpoints point at those components rather than carrying their
	// own copy — the drift this extraction exists to end.
	for _, ref := range []struct{ schema, want string }{
		{"CreateCallRequest", "#/components/schemas/UserData"},
		{"PatchUserDataRequest", "#/components/schemas/UserDataPatch"},
	} {
		got := dig(doc, "components", "schemas", ref.schema, "properties", "userData", "$ref")
		if got != ref.want {
			t.Errorf("%s.userData is %v, want a $ref to %s", ref.schema, got, ref.want)
		}
	}
}

// valueSchemaAdmits evaluates a value schema against a string the way a JSON
// Schema validator would, for the keywords such a schema uses: maxLength and
// minLength count characters (code points, as RFC 8259 defines them), never
// bytes. A keyword it does not know fails the test rather than being skipped,
// because a skipped keyword is a verdict guessed.
func valueSchemaAdmits(t *testing.T, schema map[string]any, v string) bool {
	t.Helper()
	chars := float64(utf8.RuneCountInString(v))
	admits := true
	for keyword, arg := range schema {
		switch keyword {
		case "description":
		case "type":
			types, ok := arg.([]any)
			if !ok {
				types = []any{arg}
			}
			admits = admits && slices.Contains(types, any("string"))
		case "maxLength":
			n, ok := arg.(float64)
			admits = admits && ok && chars <= n
		case "minLength":
			n, ok := arg.(float64)
			admits = admits && ok && chars >= n
		default:
			t.Fatalf("the value schema uses %q, which this test cannot evaluate; teach it the keyword", keyword)
		}
	}
	return admits
}

func (dialerDirectory) UserIDForAgent(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

// A canonical E.164 number reaches the dialer untouched, for both kinds: the
// handler neither strips the "+" nor rewrites a national number.
func TestAPlusDestinationReachesTheDialerAsSent(t *testing.T) {
	t.Parallel()
	w, ai := placeCall(t, `{"kind":"AI_OUTBOUND","to":"+8613800138000"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("AI: http = %d: %s", w.Code, w.Body)
	}
	if ai.got.To != "+8613800138000" {
		t.Errorf("AI dialled %q, want the number as sent", ai.got.To)
	}

	w, click := agentDial(t, agentSubject(), `{"kind":"AGENT_OUTBOUND","to":"+8613800138000"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("click-to-dial: http = %d: %s", w.Code, w.Body)
	}
	if click.got.To != "+8613800138000" || click.got.AgentExtension != "1008" {
		t.Errorf("click-to-dial = %+v, want the + number and the agent's own phone", click.got)
	}

	// A national number is still accepted and still not rewritten.
	_, national := placeCall(t, `{"kind":"AI_OUTBOUND","to":"13800138000"}`)
	if national.got.To != "13800138000" {
		t.Errorf("national dialled %q, want it unchanged", national.got.To)
	}
}
