// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// recordingCatalog keeps whatever the handler decided to hand the service.
type recordingCatalog struct {
	stubCatalog
	extension catalog.Extension
	queue     catalog.Queue
	did       catalog.DID
}

func (c *recordingCatalog) CreateExtension(_ context.Context, e catalog.Extension) (catalog.Extension, error) {
	c.extension = e
	return e, nil
}

func (c *recordingCatalog) UpdateExtension(_ context.Context, e catalog.Extension) (catalog.Extension, error) {
	c.extension = e
	return e, nil
}

func (c *recordingCatalog) CreateQueue(_ context.Context, q catalog.Queue) (catalog.Queue, error) {
	c.queue = q
	return q, nil
}

func (c *recordingCatalog) CreateDID(_ context.Context, d catalog.DID) (catalog.DID, error) {
	c.did = d
	return d, nil
}

func (c *recordingCatalog) UpdateDID(_ context.Context, d catalog.DID) (catalog.DID, error) {
	c.did = d
	return d, nil
}

func post(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
}

// A boolean the operator left out must arrive as the default the contract
// declares, not as Go's zero value. Nothing downstream can recover the
// difference: the column default never fires, because the insert names the
// column. An extension created this way was disabled, so the switch's
// directory view — which filters on is_enabled — never showed it, the phone
// never registered, and the API reported nothing but 201.
func TestAnOmittedBooleanTakesTheDeclaredDefault(t *testing.T) {
	t.Run("an omitted isEnabled creates an enabled extension", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateExtension(httptest.NewRecorder(), post(`{"number":"1099","password":"secret1"}`))

		if !c.extension.IsEnabled {
			t.Error("the extension was created disabled; the phone would never register")
		}
	})

	t.Run("an explicit false still disables it", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateExtension(httptest.NewRecorder(),
			post(`{"number":"1099","password":"secret1","isEnabled":false}`))

		if c.extension.IsEnabled {
			t.Error("isEnabled:false was ignored; the default overrode the operator")
		}
	})

	t.Run("update reads the same way as create", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}
		id := uuid.New()

		s.UpdateExtension(httptest.NewRecorder(), post(`{"number":"1099"}`), id)
		if !c.extension.IsEnabled {
			t.Error("an update that omitted isEnabled disabled the extension")
		}

		s.UpdateExtension(httptest.NewRecorder(), post(`{"number":"1099","isEnabled":false}`), id)
		if c.extension.IsEnabled {
			t.Error("an update could not disable an extension")
		}
	})

	t.Run("a queue keeps every boolean default it is given", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateQueue(httptest.NewRecorder(), post(`{"name":"support","extNumber":"9100"}`))

		if !c.queue.IsEnabled {
			t.Error("the queue was created disabled")
		}
		if !c.queue.IsRecordingEnabled {
			t.Error("the queue was created without recording; calls would go unrecorded")
		}
		// Not every default is true: this one's column default is false, and
		// seeding it true would be the same bug pointing the other way.
		if c.queue.IsAbandonedResumeAllowed {
			t.Error("isAbandonedResumeAllowed defaulted true, want false")
		}
	})

	t.Run("a number is reachable and recorded unless told otherwise", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateDID(httptest.NewRecorder(), post(`{"number":"95009"}`))
		if !c.did.IsEnabled || !c.did.IsRecordingEnabled {
			t.Errorf("the number was created disabled or unrecorded: %+v", c.did)
		}

		s.CreateDID(httptest.NewRecorder(),
			post(`{"number":"95009","isEnabled":false,"isRecordingEnabled":false}`))
		if c.did.IsEnabled || c.did.IsRecordingEnabled {
			t.Errorf("an explicit false was overridden: %+v", c.did)
		}
	})
}
