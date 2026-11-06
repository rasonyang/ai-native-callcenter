// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

var update = flag.Bool("update", false, "rewrite deploy/e2e/scenario.<FORM>.json from deploy/e2e/scenario.json")

const scenarioDir = "../../deploy/e2e"

// forEachForm runs a test once per dial form, each against the scenario file
// of that form.
func forEachForm(t *testing.T, fn func(t *testing.T, form phonenum.DialForm)) {
	t.Helper()
	for _, form := range DialForms {
		t.Run(string(form), func(t *testing.T) { fn(t, form) })
	}
}

func loadScenarioFor(t *testing.T, form phonenum.DialForm) *calleesim.Scenario {
	t.Helper()
	s, err := calleesim.LoadScenario(filepath.Join(scenarioDir, ScenarioFileName(form)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The two generated scenario files are what ScenarioForForm makes of the
// hand-edited one. Run with -update to rewrite them after editing
// deploy/e2e/scenario.json or a case table.
func TestScenarioForms(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(scenarioDir, ScenarioFileName(phonenum.FormE164)))
	if err != nil {
		t.Fatal(err)
	}
	// The source itself must be fully classified, as E164 leaves every key.
	if _, err := ScenarioForForm(source, phonenum.FormE164); err != nil {
		t.Fatal(err)
	}
	for _, form := range DialForms[1:] {
		want, err := ScenarioForForm(source, form)
		if err != nil {
			t.Fatalf("%s: %v", form, err)
		}
		path := filepath.Join(scenarioDir, ScenarioFileName(form))
		if *update {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v; generate it with: go test ./internal/e2e -run TestScenarioForms -update", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is not what deploy/e2e/scenario.json makes of the form %s; regenerate it with: go test ./internal/e2e -run TestScenarioForms -update",
				path, form)
		}
	}
}

func TestScenarioForFormRefusesWhatItCannotClassify(t *testing.T) {
	good := `{"numbers": {"+12025550103": [{"finalStatus": 486}]}}`
	if _, err := ScenarioForForm([]byte(good), phonenum.FormNational); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ in, want string }{
		"an unknown number":      {`{"numbers": {"+12025550103": [{}], "+19998887777": [{}]}}`, "cannot classify +19998887777"},
		"a duplicated key":       {`{"numbers": {"+12025550103": [{}], "+12025550103": [{}]}}`, "both become"},
		"a second top-level":     {`{"numbers": {}, "other": 1}`, "expected"},
		"a field before numbers": {`{"other": {}, "numbers": {}}`, "only top-level field"},
		"not an object":          {`[]`, "expected"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ScenarioForForm([]byte(tc.in), phonenum.FormE164NoPlus)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// The Request-URI user parts, written out: the US campaign's country code is 1
// and the Chinese one's 86, which is also the e2e deployment's own
// (AICC_OUTBOUND_DEFAULT_COUNTRY_CODE), so NATIONAL covers both sides of #477.
func TestRequestURIUserPartsPerForm(t *testing.T) {
	want := map[phonenum.DialForm][2]string{
		phonenum.FormE164:       {"+12025550103", "+8613988886603"},
		phonenum.FormE164NoPlus: {"12025550103", "8613988886603"},
		phonenum.FormNational:   {"2025550103", "13988886603"},
	}
	c03 := DefaultCases[0]
	if c03.ID != "C03" {
		t.Fatalf("DefaultCases[0] is %s", c03.ID)
	}
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		us, cn := Regions[0].DialKey(c03, form), Regions[1].DialKey(c03, form)
		if us != want[form][0] || cn != want[form][1] {
			t.Fatalf("C03 dials %q and %q, want %q and %q", us, cn, want[form][0], want[form][1])
		}
		// The scenario of the form answers exactly these strings.
		s := loadScenarioFor(t, form)
		for _, key := range []string{us, cn} {
			if len(s.Numbers[key]) != 1 {
				t.Errorf("the %s scenario has %d behaviours for %s, want 1", form, len(s.Numbers[key]), key)
			}
		}
		// The attempt and the contact keep the canonical number: only the
		// dial string follows the form.
		if n := Regions[0].Number(c03); n != "+12025550103" {
			t.Errorf("the US contact's number is %s", n)
		}
	})
}

// Every key of every form's scenario is distinct, and no campaign key of one
// region can be read as the other's: each belongs to its own region's dialled
// prefix and to nothing of the other's, and neither prefix starts the other.
func TestScenarioKeysStayApartInEveryForm(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		prefix := map[string]string{}
		for _, r := range Regions {
			prefix[r.Code] = dialPrefix(r, form)
		}
		if strings.HasPrefix(prefix["US"], prefix["CN"]) || strings.HasPrefix(prefix["CN"], prefix["US"]) {
			t.Fatalf("the dialled prefixes %v overlap", prefix)
		}
		seen := map[string]string{}
		check := func(label, key string, r Region) {
			t.Helper()
			if prev, dup := seen[key]; dup {
				t.Errorf("%s and %s dial the same string %s", prev, label, key)
			}
			seen[key] = label
			for _, other := range Regions {
				own := other.Code == r.Code
				if has := strings.HasPrefix(key, prefix[other.Code]); has != own {
					t.Errorf("%s (%s) has the prefix of %s: %v", label, key, other.Code, has)
				}
			}
		}
		for _, r := range Regions {
			for _, cs := range AllCases {
				check(r.Code+" "+cs.ID, r.DialKey(cs, form), r)
			}
		}
		for _, id := range []string{"K1", "K2"} {
			sc := CapacityScenarios[id]
			for _, r := range sc.Regions {
				for _, cs := range sc.Cases {
					check(id+" "+r.Code+" "+cs.ID, r.DialKey(cs, form), r)
				}
			}
		}
		for _, cs := range K3.CampaignCases() {
			check("K3 "+cs.ID, K3.Region.DialKey(cs, form), K3.Region)
		}
		// Every dialled key is in the form's file; the file holds nothing else
		// but the direct calls and the smoke number.
		s := loadScenarioFor(t, form)
		direct := map[string]bool{SmokeNumber: true}
		for _, ds := range K1.Direct {
			direct[ds.DialKey()] = true
		}
		for _, rc := range K3.Direct {
			direct[rc.DialKey(K3.Region)] = true
		}
		for key := range s.Numbers {
			if _, ok := seen[key]; !ok && !direct[key] {
				if len(key) > 0 {
					// The do-not-call C09 is never in the file; anything else is stray.
					t.Errorf("the %s scenario holds %s, which no case dials", form, key)
				}
			}
		}
		for key := range direct {
			if _, clash := seen[key]; clash {
				t.Errorf("%s is both a direct call and a campaign contact in the %s scenario", key, form)
			}
		}
	})
}

// A scenario left in the wrong form is refused, and the refusal names the form
// the deployment runs.
func TestScenarioInTheWrongFormIsRefused(t *testing.T) {
	for _, form := range DialForms {
		if err := CheckScenarioCoversCases(loadScenarioFor(t, form), Regions, AllCases, form); err != nil {
			t.Errorf("%s: the right file was refused: %v", form, err)
		}
		for _, other := range DialForms {
			if other == form {
				continue
			}
			err := CheckScenarioCoversCases(loadScenarioFor(t, other), Regions, AllCases, form)
			if err == nil {
				t.Errorf("the %s scenario passed for a deployment in %s", other, form)
				continue
			}
			if !strings.Contains(err.Error(), "AICC_OUTBOUND_DIAL_FORM="+string(form)+";") {
				t.Errorf("the refusal of the %s scenario for %s does not name the form: %v", other, form, err)
			}
		}
	}
}

// POST /calls dials its number exactly as sent; only the campaign dialer
// applies the form. So a direct call's key is its canonical number whatever
// the deployment's form is.
func TestDirectCallKeysAreTheNumberAsSent(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		s := loadScenarioFor(t, form)
		for _, id := range []string{"K1"} {
			for _, ds := range CapacityScenarios[id].Direct {
				if ds.DialKey() != ds.Number() || !phonenum.IsCanonical(ds.DialKey()) {
					t.Errorf("%s direct %s: key %q, number %q", id, ds.Suffix, ds.DialKey(), ds.Number())
				}
				if len(s.Numbers[ds.DialKey()]) != 1 {
					t.Errorf("the %s scenario has no behaviour for the direct call %s", form, ds.DialKey())
				}
			}
		}
		for _, rc := range K3.Direct {
			if rc.DialKey(K3.Region) != rc.Number(K3.Region) {
				t.Errorf("K3 %s: key %q, number %q", rc.ID, rc.DialKey(K3.Region), rc.Number(K3.Region))
			}
		}
		if len(s.Numbers[SmokeNumber]) != 1 {
			t.Errorf("the %s scenario does not answer the smoke number", form)
		}
	})
}

// The run-wide "no INVITE of one campaign carries the other country's number"
// check works in every form.
func TestAttributionSeparatesTheCountriesInEveryForm(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		c03 := DefaultCases[0]
		dids := []string{"95000001", "95000002"}
		build := func(usKey, cnKey string) (*RunView, []ScenarioResult) {
			v := &RunView{Form: form, Cases: []Case{c03}}
			var results []ScenarioResult
			for i, r := range Regions {
				v.Regions = append(v.Regions, RegionView{Region: r, DID: api.DID{Number: dids[i]}, Contacts: []ContactView{{Case: c03, Number: r.Number(c03)}}})
				results = append(results, ScenarioResult{Region: r.Code, Invites: 1})
			}
			dlg := func(did, key string) calleesim.DialogRecord {
				return calleesim.DialogRecord{Number: key, InviteHeaders: map[string]string{"from": "<sip:" + did + "@10.0.0.11>"}}
			}
			v.Dialogs = []calleesim.DialogRecord{dlg(dids[0], usKey), dlg(dids[1], cnKey)}
			return v, results
		}
		us, cn := Regions[0].DialKey(c03, form), Regions[1].DialKey(c03, form)
		v, results := build(us, cn)
		var ok Checks
		assessAttribution(&ok, v, form, results)
		if ok.Failed() {
			t.Fatalf("a correct run failed: %v", ok.failures)
		}
		// The US campaign's caller id on a Chinese contact's string: the
		// other campaign's INVITE. The wrong caller id and the foreign number
		// are both seen.
		v, results = build(cn, cn)
		var bad Checks
		assessAttribution(&bad, v, form, results)
		if !bad.Failed() {
			t.Fatal("an INVITE of the US campaign for the Chinese number passed")
		}
	})
}

// fakeC10 is a C10 contact as it should come out: attempt 1 busy, the number
// listed while the retry waited, attempt 2 skipped.
func fakeC10(t *testing.T) (RegionView, ContactView, []calleesim.DialogRecord, []calleesim.DialogRecord, map[string][]api.CDR, time.Time) {
	t.Helper()
	r := Regions[0]
	cs := caseByID(AllCases, "C10")
	base := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	did := "95000001"
	rv := RegionView{Region: r, DID: api.DID{Number: did}}
	busy, skipped := api.CampaignOutcome("BUSY"), api.CampaignOutcome(listedSkipOutcome)
	busyCause, dncCause := "USER_BUSY", listedSkipCause
	end1, end2 := at(1000), at(9000)
	nextAt := at(61000)
	a1 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
		Outcome: &busy, HangupCause: &busyCause, StartedAt: at(0), FinishedAt: &end1}
	a2 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
		Outcome: &skipped, HangupCause: &dncCause, StartedAt: at(9000), FinishedAt: &end2}
	campaignID := uuid.Must(uuid.NewV7())
	toNumber := r.DialKey(cs, phonenum.FormE164)
	cdrs := map[string][]api.CDR{a1.CallID.String(): {{CallID: a1.CallID, HangupCause: &busyCause, ToNumber: toNumber, DID: &did, CallType: "OUTBOUND", CampaignID: &campaignID}}}
	lastOutcome := skipped
	af := api.CampaignContact{Number: r.Number(cs), Status: "ACTIVE", Attempts: 1, NextAttemptAt: &nextAt, LastOutcome: &busy}
	final := api.CampaignContact{Number: r.Number(cs), Status: "DNC", Attempts: 1, LastOutcome: &lastOutcome}
	cv := ContactView{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a1, a2}, AfterFirst: &af, Contact: &final,
		Listing: &Listing{PostedAt: at(8990), ListedAt: at(9000), ConfirmedAt: at(9010), IsOnList: true, NextAttemptAt: nextAt}}
	dlg := calleesim.DialogRecord{Number: toNumber, InviteAt: at(100), InviteHeaders: map[string]string{"from": "<sip:" + did + "@10.0.0.11>"},
		Provisionals: []calleesim.ProvisionalRecord{{Status: 180}}, Final: &calleesim.FinalRecord{Status: 486, SentAt: at(400)}}
	ctl := calleesim.DialogRecord{InviteAt: at(100)}
	ctl2 := calleesim.DialogRecord{InviteAt: at(62000)}
	return rv, cv, []calleesim.DialogRecord{dlg}, []calleesim.DialogRecord{ctl, ctl2}, cdrs, at(70000)
}

func TestAssessListedMidRun(t *testing.T) {
	ok := func(mutate func(*ContactView, *[]calleesim.DialogRecord, *[]calleesim.DialogRecord, map[string][]api.CDR, *time.Time)) ScenarioResult {
		rv, cv, dlg, ctl, cdrs, read := fakeC10(t)
		if mutate != nil {
			mutate(&cv, &dlg, &ctl, cdrs, &read)
		}
		return assessListedMidRun(rv, cv, Regions[0].DialKey(cv.Case, phonenum.FormE164), dlg, ctl, cdrs, read)
	}
	if res := ok(nil); !res.Passed || len(res.Checks) < 20 {
		t.Fatalf("the designed run failed or made %d checks: %v", len(res.Checks), res.Failures)
	}
	type mut = func(*ContactView, *[]calleesim.DialogRecord, *[]calleesim.DialogRecord, map[string][]api.CDR, *time.Time)
	for name, spoil := range map[string]mut{
		"listed after the retry came due": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			cv.Listing.ListedAt = cv.Listing.NextAttemptAt.Add(time.Second)
		},
		"never listed": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			cv.Listing = nil
		},
		"not read back": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			cv.Listing.IsOnList = false
		},
		"dialled a second time": func(_ *ContactView, d, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			*d = append(*d, (*d)[0])
		},
		"a CDR for the skipped attempt": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, cdrs map[string][]api.CDR, _ *time.Time) {
			cdrs[cv.Attempts[1].CallID.String()] = []api.CDR{{CallID: cv.Attempts[1].CallID}}
		},
		"the contact is not DNC": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			cv.Contact.Status = "EXHAUSTED"
		},
		"the second attempt is a busy one": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			busy := api.CampaignOutcome("BUSY")
			cv.Attempts[1].Outcome = &busy
		},
		"the control was never redialled": func(_ *ContactView, _, c *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			*c = (*c)[:1]
		},
		"the control's retry came before C10's was due": func(_ *ContactView, _, c *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			(*c)[1].InviteAt = (*c)[1].InviteAt.Add(-time.Minute)
		},
		"the simulator was read before the retry was due": func(_ *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, read *time.Time) {
			*read = read.Add(-time.Minute)
		},
		"the skipped attempt came before the listing": func(cv *ContactView, _, _ *[]calleesim.DialogRecord, _ map[string][]api.CDR, _ *time.Time) {
			cv.Attempts[1].StartedAt = cv.Listing.PostedAt.Add(-time.Minute)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if res := ok(spoil); res.Passed {
				t.Fatalf("passed (%d checks)", len(res.Checks))
			}
		})
	}
}

// C10 is a case of the table, in the scenario of every form, with the one
// callee behaviour its step names.
func TestC10IsCoveredInEveryForm(t *testing.T) {
	cs := caseByID(AllCases, "C10")
	if cs.ID != "C10" || !cs.IsListedMidRun || cs.ControlID != "C03" || len(cs.Steps) != 1 {
		t.Fatalf("C10 is %+v", cs)
	}
	if ctl := caseByID(AllCases, cs.ControlID); ctl.ID != "C03" || ctl.IsOnDNC || ctl.IsListedMidRun || ctl.SIPStatus != cs.Steps[0].SIPStatus {
		t.Fatalf("C10's control is %+v", ctl)
	}
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		s := loadScenarioFor(t, form)
		for _, r := range Regions {
			if got := len(s.Numbers[r.DialKey(cs, form)]); got != 1 {
				t.Errorf("%s: %d behaviours for C10, want 1", r.Code, got)
			}
		}
		// A simulator that has already seen the number is refused: the second
		// INVITE, if it came, would meet no behaviour of its own.
		seen := calleesim.Report{Dialogs: []calleesim.DialogRecord{{Number: Regions[0].DialKey(cs, form)}}}
		if err := checkSimulatorUnused(seen, Regions, AllCases, form); err == nil || !strings.Contains(err.Error(), "US C10") {
			t.Errorf("a simulator that had seen C10's number: %v", err)
		}
	})
}
