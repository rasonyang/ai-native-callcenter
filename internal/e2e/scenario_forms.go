// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// The simulator's scenario is keyed on the Request-URI user part, which for a
// campaign contact is the number in the deployment's AICC_OUTBOUND_DIAL_FORM
// (internal/dialer/admit.go), so there is one scenario file per form.
// deploy/e2e/scenario.json is the one hand-edited source, keyed on the
// canonical numbers: it is the E164 file. The other two are generated from it
// by ScenarioForForm and committed, and TestScenarioForms fails when one
// differs from what it would generate.

// SmokeNumber is the number the smoke test dials through POST /calls. It is
// not a campaign contact, so no form rewrites it.
const SmokeNumber = "13800000001"

// ScenarioFileName is the file in deploy/e2e that holds the scenario for a
// dial form: scenario.json for E164, scenario.<FORM>.json for the others.
func ScenarioFileName(form phonenum.DialForm) string {
	if form == phonenum.FormE164 {
		return "scenario.json"
	}
	return "scenario." + string(form) + ".json"
}

// DialForms are the three forms an e2e deployment may run, in the order the
// documentation lists them.
var DialForms = []phonenum.DialForm{phonenum.FormE164, phonenum.FormE164NoPlus, phonenum.FormNational}

// scenarioKeyKind says what a key of the E164 scenario is.
type scenarioKeyKind int

const (
	// keyCampaign is a campaign contact's number: the dialer applies the
	// deployment's dial form to it.
	keyCampaign scenarioKeyKind = iota + 1
	// keyDirect is a number the driver dials through POST /calls (K1's slot
	// holders, K3's direct calls) or the smoke dials: it is dialled exactly as
	// sent, in every form.
	keyDirect
)

// campaignKeyOwner is a contact of a campaign the harness runs.
type campaignKeyOwner struct {
	region Region
	label  string
}

// scenarioKeyClasses classifies every number the harness's runs dial, keyed
// on the canonical number: every contact of every campaign (the campaign
// table, the capacity scenarios' campaigns, K3's campaign) and every direct
// call. A number that is both, or a campaign number two regions share, is an
// error: the file could not be rewritten without guessing.
func scenarioKeyClasses() (campaign map[string]campaignKeyOwner, direct map[string]string, err error) {
	campaign = map[string]campaignKeyOwner{}
	direct = map[string]string{SmokeNumber: "the smoke test"}
	addCampaign := func(r Region, cs Case, from string) error {
		n := r.Number(cs)
		label := fmt.Sprintf("%s %s %s", from, r.Code, cs.ID)
		if prev, dup := campaign[n]; dup && prev.label != label && prev.region.Code != r.Code {
			return fmt.Errorf("%s and %s are the same number %s in two regions", prev.label, label, n)
		}
		campaign[n] = campaignKeyOwner{region: r, label: label}
		return nil
	}
	for _, r := range Regions {
		for _, cs := range AllCases {
			if err := addCampaign(r, cs, "campaign"); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, id := range []string{"K1", "K2"} {
		sc := CapacityScenarios[id]
		for _, r := range sc.Regions {
			for _, cs := range sc.Cases {
				if err := addCampaign(r, cs, id); err != nil {
					return nil, nil, err
				}
			}
		}
		for _, ds := range sc.Direct {
			direct[ds.DialKey()] = id + " direct call " + ds.Suffix
		}
	}
	for _, cs := range K3.CampaignCases() {
		if err := addCampaign(K3.Region, cs, "K3"); err != nil {
			return nil, nil, err
		}
	}
	for _, rc := range K3.Direct {
		direct[rc.DialKey(K3.Region)] = "K3 direct call " + rc.ID
	}
	for n, o := range campaign {
		if what, both := direct[n]; both {
			return nil, nil, fmt.Errorf("%s is dialled as %s and as a direct call (%s): a number is one or the other", n, o.label, what)
		}
	}
	return campaign, direct, nil
}

// ScenarioForForm rewrites the E164 scenario for a dial form: the key of every
// campaign contact becomes the string the dialer hands the switch in that form
// (Region.DialKey), and every other key (the direct calls, the smoke number)
// stays, since POST /calls dials its number as sent. The behaviours are copied
// as they are, and the output is one number per line in the input's order. It
// fails on a key it cannot classify and on two keys that would become one.
func ScenarioForForm(e164 []byte, form phonenum.DialForm) ([]byte, error) {
	campaign, direct, err := scenarioKeyClasses()
	if err != nil {
		return nil, err
	}
	keys, values, err := orderedScenarioNumbers(e164)
	if err != nil {
		return nil, err
	}
	var (
		b       bytes.Buffer
		written = map[string]string{}
		unknown []string
	)
	b.WriteString("{\n  \"numbers\": {\n")
	for i, key := range keys {
		out := key
		switch o, isCampaign := campaign[key]; {
		case isCampaign:
			var ok bool
			if out, ok = o.region.DialNumber(key, form); !ok && form != phonenum.FormE164 {
				return nil, fmt.Errorf("%s (%s) cannot be written in the form %s with country code %s", key, o.label, form, o.region.CountryCode)
			}
		case direct[key] != "":
		default:
			unknown = append(unknown, key)
			continue
		}
		if prev, dup := written[out]; dup {
			return nil, fmt.Errorf("%s and %s both become %s in the form %s", prev, key, out, form)
		}
		written[out] = key
		var compact bytes.Buffer
		if err := json.Compact(&compact, values[i]); err != nil {
			return nil, fmt.Errorf("the behaviours of %s: %w", key, err)
		}
		kb, _ := json.Marshal(out)
		fmt.Fprintf(&b, "    %s: %s,\n", kb, compact.String())
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("cannot classify %s: not a campaign contact or a direct call of any harness run (internal/e2e/scenario_forms.go)", strings.Join(unknown, ", "))
	}
	out := bytes.TrimSuffix(b.Bytes(), []byte(",\n"))
	return append(out, []byte("\n  }\n}\n")...), nil
}

// orderedScenarioNumbers reads the scenario's "numbers" object keeping its
// order, which a map would lose.
func orderedScenarioNumbers(data []byte) ([]string, []json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	expect := func(want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); !ok || d != want {
			return fmt.Errorf("scenario: expected %q, found %v", want, tok)
		}
		return nil
	}
	if err := expect('{'); err != nil {
		return nil, nil, err
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if name, _ := tok.(string); name != "numbers" {
		return nil, nil, fmt.Errorf("scenario: the only top-level field is numbers, found %v", tok)
	}
	if err := expect('{'); err != nil {
		return nil, nil, err
	}
	var keys []string
	var values []json.RawMessage
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("scenario: a number key is expected, found %v", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		keys, values = append(keys, key), append(values, raw)
	}
	if err := expect('}'); err != nil {
		return nil, nil, err
	}
	if err := expect('}'); err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, fmt.Errorf("scenario: trailing data after the object")
	}
	return keys, values, nil
}
