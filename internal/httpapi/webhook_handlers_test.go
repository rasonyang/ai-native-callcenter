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
	return store.WebhookSubscription{SubscriptionID: uuid.New(), Name: in.Name, URL: in.URL, Filter: in.Filter}, nil
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
			post(`{"name":"x","url":"https://example.com/h","filter":{"queue_id":["9d"]}}`), uuid.New())

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

// The closed list is written down twice — once as the contract's properties,
// once as the Go slice the handler checks against — and nothing but this joins
// them. A key added to the schema and not to the store would be declared and
// then refused; one added to the store and not to the schema would be accepted
// by the server and rejected by every client generated from the contract.
func TestTheFilterKeysAreTheOnesTheContractDeclares(t *testing.T) {
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
