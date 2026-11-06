// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

type fakeDNC struct {
	listed map[string]bool
	err    error
	asked  []string
}

func (f *fakeDNC) Contains(_ context.Context, number string) (bool, error) {
	f.asked = append(f.asked, number)
	return f.listed[number], f.err
}

func dncService(t *testing.T, sw *fakeSwitch, countryCode string, dnc DNCList) *Service {
	t.Helper()
	s := testService(t, sw, nil)
	s.cfg.DefaultCountryCode = countryCode
	if dnc != nil {
		s.WithDNC(dnc)
	}
	return s
}

// Both dial paths ask the same question of the same canonical number, and a
// "yes" stops the call before the switch hears of it.
func TestBothDialPathsRefuseAListedNumberBeforeTheSwitchHearsOfIt(t *testing.T) {
	t.Parallel()
	for _, dialled := range []string{"13800138000", "+8613800138000", "008613800138000", "013800138000"} {
		sw := &fakeSwitch{}
		dnc := &fakeDNC{listed: map[string]bool{"+8613800138000": true}}
		s := dncService(t, sw, "86", dnc)

		if _, err := s.DialAI(context.Background(), AIDialRequest{To: dialled, DIDNumber: "95012"}); !errors.Is(err, ErrNumberOnDNC) {
			t.Errorf("DialAI(%q) = %v, want ErrNumberOnDNC", dialled, err)
		}
		if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: dialled}); !errors.Is(err, ErrNumberOnDNC) {
			t.Errorf("Dial(%q) = %v, want ErrNumberOnDNC", dialled, err)
		}
		if len(sw.originates) != 0 {
			t.Errorf("%q: %d originates for a listed number", dialled, len(sw.originates))
		}
	}
}

// With no country code a national number cannot be completed. That is the
// documented gap: it goes through, and the list is not even asked.
func TestANationalNumberWithNoCountryCodeIsNotChecked(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	dnc := &fakeDNC{listed: map[string]bool{"+8613800138000": true}}
	s := dncService(t, sw, "", dnc)

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13800138000", DIDNumber: "95012"}); err != nil {
		t.Fatalf("DialAI: %v", err)
	}
	if len(dnc.asked) != 0 {
		t.Errorf("the list was asked about %v, want nothing to compare", dnc.asked)
	}

	// A plus number needs no country code.
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "+8613800138000", DIDNumber: "95012"}); !errors.Is(err, ErrNumberOnDNC) {
		t.Errorf("DialAI(+...) = %v, want ErrNumberOnDNC", err)
	}
}

// A list that cannot be read is not a licence to dial.
func TestADNCLookupFailureRefusesTheCall(t *testing.T) {
	t.Parallel()
	boom := errors.New("storage down")
	sw := &fakeSwitch{}
	s := dncService(t, sw, "86", &fakeDNC{err: boom})

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13800138000", DIDNumber: "95012"}); !errors.Is(err, boom) {
		t.Errorf("DialAI = %v, want the lookup failure", err)
	}
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: "13800138000"}); !errors.Is(err, boom) {
		t.Errorf("Dial = %v, want the lookup failure", err)
	}
	if len(sw.originates) != 0 {
		t.Error("a call went out although the list could not be read")
	}
}

// No list installed, no check: the dialler is usable without one.
func TestNoListInstalledMeansNothingIsChecked(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := dncService(t, sw, "86", nil)
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13800138000", DIDNumber: "95012"}); err != nil {
		t.Fatalf("DialAI: %v", err)
	}
}

// A campaign attempt dialled in the NATIONAL form reaches DialAI as a national
// string; the check completes it with the deployment's country code, so a
// listed number is still caught.
func TestANationalDialStringOfAListedNumberIsStillRefused(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	dnc := &fakeDNC{listed: map[string]bool{"+8618688886666": true}}
	s := dncService(t, sw, "86", dnc)
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95012"}); !errors.Is(err, ErrNumberOnDNC) {
		t.Fatalf("DialAI(national) = %v, want ErrNumberOnDNC", err)
	}
	if len(sw.originates) != 0 {
		t.Errorf("%d originates for a listed number", len(sw.originates))
	}
}

// A campaign attempt carries the contact's stored canonical number next to the
// dial string, and the list is asked about that number whatever the trunk's
// form or the campaign's country code made of the dial string (#477). The
// dial string is built exactly as the dialer builds it.
func TestACampaignAttemptIsCheckedAgainstTheStoredNumberInEveryDialForm(t *testing.T) {
	forms := []phonenum.DialForm{phonenum.FormE164, phonenum.FormE164NoPlus, phonenum.FormNational}
	numbers := []string{
		"+8613800138000", "+8618688886666", // +86 mobiles
		"+12025550143", "+12025550177", // NANP, fictional 555-01xx
		"+442079460958",    // +44 in a +86 (or +1) campaign: NATIONAL falls back to E.164
		"+123456789012345", // the longest E.164
	}
	for _, form := range forms {
		for _, deploymentCC := range []string{"1", "86"} {
			for _, campaignCC := range []string{"1", "86", "44"} {
				for _, number := range numbers {
					dialString, _ := phonenum.DialString(number, form, campaignCC)
					name := string(form) + "/deployment" + deploymentCC + "/campaign" + campaignCC + "/" + number
					t.Run(name+"/listed", func(t *testing.T) {
						sw := &fakeSwitch{}
						dnc := &fakeDNC{listed: map[string]bool{number: true}}
						s := dncService(t, sw, deploymentCC, dnc)
						_, err := s.PlaceAI(context.Background(), AIDialRequest{
							To: dialString, CanonicalTo: number, DIDNumber: "95012",
						})
						if !errors.Is(err, ErrNumberOnDNC) {
							t.Fatalf("PlaceAI(%q for %s) = %v, want ErrNumberOnDNC (asked %v)", dialString, number, err, dnc.asked)
						}
						if len(sw.originates) != 0 {
							t.Errorf("%d originates for a listed number", len(sw.originates))
						}
					})
					t.Run(name+"/unlisted", func(t *testing.T) {
						sw := &fakeSwitch{}
						dnc := &fakeDNC{}
						s := dncService(t, sw, deploymentCC, dnc)
						if _, err := s.PlaceAI(context.Background(), AIDialRequest{
							To: dialString, CanonicalTo: number, DIDNumber: "95012",
						}); err != nil {
							t.Fatalf("PlaceAI: %v", err)
						}
						if len(dnc.asked) != 1 || dnc.asked[0] != number {
							t.Errorf("the list was asked about %v, want exactly [%s]", dnc.asked, number)
						}
						if len(sw.originates) != 1 {
							t.Fatalf("%d originates, want 1", len(sw.originates))
						}
						if got, want := sw.lastOriginate().endpoint, "sofia/gateway/pstn_gateway/"+dialString; got != want {
							t.Errorf("originate endpoint = %q, want %q (the dial string is unchanged)", got, want)
						}
					})
				}
			}
		}
	}
}

// A canonical number that is not canonical is a defect of the caller. The
// check is not skipped for it and the call is not placed.
func TestACanonicalNumberThatIsNotCanonicalRefusesTheCall(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"8613800138000", "13800138000", "+0123456789", "+86 138", "+86138abc8000", "+1234567890123456", "+1"} {
		sw := &fakeSwitch{}
		dnc := &fakeDNC{}
		s := dncService(t, sw, "86", dnc)
		_, err := s.PlaceAI(context.Background(), AIDialRequest{
			To: "8613800138000", CanonicalTo: bad, DIDNumber: "95012",
		})
		if !errors.Is(err, ErrBadCanonicalNumber) {
			t.Errorf("PlaceAI(CanonicalTo=%q) = %v, want ErrBadCanonicalNumber", bad, err)
		}
		if errors.Is(err, ErrBadNumber) || errors.Is(err, ErrNumberOnDNC) {
			t.Errorf("CanonicalTo=%q: %v is mislabelled as a fact about the customer", bad, err)
		}
		if len(sw.originates) != 0 || len(dnc.asked) != 0 {
			t.Errorf("CanonicalTo=%q: %d originates, list asked %v; want neither", bad, len(sw.originates), dnc.asked)
		}
	}
}

// An already placed call is reported as placed even when the number has since
// joined the list: the check keeps its place after the idempotency check.
func TestANumberListedAfterPlacementIsStillReportedAsPlaced(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	dnc := &fakeDNC{listed: map[string]bool{}}
	s := dncService(t, sw, "1", dnc)
	req := AIDialRequest{To: "8613800138000", CanonicalTo: "+8613800138000", DIDNumber: "95012", CallID: uuid.Must(uuid.NewV7())}
	if _, err := s.PlaceAI(context.Background(), req); err != nil {
		t.Fatalf("first PlaceAI: %v", err)
	}
	dnc.listed["+8613800138000"] = true
	if _, err := s.PlaceAI(context.Background(), req); !errors.Is(err, ErrAlreadyPlaced) {
		t.Fatalf("second PlaceAI = %v, want ErrAlreadyPlaced", err)
	}
}
