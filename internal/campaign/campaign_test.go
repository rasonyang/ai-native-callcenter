// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// fakeRepo is an in-memory Repository: enough to test what the service decides
// (validation, field locks, which transitions exist) without a database. What
// the database decides (races, constraints, atomic imports) is tested against
// PostgreSQL in internal/store and internal/httpapi.
type fakeRepo struct {
	mu        sync.Mutex
	dids      map[uuid.UUID]DIDFlow
	campaigns map[uuid.UUID]Campaign
	contacts  map[uuid.UUID]map[string]uuid.UUID // campaign -> number -> contact id
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		dids:      map[uuid.UUID]DIDFlow{},
		campaigns: map[uuid.UUID]Campaign{},
		contacts:  map[uuid.UUID]map[string]uuid.UUID{},
	}
}

func (r *fakeRepo) DIDFlow(_ context.Context, id uuid.UUID) (DIDFlow, error) {
	d, ok := r.dids[id]
	if !ok {
		return DIDFlow{}, ErrDIDNotFound
	}
	return d, nil
}
func (r *fakeRepo) Create(_ context.Context, c Campaign) (Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.campaigns[c.ID] = c
	return c, nil
}
func (r *fakeRepo) Get(_ context.Context, id uuid.UUID) (Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.campaigns[id]
	if !ok {
		return Campaign{}, NewNotFound()
	}
	return c, nil
}
func (r *fakeRepo) List(context.Context, ListFilter) ([]Campaign, int64, error) { return nil, 0, nil }
func (r *fakeRepo) Update(_ context.Context, id uuid.UUID, change func(Campaign) (Campaign, error)) (Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.campaigns[id]
	if !ok {
		return Campaign{}, NewNotFound()
	}
	next, err := change(cur)
	if err != nil {
		return Campaign{}, err
	}
	r.campaigns[id] = next
	return next, nil
}
func (r *fakeRepo) Delete(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.campaigns[id]
	if !ok {
		return NewNotFound()
	}
	if c.Status != StatusDraft {
		return NewTransitionError(c.Status, "DELETE")
	}
	delete(r.campaigns, id)
	return nil
}
func (r *fakeRepo) move(id uuid.UUID, action string, from []Status, apply func(*Campaign)) (Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.campaigns[id]
	if !ok {
		return Campaign{}, NewNotFound()
	}
	if !slices.Contains(from, c.Status) {
		return Campaign{}, NewTransitionError(c.Status, action)
	}
	apply(&c)
	r.campaigns[id] = c
	return c, nil
}
func (r *fakeRepo) Start(_ context.Context, id uuid.UUID) (Campaign, error) {
	return r.move(id, "START", []Status{StatusDraft}, func(c *Campaign) { c.Status = StatusRunning })
}
func (r *fakeRepo) Pause(_ context.Context, id uuid.UUID) (Campaign, error) {
	return r.move(id, "PAUSE", []Status{StatusRunning}, func(c *Campaign) {
		c.Status, c.PauseReason = StatusPaused, PauseManual
	})
}
func (r *fakeRepo) Resume(_ context.Context, id uuid.UUID) (Campaign, error) {
	return r.move(id, "RESUME", []Status{StatusPaused}, func(c *Campaign) {
		c.Status, c.PauseReason = StatusRunning, ""
	})
}
func (r *fakeRepo) Cancel(_ context.Context, id uuid.UUID) (Campaign, error) {
	return r.move(id, "CANCEL", []Status{StatusDraft, StatusRunning, StatusPaused}, func(c *Campaign) {
		c.Status, c.PauseReason = StatusCancelled, ""
	})
}
func (r *fakeRepo) ImportContacts(_ context.Context, id uuid.UUID, in []NewContact) ([]Stored, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.contacts[id] == nil {
		r.contacts[id] = map[string]uuid.UUID{}
	}
	out := make([]Stored, len(in))
	for i, c := range in {
		if existing, ok := r.contacts[id][c.Number]; ok {
			out[i] = Stored{ID: existing}
			continue
		}
		r.contacts[id][c.Number] = c.ID
		out[i] = Stored{ID: c.ID, IsNew: true}
	}
	return out, nil
}
func (r *fakeRepo) ListContacts(context.Context, uuid.UUID, ContactFilter) ([]Contact, int64, error) {
	return nil, 0, nil
}

// Test flows.

func testSpec(t *testing.T, nodeTools []string, extra func(m map[string]any)) *flow.Spec {
	t.Helper()
	if nodeTools == nil {
		nodeTools = []string{}
	}
	m := map[string]any{
		"id": "t", "specVersion": "v2", "initialNode": "ask",
		"global": map[string]any{"persona": "A polite caller."},
		"nodes": map[string]any{
			"ask": map[string]any{
				"instruction": "Greet {slots.userData.name} about the {slots.userData.plan} plan.",
				"tools":       nodeTools,
			},
		},
	}
	if extra != nil {
		extra(m)
	}
	raw, _ := json.Marshal(m)
	spec, err := flow.Load(raw)
	if err != nil {
		t.Fatalf("test flow: %v", err)
	}
	return spec
}

type fixture struct {
	repo *fakeRepo
	svc  *Service
	did  uuid.UUID
}

func newFixture(t *testing.T, env Env) *fixture {
	t.Helper()
	repo := newFakeRepo()
	did := uuid.New()
	repo.dids[did] = DIDFlow{ID: did, Number: "95099", IsEnabled: true, Spec: testSpec(t, []string{"hangup"}, nil)}
	return &fixture{repo: repo, svc: NewService(repo, nil, env), did: did}
}

func (f *fixture) validNew() New {
	return New{
		Name: "Autumn follow-up", DIDID: f.did, TimeZone: "Asia/Shanghai",
		CallingWindow: CallingWindow{
			Days:   []string{"MON", "TUE", "WED", "THU", "FRI"},
			Ranges: []TimeRange{{Start: "09:00", End: "12:00"}, {Start: "14:00", End: "18:00"}},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func wantKind(t *testing.T, err error, kind error, rule string) *Error {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) || !errors.Is(err, kind) {
		t.Fatalf("error = %v, want a %v", err, kind)
	}
	if rule != "" && ce.Rule != rule {
		t.Errorf("rule = %q, want %q (%s)", ce.Rule, rule, ce.Message)
	}
	return ce
}

func TestCreateAppliesDefaultsAndCanonicalForm(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	in := f.validNew()
	in.Name = "  Autumn follow-up  "
	in.CallingWindow.Days = []string{"FRI", "MON", "WED"}
	in.CallingWindow.Ranges = []TimeRange{{Start: "14:00", End: "18:00"}, {Start: "09:00", End: "12:00"}}
	c, err := f.svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Status != StatusDraft || c.Name != "Autumn follow-up" || c.DIDNumber != "95099" {
		t.Errorf("campaign = %+v", c)
	}
	if c.MaxConcurrent != DefaultMaxConcurrent || c.RingTimeoutSec != DefaultRingTimeoutSec {
		t.Errorf("defaults = %d, %d", c.MaxConcurrent, c.RingTimeoutSec)
	}
	if !slices.Equal(c.CallingWindow.Days, []string{"MON", "WED", "FRI"}) ||
		c.CallingWindow.Ranges[0].Start != "09:00" || c.CallingWindow.Ranges[1].Start != "14:00" {
		t.Errorf("window not in canonical order: %+v", c.CallingWindow)
	}
	want := DefaultRetryPolicy()
	got, _ := json.Marshal(c.RetryPolicy)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("retry policy = %s, want the design's defaults %s", got, exp)
	}
	if want.MaxAttempts != 3 || want.MaxSystemRetries != 5 ||
		want.IntervalSec[OutcomeNoAnswer] != 7200 || want.IntervalSec[OutcomeBusy] != 1800 ||
		want.IntervalSec[OutcomeRejected] != 86400 || want.IntervalSec[OutcomeUnknown] != 86400 ||
		want.TransientBackoff != (Backoff{BaseSec: 300, Factor: 3, CapSec: 14400}) {
		t.Errorf("default retry policy is not the design's: %+v", want)
	}
}

func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	transferDID, disabledDID, flowlessDID := uuid.New(), uuid.New(), uuid.New()
	f.repo.dids[transferDID] = DIDFlow{ID: transferDID, Number: "95001", IsEnabled: true,
		Spec: testSpec(t, []string{"hangup", "transfer_to_agent"}, nil)}
	f.repo.dids[disabledDID] = DIDFlow{ID: disabledDID, Number: "95002", IsEnabled: false,
		Spec: testSpec(t, nil, nil)}
	f.repo.dids[flowlessDID] = DIDFlow{ID: flowlessDID, Number: "95003", IsEnabled: true}

	for _, tc := range []struct {
		name   string
		mutate func(*New)
		kind   error
		rule   string
		field  string
	}{
		{"empty name", func(n *New) { n.Name = "  " }, ErrValidation, "EMPTY", "name"},
		{"name over 200 characters", func(n *New) { n.Name = strings.Repeat("名", 201) }, ErrValidation, "TOO_LONG", "name"},
		{"unknown DID", func(n *New) { n.DIDID = uuid.New() }, ErrValidation, "DID_NOT_FOUND", "didId"},
		{"disabled DID", func(n *New) { n.DIDID = disabledDID }, ErrValidation, "DID_DISABLED", "didId"},
		{"DID without a published flow", func(n *New) { n.DIDID = flowlessDID }, ErrValidation, "DID_HAS_NO_PUBLISHED_FLOW", "didId"},
		{"a flow that can transfer", func(n *New) { n.DIDID = transferDID }, ErrFlowHasTransfer, "FLOW_CAN_TRANSFER", "didId"},
		{"unknown time zone", func(n *New) { n.TimeZone = "Mars/Olympus" }, ErrValidation, "TIME_ZONE_INVALID", "timeZone"},
		{"Local is not a place", func(n *New) { n.TimeZone = "Local" }, ErrValidation, "TIME_ZONE_INVALID", "timeZone"},
		{"no time zone", func(n *New) { n.TimeZone = "" }, ErrValidation, "TIME_ZONE_INVALID", "timeZone"},
		{"ring timeout under 5", func(n *New) { n.RingTimeoutSec = ptr(4) }, ErrValidation, "OUT_OF_RANGE", "ringTimeoutSec"},
		{"ring timeout at the answer window", func(n *New) { n.RingTimeoutSec = ptr(60) }, ErrRingTimeoutTooLong, "NOT_BELOW_ANSWER_WINDOW", "ringTimeoutSec"},
		{"ring timeout above the answer window", func(n *New) { n.RingTimeoutSec = ptr(90) }, ErrRingTimeoutTooLong, "", "ringTimeoutSec"},
		{"max concurrent zero is a value, not a default", func(n *New) { n.MaxConcurrent = ptr(0) }, ErrValidation, "OUT_OF_RANGE", "maxConcurrent"},
		{"ring timeout zero is a value, not a default", func(n *New) { n.RingTimeoutSec = ptr(0) }, ErrValidation, "OUT_OF_RANGE", "ringTimeoutSec"},
		{"max concurrent over the sanity bound", func(n *New) { n.MaxConcurrent = ptr(100001) }, ErrValidation, "OUT_OF_RANGE", "maxConcurrent"},
		{"negative max concurrent", func(n *New) { n.MaxConcurrent = ptr(-1) }, ErrValidation, "OUT_OF_RANGE", "maxConcurrent"},
		{"bad country code", func(n *New) { n.DefaultCountryCode = "+86" }, ErrValidation, "INVALID", "defaultCountryCode"},
		{"no days", func(n *New) { n.CallingWindow.Days = nil }, ErrValidation, "EMPTY", "callingWindow.days"},
		{"window outside the permitted hours", func(n *New) {
			n.CallingWindow.Ranges = []TimeRange{{Start: "07:00", End: "10:00"}}
		}, ErrWindowOutsideLimit, "OUTSIDE_PERMITTED_HOURS", "callingWindow.ranges[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := f.validNew()
			tc.mutate(&in)
			_, err := f.svc.Create(context.Background(), in)
			ce := wantKind(t, err, tc.kind, tc.rule)
			if ce.Field != tc.field {
				t.Errorf("field = %q, want %q", ce.Field, tc.field)
			}
		})
	}
	if len(f.repo.campaigns) != 0 {
		t.Errorf("a refused create stored %d campaigns", len(f.repo.campaigns))
	}
}

func TestCallingWindowAgainstTheOuterHours(t *testing.T) {
	t.Parallel()
	day := []string{"MON"}
	r := func(s, e string) CallingWindow { return CallingWindow{Days: day, Ranges: []TimeRange{{s, e}}} }
	outerDefault, _ := dayrange.Parse("08:00-21:00")
	outerNight, _ := dayrange.Parse("20:00-04:00")

	for _, tc := range []struct {
		name    string
		outer   dayrange.Range
		w       CallingWindow
		wantErr error
		rule    string
	}{
		{"the whole permitted day", outerDefault, r("08:00", "21:00"), nil, ""},
		{"inside", outerDefault, r("09:00", "17:00"), nil, ""},
		{"one minute early", outerDefault, r("07:59", "12:00"), ErrWindowOutsideLimit, ""},
		{"one minute late", outerDefault, r("12:00", "21:01"), ErrWindowOutsideLimit, ""},
		{"22:00-02:00 under daytime hours", outerDefault, r("22:00", "02:00"), ErrWindowOutsideLimit, ""},
		{"22:00-02:00 under night hours", outerNight, r("22:00", "02:00"), nil, ""},
		{"22:00-02:00 under night hours, written 24:00 style", outerNight, r("22:00", "24:00"), nil, ""},
		{"daytime under night hours", outerNight, r("09:00", "12:00"), ErrWindowOutsideLimit, ""},
		{"a range may be split in two", outerDefault,
			CallingWindow{Days: day, Ranges: []TimeRange{{"09:00", "12:00"}, {"13:00", "17:00"}}}, nil, ""},
		{"one range outside among good ones", outerDefault,
			CallingWindow{Days: day, Ranges: []TimeRange{{"09:00", "12:00"}, {"20:00", "22:00"}}}, ErrWindowOutsideLimit, ""},
		{"overlapping ranges", outerDefault,
			CallingWindow{Days: day, Ranges: []TimeRange{{"09:00", "12:00"}, {"11:00", "17:00"}}}, ErrValidation, "OVERLAP"},
		{"touching ranges are fine", outerDefault,
			CallingWindow{Days: day, Ranges: []TimeRange{{"09:00", "12:00"}, {"12:00", "17:00"}}}, nil, ""},
		{"an empty range", outerDefault, r("10:00", "10:00"), ErrValidation, "INVALID_TIME"},
		{"not a time", outerDefault, r("9am", "5pm"), ErrValidation, "INVALID_TIME"},
		{"unknown day", outerDefault, CallingWindow{Days: []string{"MONDAY"}, Ranges: r("09:00", "10:00").Ranges}, ErrValidation, "UNKNOWN_DAY"},
		{"a day twice", outerDefault, CallingWindow{Days: []string{"MON", "MON"}, Ranges: r("09:00", "10:00").Ranges}, ErrValidation, "DUPLICATE_DAY"},
		{"no ranges", outerDefault, CallingWindow{Days: day}, ErrValidation, "EMPTY"},
		{"nine ranges", outerDefault, CallingWindow{Days: day, Ranges: func() []TimeRange {
			var out []TimeRange
			for h := 8; h < 17; h++ {
				out = append(out, TimeRange{Start: dayrange.Format(h*60 + 0), End: dayrange.Format(h*60 + 30)})
			}
			return out
		}()}, ErrValidation, "TOO_MANY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.w.Normalize(tc.outer)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Normalize: %v", err)
				}
				if again, err := got.Normalize(tc.outer); err != nil || !slices.Equal(again.Days, got.Days) || len(again.Ranges) != len(got.Ranges) {
					t.Errorf("a normalised window does not normalise to itself: %+v %v", again, err)
				}
				return
			}
			wantKind(t, err, tc.wantErr, tc.rule)
		})
	}
}

func TestTimeZonesIncludeDaylightSavingOnes(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"America/New_York", "Europe/London", "Asia/Shanghai", "Australia/Lord_Howe", "UTC"} {
		loc, err := LoadZone(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// A zone with daylight saving has two offsets in a year; the loader
		// must have the rules, not a fixed offset.
		_, winter := time.Date(2026, 1, 15, 12, 0, 0, 0, loc).Zone()
		_, summer := time.Date(2026, 7, 15, 12, 0, 0, 0, loc).Zone()
		if name == "America/New_York" && (winter != -5*3600 || summer != -4*3600) {
			t.Errorf("America/New_York offsets = %d, %d, want -18000 and -14400", winter, summer)
		}
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus", "new york", "../../etc/passwd"} {
		if _, err := LoadZone(bad); err == nil {
			t.Errorf("LoadZone(%q) succeeded", bad)
		}
	}
}

func TestRetryPolicyNormalize(t *testing.T) {
	t.Parallel()
	good := DefaultRetryPolicy()
	good.RetryOutcomes = []Outcome{OutcomeBusy, OutcomeBusy, OutcomeTransient}
	good.IntervalSec = map[Outcome]int{OutcomeBusy: 600}
	got, err := good.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.RetryOutcomes, []Outcome{OutcomeBusy, OutcomeTransient}) {
		t.Errorf("retry outcomes = %v, want duplicates dropped, order kept", got.RetryOutcomes)
	}
	if got.IntervalSec[OutcomeBusy] != 600 || got.IntervalSec[OutcomeNoAnswer] != 7200 ||
		got.IntervalSec[OutcomeRejected] != 86400 || got.IntervalSec[OutcomeUnknown] != 86400 {
		t.Errorf("intervals = %v, want 600 for BUSY and the defaults for the rest", got.IntervalSec)
	}
	none := DefaultRetryPolicy()
	none.RetryOutcomes = nil
	if got, err := none.Normalize(); err != nil || got.RetryOutcomes == nil || len(got.RetryOutcomes) != 0 {
		t.Errorf("a campaign that retries nothing: %v %v", got.RetryOutcomes, err)
	}

	for name, mutate := range map[string]func(*RetryPolicy){
		"zero attempts":              func(p *RetryPolicy) { p.MaxAttempts = 0 },
		"21 attempts":                func(p *RetryPolicy) { p.MaxAttempts = 21 },
		"negative system retries":    func(p *RetryPolicy) { p.MaxSystemRetries = -1 },
		"51 system retries":          func(p *RetryPolicy) { p.MaxSystemRetries = 51 },
		"answered is not retryable":  func(p *RetryPolicy) { p.RetryOutcomes = []Outcome{OutcomeAnswered} },
		"DNC is not retryable":       func(p *RetryPolicy) { p.RetryOutcomes = []Outcome{OutcomeDNCSkipped} },
		"a made-up outcome":          func(p *RetryPolicy) { p.RetryOutcomes = []Outcome{"MAYBE"} },
		"an interval for TRANSIENT":  func(p *RetryPolicy) { p.IntervalSec = map[Outcome]int{OutcomeTransient: 300} },
		"an interval under a minute": func(p *RetryPolicy) { p.IntervalSec = map[Outcome]int{OutcomeBusy: 30} },
		"an interval over 30 days":   func(p *RetryPolicy) { p.IntervalSec = map[Outcome]int{OutcomeBusy: 31 * 24 * 3600} },
		"backoff factor 0":           func(p *RetryPolicy) { p.TransientBackoff = Backoff{BaseSec: 300, Factor: 0, CapSec: 900} },
		"backoff cap under base":     func(p *RetryPolicy) { p.TransientBackoff = Backoff{BaseSec: 300, Factor: 3, CapSec: 100} },
		"backoff factor 11":          func(p *RetryPolicy) { p.TransientBackoff = Backoff{BaseSec: 300, Factor: 11, CapSec: 900} },
	} {
		p := DefaultRetryPolicy()
		mutate(&p)
		if _, err := p.Normalize(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
}

// The state machine through the service: every action from every status. The
// campaign is placed in the status directly; the database-level race tests are
// in internal/httpapi.
func TestEveryActionFromEveryStatus(t *testing.T) {
	t.Parallel()
	statuses := []Status{StatusDraft, StatusRunning, StatusPaused, StatusCompleted, StatusCancelled}
	type result struct {
		to     Status
		reason PauseReason
	}
	ok := func(to Status, reason PauseReason) *result { return &result{to, reason} }
	table := map[string]map[Status]*result{
		"start":  {StatusDraft: ok(StatusRunning, "")},
		"pause":  {StatusRunning: ok(StatusPaused, PauseManual)},
		"resume": {StatusPaused: ok(StatusRunning, "")},
		"cancel": {StatusDraft: ok(StatusCancelled, ""), StatusRunning: ok(StatusCancelled, ""), StatusPaused: ok(StatusCancelled, "")},
	}
	for action, row := range table {
		for _, from := range statuses {
			t.Run(action+" from "+string(from), func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, Env{})
				c, err := f.svc.Create(context.Background(), f.validNew())
				if err != nil {
					t.Fatal(err)
				}
				f.repo.campaigns[c.ID] = withStatus(c, from)
				var got Campaign
				switch action {
				case "start":
					got, err = f.svc.Start(context.Background(), c.ID)
				case "pause":
					got, err = f.svc.Pause(context.Background(), c.ID)
				case "resume":
					got, err = f.svc.Resume(context.Background(), c.ID)
				case "cancel":
					got, err = f.svc.Cancel(context.Background(), c.ID)
				}
				want := row[from]
				if want == nil {
					ce := wantKind(t, err, ErrInvalidTransition, "")
					if ce.Params["status"] != string(from) {
						t.Errorf("the refusal does not name the status found: %v", ce.Params)
					}
					if f.repo.campaigns[c.ID].Status != from {
						t.Errorf("a refused %s changed the status to %s", action, f.repo.campaigns[c.ID].Status)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s from %s: %v", action, from, err)
				}
				if got.Status != want.to || got.PauseReason != want.reason {
					t.Errorf("after %s: %s/%q, want %s/%q", action, got.Status, got.PauseReason, want.to, want.reason)
				}
			})
		}
	}
	// Nothing in the API completes a campaign: COMPLETED is the dialer's.
	f := newFixture(t, Env{})
	if _, err := f.svc.Start(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown campaign: %v, want ErrNotFound", err)
	}
}

func withStatus(c Campaign, s Status) Campaign {
	c.Status = s
	if s == StatusPaused {
		c.PauseReason = PauseManual
	}
	return c
}

func TestStartChecksTheCampaignAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	c, _ := f.svc.Create(context.Background(), f.validNew())

	// The DID's flow gained a transfer between create and start.
	f.repo.dids[f.did] = DIDFlow{ID: f.did, Number: "95099", IsEnabled: true,
		Spec: testSpec(t, []string{"transfer_to_agent"}, nil)}
	_, err := f.svc.Start(context.Background(), c.ID)
	wantKind(t, err, ErrFlowHasTransfer, "")
	if f.repo.campaigns[c.ID].Status != StatusDraft {
		t.Error("a refused start left the DRAFT running")
	}

	// Or the DID was disabled.
	f.repo.dids[f.did] = DIDFlow{ID: f.did, Number: "95099", IsEnabled: false, Spec: testSpec(t, nil, nil)}
	_, err = f.svc.Start(context.Background(), c.ID)
	wantKind(t, err, ErrValidation, "DID_DISABLED")

	// Or the outer hours were narrowed under it.
	f2 := newFixture(t, Env{})
	c2, _ := f2.svc.Create(context.Background(), f2.validNew())
	narrow, _ := dayrange.Parse("10:00-16:00")
	f2.svc.env.OutboundHours = narrow
	_, err = f2.svc.Start(context.Background(), c2.ID)
	wantKind(t, err, ErrWindowOutsideLimit, "")
}

func TestFieldLocksPerStatus(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	newDID := uuid.New()

	patches := map[string]Patch{
		"name":               {Name: str("Renamed")},
		"didId":              {DIDID: &newDID},
		"timeZone":           {TimeZone: str("America/New_York")},
		"defaultCountryCode": {DefaultCountryCode: str("1")},
		"ringTimeoutSec":     {RingTimeoutSec: num(20)},
		"followUpQueueId":    {FollowUpQueueID: func() *uuid.UUID { u := uuid.New(); return &u }()},
		"maxConcurrent":      {MaxConcurrent: num(9)},
		"callingWindow":      {CallingWindow: &CallingWindow{Days: []string{"SAT"}, Ranges: []TimeRange{{"10:00", "11:00"}}}},
		"retryPolicy":        {RetryPolicy: func() *RetryPolicy { p := DefaultRetryPolicy(); p.MaxAttempts = 7; return &p }()},
	}
	openFields := map[string]bool{"maxConcurrent": true, "callingWindow": true, "retryPolicy": true}

	for _, status := range []Status{StatusDraft, StatusRunning, StatusPaused, StatusCompleted, StatusCancelled} {
		for field, patch := range patches {
			t.Run(string(status)+"/"+field, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, Env{})
				f.repo.dids[newDID] = DIDFlow{ID: newDID, Number: "95100", IsEnabled: true, Spec: testSpec(t, nil, nil)}
				c, err := f.svc.Create(context.Background(), f.validNew())
				if err != nil {
					t.Fatal(err)
				}
				f.repo.campaigns[c.ID] = withStatus(c, status)

				got, err := f.svc.Update(context.Background(), c.ID, patch)
				allowed := status == StatusDraft || (!status.IsTerminal() && openFields[field])
				if allowed {
					if err != nil {
						t.Fatalf("patching %s in %s: %v", field, status, err)
					}
					if status != StatusDraft && got.Status != status {
						t.Errorf("a patch changed the status to %s", got.Status)
					}
					return
				}
				ce := wantKind(t, err, ErrFieldLocked, "")
				if fields, _ := ce.Params["fields"].([]string); !slices.Equal(fields, []string{field}) {
					t.Errorf("locked fields = %v, want [%s]", ce.Params["fields"], field)
				}
				if stored := f.repo.campaigns[c.ID]; stored.Name != c.Name || stored.TimeZone != c.TimeZone {
					t.Errorf("a refused patch changed the stored campaign")
				}
			})
		}
	}
}

func TestAPatchThatChangesNothingIsNotLocked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	c, _ := f.svc.Create(context.Background(), f.validNew())
	f.repo.campaigns[c.ID] = withStatus(c, StatusRunning)

	// The whole form sent back, one open field changed: the locked fields are
	// repeated unchanged and must not be refused.
	n := 12
	same := c.Name
	tz := c.TimeZone
	did := c.DIDID
	win := CallingWindow{
		Days:   []string{"FRI", "MON", "THU", "TUE", "WED"}, // another order, same meaning
		Ranges: []TimeRange{{"14:00", "18:00"}, {"09:00", "12:00"}},
	}
	got, err := f.svc.Update(context.Background(), c.ID, Patch{
		Name: &same, TimeZone: &tz, DIDID: &did, MaxConcurrent: &n, CallingWindow: &win,
	})
	if err != nil {
		t.Fatalf("a form sent back whole: %v", err)
	}
	if got.MaxConcurrent != 12 {
		t.Errorf("maxConcurrent = %d, want 12", got.MaxConcurrent)
	}
}

// A campaign written under yesterday's outer hours still accepts a change to a
// field the hours do not touch; narrowing a window on a running campaign is a
// change like any other and is held to the hours.
func TestPatchValidatesOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	c, _ := f.svc.Create(context.Background(), f.validNew())
	f.repo.campaigns[c.ID] = withStatus(c, StatusRunning)

	narrow, _ := dayrange.Parse("10:00-11:00")
	f.svc.env.OutboundHours = narrow // the stored 09:00-12:00 / 14:00-18:00 is now outside

	n := 3
	if _, err := f.svc.Update(context.Background(), c.ID, Patch{MaxConcurrent: &n}); err != nil {
		t.Errorf("maxConcurrent under narrowed hours: %v", err)
	}
	win := CallingWindow{Days: []string{"MON"}, Ranges: []TimeRange{{"09:00", "10:30"}}}
	_, err := f.svc.Update(context.Background(), c.ID, Patch{CallingWindow: &win})
	wantKind(t, err, ErrWindowOutsideLimit, "")
	win = CallingWindow{Days: []string{"MON"}, Ranges: []TimeRange{{"10:15", "10:45"}}}
	if _, err := f.svc.Update(context.Background(), c.ID, Patch{CallingWindow: &win}); err != nil {
		t.Errorf("narrowing the window of a running campaign inside the hours: %v", err)
	}
}

func TestPatchClearsTheTwoOptionalFields(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{})
	in := f.validNew()
	q := uuid.New()
	in.FollowUpQueueID = &q
	in.DefaultCountryCode = "86"
	c, err := f.svc.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Update(context.Background(), c.ID, Patch{ClearDefaultCountryCode: true, ClearFollowUpQueue: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultCountryCode != "" || got.FollowUpQueueID != nil {
		t.Errorf("not cleared: %q %v", got.DefaultCountryCode, got.FollowUpQueueID)
	}
}

func TestRingTimeoutFollowsTheAnswerWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t, Env{AnswerWindow: 45 * time.Second})
	in := f.validNew()
	in.RingTimeoutSec = ptr(44)
	if _, err := f.svc.Create(context.Background(), in); err != nil {
		t.Errorf("44 s under a 45 s window: %v", err)
	}
	in.RingTimeoutSec = ptr(45)
	_, err := f.svc.Create(context.Background(), in)
	ce := wantKind(t, err, ErrRingTimeoutTooLong, "")
	if ce.Params["answerWindowSec"] != 45 {
		t.Errorf("params = %v", ce.Params)
	}
}

func TestDeleteOnlyADraft(t *testing.T) {
	t.Parallel()
	for _, status := range []Status{StatusDraft, StatusRunning, StatusPaused, StatusCompleted, StatusCancelled} {
		f := newFixture(t, Env{})
		c, _ := f.svc.Create(context.Background(), f.validNew())
		f.repo.campaigns[c.ID] = withStatus(c, status)
		err := f.svc.Delete(context.Background(), c.ID)
		if status == StatusDraft {
			if err != nil {
				t.Errorf("deleting a DRAFT: %v", err)
			}
			continue
		}
		wantKind(t, err, ErrInvalidTransition, "")
	}
}
