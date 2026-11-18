// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func importFixture(t *testing.T) (*fixture, Campaign) {
	t.Helper()
	f := newFixture(t, Env{DefaultCountryCode: "86"})
	c, err := f.svc.Create(context.Background(), f.validNew())
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestARowIsRejectedForTheFirstThingWrongWithIt(t *testing.T) {
	t.Parallel()
	declared := []string{"name", "plan"}
	tooLong := strings.Repeat("长", MaxSlotValueRunes+1)
	var many = map[string]string{}
	for i := 0; i < MaxSlotsPerRow+1; i++ {
		many["undeclared"+strings.Repeat("x", i)] = "v"
	}

	for _, tc := range []struct {
		name string
		row  ImportRow
		want RowCode
		det  string
	}{
		{"fine", ImportRow{Number: "138 0013 8000", Slots: map[string]string{"name": "王"}}, "", ""},
		{"empty", ImportRow{Number: ""}, RowNumberEmpty, ""},
		{"only separators", ImportRow{Number: " - ( ) . "}, RowNumberEmpty, ""},
		{"letters", ImportRow{Number: "1380013abcd"}, RowNumberInvalid, ""},
		{"number beats everything", ImportRow{Number: "12", TimeZone: "Nope", Slots: map[string]string{"zzz": "v"}}, RowNumberInvalid, ""},
		{"time zone beats slots", ImportRow{Number: "13800138000", TimeZone: "Nope", Slots: map[string]string{"zzz": "v"}}, RowTimeZoneInvalid, "Nope"},
		{"too many slots beats unknown", ImportRow{Number: "13800138000", Slots: many}, RowSlotsTooMany, ""},
		{"unknown beats too long", ImportRow{Number: "13800138000", Slots: map[string]string{"zzz": tooLong, "name": tooLong}}, RowSlotUnknown, "zzz"},
		{"too long", ImportRow{Number: "13800138000", Slots: map[string]string{"name": tooLong}}, RowSlotTooLong, "name"},
		{"at the cap", ImportRow{Number: "13800138000", Slots: map[string]string{"name": strings.Repeat("长", MaxSlotValueRunes)}}, "", ""},
		{"an unknown slot with an empty value is not provided", ImportRow{Number: "13800138000", Slots: map[string]string{"zzz": "  "}}, "", ""},
	} {
		_, code, detail := checkRow(tc.row, "86", declared)
		if code != tc.want || detail != tc.det {
			t.Errorf("%s: code %q detail %q, want %q %q", tc.name, code, detail, tc.want, tc.det)
		}
	}
}

func TestSlotValuesAreCleanedLikeTheFlowsOwn(t *testing.T) {
	t.Parallel()
	got := cleanSlots(map[string]string{
		"a": "  line one\nline two\r\n\tindented  ",
		"b": "   ",
		"c": "😀 emoji",
	})
	if got["a"] != "line one line two   indented" || got["c"] != "😀 emoji" {
		t.Errorf("cleaned = %q", got)
	}
	if _, kept := got["b"]; kept {
		t.Error("a blank value is not a provided slot")
	}
}

func TestImportRefusesAnEmptyOrOversizedRequestAndATerminalCampaign(t *testing.T) {
	t.Parallel()
	f, c := importFixture(t)
	ctx := context.Background()

	_, err := f.svc.ImportContacts(ctx, c.ID, nil)
	wantKind(t, err, ErrValidation, "EMPTY")

	rows := make([]ImportRow, MaxImportRows+1)
	for i := range rows {
		rows[i] = ImportRow{Number: "13800138000"}
	}
	_, err = f.svc.ImportContacts(ctx, c.ID, rows)
	ce := wantKind(t, err, ErrValidation, "TOO_MANY_ROWS")
	if ce.Field != "contacts" {
		t.Errorf("field = %q", ce.Field)
	}
	if _, err := f.svc.ImportContacts(ctx, c.ID, rows[:MaxImportRows]); err != nil {
		t.Errorf("exactly %d rows: %v", MaxImportRows, err)
	}

	for _, status := range []Status{StatusCompleted, StatusCancelled} {
		f.repo.campaigns[c.ID] = withStatus(c, status)
		_, err := f.svc.ImportContacts(ctx, c.ID, rows[:1])
		wantKind(t, err, ErrInvalidTransition, "")
	}
	for _, status := range []Status{StatusDraft, StatusRunning, StatusPaused} {
		f.repo.campaigns[c.ID] = withStatus(c, status)
		if _, err := f.svc.ImportContacts(ctx, c.ID, rows[:1]); err != nil {
			t.Errorf("import into %s: %v", status, err)
		}
	}
	if _, err := f.svc.ImportContacts(ctx, uuid.Must(uuid.NewV7()), rows[:1]); err == nil {
		t.Error("an unknown campaign")
	}
}

func TestACampaignsCountryCodeBeatsTheDeployments(t *testing.T) {
	t.Parallel()
	f, c := importFixture(t) // deployment: 86
	ctx := context.Background()
	us := c
	us.DefaultCountryCode = "1"
	f.repo.campaigns[c.ID] = us

	res, err := f.svc.ImportContacts(ctx, c.ID, []ImportRow{{Number: "(415) 555-0132"}, {Number: "415 555 0132"}, {Number: "+14155550132"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || res.Duplicates != 2 || res.Rows[0].Number != "+14155550132" ||
		res.Rows[1].ContactID != res.Rows[0].ContactID || res.Rows[2].ContactID != res.Rows[0].ContactID {
		t.Errorf("result = %+v", res)
	}
}

// A listing's phoneNumber is read as an imported row's number is, with the
// campaign's own country code and else the deployment's, and the repository is
// handed the canonical number to match exactly. A value that is not a number is
// refused as an unreadable parameter and never reaches the repository.
func TestAListingsPhoneNumberIsReadWithTheCampaignsCountryCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, Env{DefaultCountryCode: "1"})
	own := f.validNew()
	own.DefaultCountryCode = "86"
	cn, err := f.svc.Create(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	us, err := f.svc.Create(ctx, f.validNew()) // no code of its own: the deployment's
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		campaign Campaign
		typed    string
		want     string
	}{
		{cn, "13800138000", "+8613800138000"},
		{cn, "138 0013-8000", "+8613800138000"},
		{cn, "+86 138 0013 8000", "+8613800138000"},
		{cn, "008613800138000", "+8613800138000"},
		{cn, "010 6123 4567", "+861061234567"}, // the landline's trunk 0 is dropped
		{cn, "+12025550143", "+12025550143"},   // another country, as it is
		{us, "(202) 555-0143", "+12025550143"},
		{cn, "", ""}, // no narrowing
	} {
		f.repo.contactFilter, f.repo.attemptFilter = ContactFilter{}, AttemptFilter{}
		if _, _, err := f.svc.ListContacts(ctx, tc.campaign.ID, ContactFilter{PhoneNumber: tc.typed, LeadIntent: LeadHigh}); err != nil {
			t.Fatalf("contacts %q: %v", tc.typed, err)
		}
		if got := f.repo.contactFilter; got.PhoneNumber != tc.want || got.LeadIntent != LeadHigh {
			t.Errorf("contacts %q: the repository got %+v, want %q with the lead kept", tc.typed, got, tc.want)
		}
		if _, _, err := f.svc.ListAttempts(ctx, tc.campaign.ID, AttemptFilter{PhoneNumber: tc.typed, Outcome: OutcomeBusy}); err != nil {
			t.Fatalf("attempts %q: %v", tc.typed, err)
		}
		if got := f.repo.attemptFilter; got.PhoneNumber != tc.want || got.Outcome != OutcomeBusy {
			t.Errorf("attempts %q: the repository got %+v, want %q with the outcome kept", tc.typed, got, tc.want)
		}
	}

	for _, tc := range []struct{ typed, rule string }{
		{"abc", "NUMBER_INVALID"},
		{"138", "NUMBER_INVALID"},
		{"0013800138000x", "NUMBER_INVALID"},
		{"+0123456789", "NUMBER_INVALID"},
		{"1234567890123456789", "NUMBER_INVALID"},
		{" - ", "NUMBER_EMPTY"},
	} {
		f.repo.contactFilter, f.repo.attemptFilter = ContactFilter{Status: "untouched"}, AttemptFilter{Outcome: "untouched"}
		_, _, err := f.svc.ListContacts(ctx, cn.ID, ContactFilter{PhoneNumber: tc.typed})
		if ce := wantKind(t, err, ErrUnreadableParameter, tc.rule); ce.Field != "phoneNumber" {
			t.Errorf("contacts %q: field %q", tc.typed, ce.Field)
		}
		_, _, err = f.svc.ListAttempts(ctx, cn.ID, AttemptFilter{PhoneNumber: tc.typed})
		if ce := wantKind(t, err, ErrUnreadableParameter, tc.rule); ce.Field != "phoneNumber" {
			t.Errorf("attempts %q: field %q", tc.typed, ce.Field)
		}
		if f.repo.contactFilter.Status != "untouched" || f.repo.attemptFilter.Outcome != "untouched" {
			t.Errorf("%q reached the repository", tc.typed)
		}
	}

	// An unknown campaign is not found before the number is read.
	if _, _, err := f.svc.ListContacts(ctx, uuid.Must(uuid.NewV7()), ContactFilter{PhoneNumber: "abc"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
}
