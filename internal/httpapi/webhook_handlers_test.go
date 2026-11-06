// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// stubWebhooks accepts whatever reaches it, so a refusal in these tests is the
// handler's own and never the service's.
type stubWebhooks struct {
	written store.WebhookSubscriptionWrite
	calls   int
}

func (s *stubWebhooks) List(context.Context) ([]store.WebhookSubscription, error) {
	return nil, nil
}

func (s *stubWebhooks) Get(context.Context, uuid.UUID) (store.WebhookSubscription, error) {
	return store.WebhookSubscription{}, nil
}

func (s *stubWebhooks) Create(_ context.Context, in store.WebhookSubscriptionWrite) (store.WebhookSubscription, error) {
	s.calls++
	s.written = in
	return store.WebhookSubscription{SubscriptionID: uuid.Must(uuid.NewV7()), Name: in.Name, URL: in.URL, Filter: in.Filter}, nil
}

func (s *stubWebhooks) Update(_ context.Context, id uuid.UUID, in store.WebhookSubscriptionWrite) (store.WebhookSubscription, error) {
	s.calls++
	s.written = in
	return store.WebhookSubscription{SubscriptionID: id, Name: in.Name, URL: in.URL, Filter: in.Filter}, nil
}

func (s *stubWebhooks) Delete(context.Context, uuid.UUID) (bool, error) { return true, nil }

func (s *stubWebhooks) Deliveries(context.Context, uuid.UUID, int) ([]store.WebhookDelivery, error) {
	return nil, nil
}

// A filter key the schema does not declare is a filter that can never be
// applied: nothing decodes it, so the subscription would be stored as "every
// call" and the operator told 201. The contract closes the list
// (`additionalProperties: false` on WebhookFilter) and design 09 §5 says an
// unknown key is 422 at creation; this is where that happens.
func TestAnUnknownFilterKeyIsRefusedWhenTheSubscriptionIsWritten(t *testing.T) {
	t.Parallel()
	t.Run("create is refused and names the key", func(t *testing.T) {
		svc := &stubWebhooks{}
		s := &Server{webhooks: svc}
		w := httptest.NewRecorder()

		s.CreateWebhookSubscription(w,
			post(`{"name":"x","url":"https://example.com/h","filter":{"talkSec":[30]}}`))

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
		}
		if code := errorCodeOf(t, w); code != string(CodeValidationFailed) {
			t.Errorf("code = %s, want %s", code, CodeValidationFailed)
		}
		if field, key := errorParamOf(t, w, "field"), errorParamOf(t, w, "key"); field != "filter" || key != "talkSec" {
			t.Errorf("details = field %q key %q, want field \"filter\" key \"talkSec\"", field, key)
		}
		if svc.calls != 0 {
			t.Error("the subscription was written despite the unknown key")
		}
	})

	t.Run("a filter of declared keys is still created", func(t *testing.T) {
		svc := &stubWebhooks{}
		s := &Server{webhooks: svc}
		w := httptest.NewRecorder()

		s.CreateWebhookSubscription(w,
			post(`{"name":"x","url":"https://example.com/h","filter":{"callType":["INBOUND"],"isContained":[true]}}`))

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", w.Code, w.Body)
		}
		if got := svc.written.Filter.CallType; !slices.Equal(got, []string{"INBOUND"}) {
			t.Errorf("callType = %v, want [INBOUND]", got)
		}
		if got := svc.written.Filter.IsContained; !slices.Equal(got, []bool{true}) {
			t.Errorf("isContained = %v, want [true]", got)
		}
	})

	t.Run("update reads the same way as create", func(t *testing.T) {
		svc := &stubWebhooks{}
		s := &Server{webhooks: svc}
		w := httptest.NewRecorder()

		s.UpdateWebhookSubscription(w,
			post(`{"name":"x","url":"https://example.com/h","filter":{"queue_id":["9d"]}}`), uuid.Must(uuid.NewV7()))

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
		}
		if key := errorParamOf(t, w, "key"); key != "queue_id" {
			t.Errorf("key = %q, want \"queue_id\"", key)
		}
		if svc.calls != 0 {
			t.Error("the subscription was updated despite the unknown key")
		}
	})

	t.Run("a body that is not JSON is still 400", func(t *testing.T) {
		s := &Server{webhooks: &stubWebhooks{}}
		w := httptest.NewRecorder()

		s.CreateWebhookSubscription(w, post(`{"name":`))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
		}
	})
}

// The contract says a subscription url is an absolute http or https URL with a
// host, at most 2048 characters (WebhookSubscriptionWrite.url); design 09 §4.1
// accepts http on purpose. The length is counted in characters, as maxLength is,
// so a multi-byte path of 2048 characters is within the limit.
func TestASubscriptionURLIsAnAbsoluteHTTPOrHTTPSAddress(t *testing.T) {
	t.Parallel()
	pad := func(prefix, unit string, total int) string {
		return prefix + strings.Repeat(unit, total-len([]rune(prefix)))
	}
	accepted := map[string]string{
		"https":                  "https://example.com/h",
		"http":                   "http://example.com/h",
		"http on the local host": "http://127.0.0.1:8771/hook",
		"2048 characters":        pad("https://example.com/", "a", 2048),
		"2048 multi-byte chars":  pad("https://example.com/", "例", 2048),
	}
	for name, u := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			svc := &stubWebhooks{}
			s := &Server{webhooks: svc}
			w := httptest.NewRecorder()

			s.CreateWebhookSubscription(w, post(`{"name":"x","url":"`+u+`"}`))

			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", w.Code, w.Body)
			}
			if svc.written.URL != u {
				t.Errorf("written url = %q, want %q", svc.written.URL, u)
			}
		})
	}
	refused := map[string]string{
		"ftp scheme":      "ftp://example.com/h",
		"mailto scheme":   "mailto:x@example.com",
		"no scheme":       "//example.com/h",
		"no host":         "http:///h",
		"2049 characters": pad("https://example.com/", "a", 2049),
		"2049 multi-byte": pad("https://example.com/", "例", 2049),
	}
	for name, u := range refused {
		t.Run("create refuses "+name, func(t *testing.T) {
			svc := &stubWebhooks{}
			s := &Server{webhooks: svc}
			w := httptest.NewRecorder()

			s.CreateWebhookSubscription(w, post(`{"name":"x","url":"`+u+`"}`))

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
			}
			if code := errorCodeOf(t, w); code != string(CodeValidationFailed) {
				t.Errorf("code = %s, want %s", code, CodeValidationFailed)
			}
			if field := errorParamOf(t, w, "field"); field != "url" {
				t.Errorf("field = %q, want \"url\"", field)
			}
			if svc.calls != 0 {
				t.Error("the subscription was written despite the bad url")
			}
		})
	}
	t.Run("update refuses 2049 characters", func(t *testing.T) {
		svc := &stubWebhooks{}
		s := &Server{webhooks: svc}
		w := httptest.NewRecorder()

		s.UpdateWebhookSubscription(w,
			post(`{"name":"x","url":"`+pad("https://example.com/", "a", 2049)+`"}`), uuid.Must(uuid.NewV7()))

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
		}
		if field := errorParamOf(t, w, "field"); field != "url" {
			t.Errorf("field = %q, want \"url\"", field)
		}
		if svc.calls != 0 {
			t.Error("the subscription was updated despite the bad url")
		}
	})
}

// The contract caps WebhookSubscriptionWrite.name at 120 and .authToken at 1024
// characters. Both columns are unbounded text, so the handler is the only
// place the limit holds, and it counts characters, not bytes.
func TestASubscriptionNameAndTokenAreCappedInCharactersNotBytes(t *testing.T) {
	t.Parallel()
	rep := func(unit string, n int) string { return strings.Repeat(unit, n) }
	type call struct {
		verb string
		do   func(s *Server, w *httptest.ResponseRecorder, body string)
		ok   int
	}
	calls := []call{
		{"create", func(s *Server, w *httptest.ResponseRecorder, body string) {
			s.CreateWebhookSubscription(w, post(body))
		}, http.StatusCreated},
		{"update", func(s *Server, w *httptest.ResponseRecorder, body string) {
			s.UpdateWebhookSubscription(w, post(body), uuid.Must(uuid.NewV7()))
		}, http.StatusOK},
	}
	nameBody := func(name string) string {
		return `{"name":"` + name + `","url":"https://example.com/h"}`
	}
	tokenBody := func(token string) string {
		return `{"name":"x","url":"https://example.com/h","authToken":"` + token + `"}`
	}
	names := []struct {
		label string
		value string
		ok    bool
	}{
		{"120 ASCII characters", rep("a", 120), true},
		{"121 ASCII characters", rep("a", 121), false},
		{"120 multi-byte characters", rep("例", 120), true},
		{"121 multi-byte characters", rep("例", 121), false},
		{"120 emoji", rep("😀", 120), true},
	}
	for _, c := range calls {
		for _, n := range names {
			t.Run(c.verb+" name with "+n.label, func(t *testing.T) {
				svc := &stubWebhooks{}
				w := httptest.NewRecorder()
				c.do(&Server{webhooks: svc}, w, nameBody(n.value))
				if n.ok {
					if w.Code != c.ok {
						t.Fatalf("status = %d, want %d: %s", w.Code, c.ok, w.Body)
					}
					if svc.written.Name != n.value {
						t.Error("the name was not stored as sent")
					}
					return
				}
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
				}
				if code := errorCodeOf(t, w); code != string(CodeValidationFailed) {
					t.Errorf("code = %s, want %s", code, CodeValidationFailed)
				}
				if field := errorParamOf(t, w, "field"); field != "name" {
					t.Errorf("field = %q, want \"name\"", field)
				}
				if svc.calls != 0 {
					t.Error("the subscription was written despite the long name")
				}
			})
		}
		for _, tc := range []struct {
			label string
			value string
			ok    bool
		}{
			{"1024 characters", rep("t", 1024), true},
			{"1024 multi-byte characters", rep("例", 1024), true},
			{"1025 characters", rep("t", 1025), false},
			{"1025 multi-byte characters", rep("例", 1025), false},
		} {
			t.Run(c.verb+" authToken with "+tc.label, func(t *testing.T) {
				svc := &stubWebhooks{}
				w := httptest.NewRecorder()
				c.do(&Server{webhooks: svc}, w, tokenBody(tc.value))
				if tc.ok {
					if w.Code != c.ok {
						t.Fatalf("status = %d, want %d: %s", w.Code, c.ok, w.Body)
					}
					if svc.written.AuthToken == nil || *svc.written.AuthToken != tc.value {
						t.Error("the token was not handed to the store as sent")
					}
					return
				}
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
				}
				if code := errorCodeOf(t, w); code != string(CodeValidationFailed) {
					t.Errorf("code = %s, want %s", code, CodeValidationFailed)
				}
				if field := errorParamOf(t, w, "field"); field != "authToken" {
					t.Errorf("field = %q, want \"authToken\"", field)
				}
				if svc.calls != 0 {
					t.Error("the subscription was written despite the long token")
				}
				if strings.Contains(w.Body.String(), tc.value) {
					t.Error("the refusal repeats the token")
				}
			})
		}
		t.Run(c.verb+" an empty authToken clears the token", func(t *testing.T) {
			svc := &stubWebhooks{}
			w := httptest.NewRecorder()
			c.do(&Server{webhooks: svc}, w, tokenBody(""))
			if w.Code != c.ok {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.ok, w.Body)
			}
			if svc.written.AuthToken == nil || *svc.written.AuthToken != "" {
				t.Errorf("token = %v, want a pointer to the empty string", svc.written.AuthToken)
			}
		})
		t.Run(c.verb+" an absent authToken leaves the token unchanged", func(t *testing.T) {
			svc := &stubWebhooks{}
			w := httptest.NewRecorder()
			c.do(&Server{webhooks: svc}, w, nameBody("x"))
			if w.Code != c.ok {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.ok, w.Body)
			}
			if svc.written.AuthToken != nil {
				t.Errorf("token = %q, want nil (absent)", *svc.written.AuthToken)
			}
		})
	}
}

// The closed list is written down twice — once as the contract's properties,
// once as the Go slice the handler checks against — and nothing but this joins
// them. A key added to the schema and not to the store would be declared and
// then refused; one added to the store and not to the schema would be accepted
// by the server and rejected by every client generated from the contract.
func TestTheFilterKeysAreTheOnesTheContractDeclares(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				WebhookFilter struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"WebhookFilter"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	declared := make([]string, 0, len(spec.Components.Schemas.WebhookFilter.Properties))
	for key := range spec.Components.Schemas.WebhookFilter.Properties {
		declared = append(declared, key)
	}
	if len(declared) == 0 {
		t.Fatal("the contract declares no WebhookFilter properties")
	}
	slices.Sort(declared)

	allowed := store.WebhookFilterKeys()
	slices.Sort(allowed)
	if !slices.Equal(declared, allowed) {
		t.Errorf("the contract declares %v and the store allows %v", declared, allowed)
	}
}

func errorParamOf(t *testing.T, w *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var env struct {
		Error struct {
			Params map[string]any `json:"params"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body: %v (%s)", err, w.Body)
	}
	value, _ := env.Error.Params[field].(string)
	return value
}
