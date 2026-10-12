// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"testing"
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
