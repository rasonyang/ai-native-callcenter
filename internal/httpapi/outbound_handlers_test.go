// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
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
	s.CreateCall(w, httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body)))
	return w, dialer
}

// Business data reaches the agent's screen and the ledger row, so what the
// request may attach is bounded where it arrives. Refused rather than
// truncated: a screen showing half a customer's details, with nothing to say
// the other half was sent, is worse than a request that failed.
func TestUserDataIsBoundedWhereItArrives(t *testing.T) {
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

	t.Run("the last accepted value size and the first refused", func(t *testing.T) {
		if w, _ := placeCall(t, callWithValue(userDataMaxValueBytes)); w.Code != http.StatusCreated {
			t.Errorf("%d bytes was refused: %d %s", userDataMaxValueBytes, w.Code, w.Body)
		}
		w, _ := placeCall(t, callWithValue(userDataMaxValueBytes+1))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%d bytes returned %d", userDataMaxValueBytes+1, w.Code)
		}
		if code := errorCodeOf(t, w); code != string(CodeUserDataTooLarge) {
			t.Errorf("code = %q, want USER_DATA_TOO_LARGE", code)
		}
	})

	// Bytes, not characters. The material this deployment tests with is
	// Chinese, where one character is three bytes, and a limit that counted
	// characters would let three times as much through than it says.
	t.Run("the limit counts bytes, which is what is stored and shipped", func(t *testing.T) {
		value := strings.Repeat("字", userDataMaxValueBytes/3+1) // > 1024 bytes, ~342 characters
		body, err := json.Marshal(map[string]any{
			"kind": "AI_OUTBOUND", "to": "18600000000",
			"userData": map[string]string{"note": value},
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		w, _ := placeCall(t, string(body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%d characters (%d bytes) returned %d — the limit is counting "+
				"characters, and says bytes", len([]rune(value)), len(value), w.Code)
		}
	})
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

func callWithValue(bytes int) string {
	body, _ := json.Marshal(map[string]any{
		"kind": "AI_OUTBOUND", "to": "18600000000",
		"userData": map[string]string{"note": strings.Repeat("x", bytes)},
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
	dial := func(body string) (*httptest.ResponseRecorder, *recordingDialer) {
		dialer := &recordingDialer{}
		s := &Server{outbound: dialer, agents: dialerPresence{}, agentDir: dialerDirectory{}}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/calls/dial", strings.NewReader(body))
		req = req.WithContext(contextWithIdentity(req.Context(), auth.Identity{UserID: uuid.New(), Role: auth.RoleAgent}))
		s.DialCall(w, req)
		return w, dialer
	}

	w, dialer := dial(`{"destination":"18688886669","userData":{"orderId":"9999000000000000"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if len(dialer.userData) == 0 {
		t.Fatal("the dial carried no business data at all")
	}
	if got := dialer.userData["orderId"]; got != "9999000000000000" {
		t.Errorf("orderId = %q", got)
	}

	w, _ = dial(`{"destination":"18688886669","userData":` + oversizedValue() + `}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an oversized value returned %d, want 400", w.Code)
	}
	if code := errorCodeOf(t, w); code != string(CodeUserDataTooLarge) {
		t.Errorf("code = %q, want USER_DATA_TOO_LARGE", code)
	}
}

func oversizedValue() string {
	body, _ := json.Marshal(map[string]string{"note": strings.Repeat("x", userDataMaxValueBytes+1)})
	return string(body)
}

type recordingDialer struct {
	stubOutbound
	userData map[string]string
}

func (d *recordingDialer) Dial(_ context.Context, _, _ string, userData map[string]string) (uuid.UUID, error) {
	d.userData = userData
	return uuid.New(), nil
}

type dialerDirectory struct{}

func (dialerDirectory) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (dialerDirectory) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// dialerPresence is stubAgents with a phone: click-to-dial refuses an agent
// who is not signed in at one.
type dialerPresence struct{ stubAgents }

func (dialerPresence) Presence(uuid.UUID) agents.Presence {
	return agents.Presence{ExtensionNumber: "1008"}
}

// The bounds are written down twice — once in the contract, once as the Go
// constants the merge enforces — and nothing but this joins them.
//
// The contract states them per request schema (`maxProperties` and the value
// `maxLength`); telephony.MergeUserData is where they are actually applied, to
// every source of business data and not only to these two endpoints. Change
// one and the other goes quietly out of step: a client told it may send 64
// keys would have half of them dropped without being refused.
func TestTheUserDataBoundsAreTheOnesTheContractStates(t *testing.T) {
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

	// Both request schemas carry the field today. When they are folded into
	// one shared UserData component this loop simply has one entry.
	for _, name := range []string{"CreateCallRequest", "DialRequest"} {
		userData := dig(doc, "components", "schemas", name, "properties", "userData")
		if userData == nil {
			t.Fatalf("%s has no userData property in the contract", name)
		}
		keys, ok := number(dig(userData, "maxProperties"))
		if !ok {
			t.Errorf("%s.userData states no maxProperties, so the contract promises no bound at all", name)
		} else if keys != telephony.UserDataMaxKeys {
			t.Errorf("%s.userData allows %d keys, the merge allows %d", name, keys, telephony.UserDataMaxKeys)
		}
		bytes, ok := number(dig(userData, "additionalProperties", "maxLength"))
		if !ok {
			t.Errorf("%s.userData states no value maxLength", name)
		} else if bytes != telephony.UserDataMaxValueBytes {
			t.Errorf("%s.userData allows %d-byte values, the merge allows %d",
				name, bytes, telephony.UserDataMaxValueBytes)
		}
	}
}
