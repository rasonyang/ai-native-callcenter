// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
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
