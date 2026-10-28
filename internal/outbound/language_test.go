// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

// serviceWithLanguage is a service whose one outbound number was stored with
// the given language, as a number written before languages were checked may
// have been.
func serviceWithLanguage(t *testing.T, sw *fakeSwitch, language string) *Service {
	t.Helper()
	flowID := uuid.New()
	dids := fakeDIDs{{Number: "95012", Language: language, FlowID: &flowID, IsEnabled: true,
		AllowInbound: true, AllowOutbound: true, IsDefaultOutbound: true}}
	return New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s"}, sw, dids, provisioned(),
		func(context.Context, uuid.UUID) (bool, error) { return false, nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A number's stored language that the switch would read as syntax would have
// the bridge to the bot refused once the customer answered, leaving them on a
// silent line. The call is refused before anybody is rung instead.
func TestAStoredLanguageTheSwitchCannotCarryRefusesTheCallBeforeItRings(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"en'", "en^x", "zh,x", "en x", "en$", "en\n", "en{", `en\`, "en|x"} {
		sw := &fakeSwitch{}
		s := serviceWithLanguage(t, sw, language)
		_, err := s.PlaceAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"})
		if !errors.Is(err, ErrBadLanguage) {
			t.Errorf("language %q: PlaceAI error = %v, want ErrBadLanguage", language, err)
		}
		if n := len(sw.originates); n != 0 {
			t.Errorf("language %q: %d originates, want the customer never rung", language, n)
		}
		if s.LinesInUse("95012") != 0 {
			t.Errorf("language %q: a refused call holds a line", language)
		}
	}
}

// A language stored before the check that is merely not canonical ("EN",
// "zh_CN") is not syntax to the switch, and its calls place as they always
// did: refusing them would break a number that works.
func TestANonCanonicalStoredLanguageStillPlacesItsCalls(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"EN", "zh_CN", "english"} {
		sw := &fakeSwitch{}
		s := serviceWithLanguage(t, sw, language)
		if _, err := s.PlaceAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"}); err != nil {
			t.Errorf("language %q: PlaceAI error = %v", language, err)
			continue
		}
		if got := sw.lastOriginate().vars["aicc_language"]; got != language {
			t.Errorf("language %q rode the leg as %q", language, got)
		}
	}
}

// The request's own language wins over the number's and rides the leg and
// the bridge exactly as given.
func TestARequestLanguageRidesTheLegAndTheBridge(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := serviceWithLanguage(t, sw, "zh")
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{
		To: "13912345678", DIDNumber: "95012", Language: "pt-br",
	}); err != nil {
		t.Fatal(err)
	}
	leg := sw.lastOriginate()
	if leg.vars["aicc_language"] != "pt-br" {
		t.Errorf("aicc_language = %q on the leg, want pt-br", leg.vars["aicc_language"])
	}
	answer(s, leg.partyID.String())
	waitBridges(t, sw, 1)
	bridge := sw.lastBridge()
	if bridge.vars["aicc_language"] != "pt-br" || bridge.vars["sip_h_X-AICC-Language"] != "pt-br" {
		t.Errorf("bridge carries %q / %q, want pt-br", bridge.vars["aicc_language"], bridge.vars["sip_h_X-AICC-Language"])
	}
}
