// SPDX-License-Identifier: Apache-2.0

package leadjudge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
)

// fakeStore is one owed judgement, with the store's rules: a verdict is
// written only while it is PENDING and before its deadline, the lead it leaves
// is campaign.LeadAfterVerdict's, and the sweep settles it NOT_JUDGED past the
// deadline.
type fakeStore struct {
	mu      sync.Mutex
	j       store.LeadJudgement
	lead    campaign.LeadIntent
	lines   []store.TranscriptLine
	reason  string
	writes  int
	reads   int
	applied chan campaign.LeadVerdict
}

func newFakeStore(declared campaign.LeadIntent, lang string, deadlineIn time.Duration, lines ...store.TranscriptLine) *fakeStore {
	now := time.Now()
	return &fakeStore{
		j: store.LeadJudgement{
			CallID: uuid.Must(uuid.NewV7()), Verdict: campaign.VerdictPending,
			DeclaredLeadIntent: declared, LeadNotes: "Agreed to the offer", Language: lang,
			DeadlineAt: now.Add(deadlineIn),
		},
		lead: declared, lines: lines, applied: make(chan campaign.LeadVerdict, 4),
	}
}

func (f *fakeStore) LeadJudgement(_ context.Context, callID uuid.UUID) (store.LeadJudgement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if callID != f.j.CallID {
		return store.LeadJudgement{}, campaign.ErrNoAttempt
	}
	return f.j, nil
}

func (f *fakeStore) ListTranscript(context.Context, uuid.UUID) ([]store.TranscriptLine, error) {
	return f.lines, nil
}

func (f *fakeStore) ApplyLeadVerdict(_ context.Context, callID uuid.UUID, v campaign.LeadVerdict, reason string) (campaign.LeadIntent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if callID != f.j.CallID || f.j.Verdict != campaign.VerdictPending || !time.Now().Before(f.j.DeadlineAt) {
		return "", false, nil
	}
	f.j.Verdict, f.reason = v, reason
	f.lead = campaign.LeadAfterVerdict(f.j.DeclaredLeadIntent, v)
	f.applied <- v
	return f.lead, true, nil
}

func (f *fakeStore) ExpireLeadJudgements(context.Context, int) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.j.Verdict != campaign.VerdictPending || time.Now().Before(f.j.DeadlineAt) {
		return nil, nil
	}
	f.j.Verdict = campaign.VerdictNotJudged
	f.applied <- campaign.VerdictNotJudged
	return []uuid.UUID{f.j.CallID}, nil
}

func (f *fakeStore) PendingLeadJudgements(context.Context, int) ([]store.PendingLeadJudgement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.j.Verdict != campaign.VerdictPending || !time.Now().Before(f.j.DeadlineAt) {
		return nil, nil
	}
	return []store.PendingLeadJudgement{{CallID: f.j.CallID, DeadlineAt: f.j.DeadlineAt}}, nil
}

func (f *fakeStore) state() (campaign.LeadVerdict, campaign.LeadIntent, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.j.Verdict, f.lead, f.reason
}

// fakeLLM answers each request with the next of its replies (the last one
// repeats) and keeps the requests.
type fakeLLM struct {
	mu      sync.Mutex
	replies []func(ctx context.Context) (string, error)
	reqs    []textllm.Request
}

func (l *fakeLLM) Complete(ctx context.Context, req textllm.Request) (string, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, req)
	reply := l.replies[min(len(l.reqs), len(l.replies))-1]
	l.mu.Unlock()
	return reply(ctx)
}

func (l *fakeLLM) requests() []textllm.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]textllm.Request(nil), l.reqs...)
}

func says(s string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return s, nil }
}

func fails(err error) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return "", err }
}

// hangs answers only when the job gives up on it.
func hangs(ctx context.Context) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func text(speaker, s string) store.TranscriptLine {
	return store.TranscriptLine{Speaker: speaker, Kind: store.TranscriptKindText, Content: map[string]any{"text": s}}
}

func bot(s string) store.TranscriptLine    { return text(store.SpeakerBot, s) }
func callee(s string) store.TranscriptLine { return text(store.SpeakerCustomer, s) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// bothLeads is v0.9.2's list, which judges every lead a judge can read.
var bothLeads = campaign.LeadJudgeIntents{campaign.LeadHigh, campaign.LeadMedium}

// newTestService is a service that judges HIGH and MEDIUM, reads at once and
// keeps a short margin.
func newTestService(st Store, llm textllm.Client) *Service {
	s := New(st, llm, Config{Timeout: 10 * time.Second, BudgetChars: DefaultBudgetChars, Intents: bothLeads}, quiet())
	s.settle, s.margin = 0, 50*time.Millisecond
	return s
}

// judgeOnce wakes the service for the store's call and waits for the job.
func judgeOnce(t *testing.T, s *Service, st *fakeStore) {
	t.Helper()
	s.AttemptFinished(campaign.Finished{CallID: st.j.CallID, IsLeadJudgementOwed: true})
	s.Wait()
}

var acceptedCall = []store.TranscriptLine{
	bot("Hello, we are offering a new data plan at half price this month. Would you like it?"),
	callee("How much is it per month?"),
	bot("Ten dollars a month."),
	callee("OK, sign me up for it."),
}

func TestAVerdictIsReadFromTheModelAndWritten(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		reply    string
		verdict  campaign.LeadVerdict
		declared campaign.LeadIntent
		lead     campaign.LeadIntent
	}{
		{`{"verdict":"CONFIRMED","reason":"The callee said: sign me up."}`, campaign.VerdictConfirmed, campaign.LeadHigh, campaign.LeadHigh},
		{"```json\n{\"verdict\":\"UNDECIDED\",\"reason\":\"Only asked a price.\"}\n```", campaign.VerdictUndecided, campaign.LeadHigh, campaign.LeadMedium},
		{`{"verdict":"undecided","reason":"x"}`, campaign.VerdictUndecided, campaign.LeadMedium, campaign.LeadMedium},
		{`{"verdict":"REFUSED","reason":"Declined."}`, campaign.VerdictRefused, campaign.LeadHigh, campaign.LeadNone},
		{`{"verdict":"MACHINE","reason":"A voicemail prompt answered."}`, campaign.VerdictMachine, campaign.LeadMedium, campaign.LeadNone},
		{`{"verdict":"NO_EVIDENCE","reason":"Only a greeting."}`, campaign.VerdictNoEvidence, campaign.LeadHigh, campaign.LeadNone},
	} {
		st := newFakeStore(c.declared, "en", 10*time.Second, acceptedCall...)
		llm := &fakeLLM{replies: []func(context.Context) (string, error){says(c.reply)}}
		judgeOnce(t, newTestService(st, llm), st)
		verdict, lead, reason := st.state()
		if verdict != c.verdict || lead != c.lead || reason == "" {
			t.Errorf("%s: verdict %s lead %s reason %q, want %s and %s", c.reply, verdict, lead, reason, c.verdict, c.lead)
		}
		if n := len(llm.requests()); n != 1 {
			t.Errorf("%s: %d requests, want 1", c.reply, n)
		}
	}
}

// Every answer the job cannot use is no verdict: after the one retry the
// judgement is NOT_JUDGED and the declared lead stands.
func TestAnUnusableAnswerLeavesTheDeclaredLeadNotJudged(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]func(context.Context) (string, error){
		"malformed":              says(`verdict: CONFIRMED`),
		"empty":                  says(""),
		"blank":                  says("   "),
		"unknown verdict":        says(`{"verdict":"MAYBE","reason":"?"}`),
		"the platform's verdict": says(`{"verdict":"NOT_JUDGED","reason":"?"}`),
		"pending":                says(`{"verdict":"PENDING"}`),
		"no verdict":             says(`{"reason":"I think they agreed."}`),
		"transport error":        fails(errors.New("textllm: status 502: bad gateway")),
	} {
		for _, declared := range []campaign.LeadIntent{campaign.LeadHigh, campaign.LeadMedium} {
			st := newFakeStore(declared, "en", 10*time.Second, acceptedCall...)
			llm := &fakeLLM{replies: []func(context.Context) (string, error){reply}}
			judgeOnce(t, newTestService(st, llm), st)
			verdict, lead, _ := st.state()
			if verdict != campaign.VerdictNotJudged || lead != declared {
				t.Errorf("%s (%s): verdict %s lead %s, want NOT_JUDGED and the declared lead", name, declared, verdict, lead)
			}
			if n := len(llm.requests()); n != 2 {
				t.Errorf("%s (%s): %d requests, want 2 (one retry)", name, declared, n)
			}
		}
	}
}

// A model slower than the job's bound gets one request, no retry, and the job
// writes NOT_JUDGED while the judgement's deadline is still ahead. The bound
// here is AICC_TEXT_LLM_TIMEOUT's, which starts with the job, so the test does
// not depend on when the job is scheduled; the deadline bounds the same
// context.
func TestALateAnswerIsNotJudgedAndTheDeclaredLeadStands(t *testing.T) {
	t.Parallel()
	st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
	llm := &fakeLLM{replies: []func(context.Context) (string, error){hangs}}
	s := newTestService(st, llm)
	s.cfg.Timeout = time.Second
	judgeOnce(t, s, st)
	verdict, lead, _ := st.state()
	if verdict != campaign.VerdictNotJudged || lead != campaign.LeadHigh {
		t.Errorf("verdict %s lead %s, want NOT_JUDGED HIGH", verdict, lead)
	}
	if n := len(llm.requests()); n != 1 {
		t.Errorf("%d requests, want 1: no time is left for a retry", n)
	}
}

// An answer that arrives after the deadline is discarded: nothing the job
// writes lands, and the sweep settles the judgement as NOT_JUDGED.
func TestAVerdictAfterTheDeadlineIsDiscarded(t *testing.T) {
	t.Parallel()
	st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
	// The deadline passes while the model works, and the model answers anyway.
	late := func(context.Context) (string, error) {
		st.mu.Lock()
		st.j.DeadlineAt = time.Now().Add(-time.Millisecond)
		st.mu.Unlock()
		return `{"verdict":"REFUSED","reason":"Declined."}`, nil
	}
	llm := &fakeLLM{replies: []func(context.Context) (string, error){late}}
	s := newTestService(st, llm)
	judgeOnce(t, s, st)
	if verdict, _, _ := st.state(); verdict != campaign.VerdictPending {
		t.Fatalf("verdict %s written after the deadline", verdict)
	}
	if st.writes != 1 {
		t.Errorf("%d writes, want the one that was refused", st.writes)
	}
	s.sweep()
	verdict, lead, _ := st.state()
	if verdict != campaign.VerdictNotJudged || lead != campaign.LeadHigh {
		t.Errorf("after the sweep: verdict %s lead %s, want NOT_JUDGED HIGH", verdict, lead)
	}
}

// A call the transcript holds no callee words for is NO_EVIDENCE, decided
// without asking the model.
func TestNoCalleeLineIsNoEvidenceWithoutARequest(t *testing.T) {
	t.Parallel()
	for name, lines := range map[string][]store.TranscriptLine{
		"no lines at all": nil,
		"only the bot":    {bot("您好，我们这个月有半价流量套餐，您需要吗？"), bot("那我先不打扰您了。")},
		"blank callee lines and tool rows": {
			bot("您好，我们这个月有半价流量套餐，您需要吗？"),
			callee("   "),
			{Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolCall,
				Content: map[string]any{"name": "record_lead", "args": `{"intent":"HIGH","notes":"客户同意办理"}`}},
			{Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolResult, Content: map[string]any{"ok": "1"}},
		},
	} {
		st := newFakeStore(campaign.LeadHigh, "zh", 10*time.Second, lines...)
		llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"CONFIRMED"}`)}}
		judgeOnce(t, newTestService(st, llm), st)
		verdict, lead, reason := st.state()
		if verdict != campaign.VerdictNoEvidence || lead != campaign.LeadNone || reason != noCalleeReason("zh") {
			t.Errorf("%s: verdict %s lead %s reason %q, want NO_EVIDENCE NONE", name, verdict, lead, reason)
		}
		if n := len(llm.requests()); n != 0 {
			t.Errorf("%s: %d requests, want none", name, n)
		}
	}
}

// A callee who made only pickup greetings and acknowledgement sounds is
// NO_EVIDENCE without asking the model, which read a bare sound after the
// bot's question as UNDECIDED; an answer word still goes to the model.
func TestOnlySoundsFromTheCalleeIsNoEvidenceWithoutARequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		lang  string
		lines []store.TranscriptLine
	}{
		{"zh", []store.TranscriptLine{bot("您好，我们这个月有半价流量套餐，您需要吗？"), callee("嗯。"), bot("那我帮您办理？"), callee("嗯")}},
		{"en", []store.TranscriptLine{callee("Hello?"), bot("We have a half-price data plan this month. Would you like it?"), callee("Mm-hmm.")}},
	} {
		st := newFakeStore(campaign.LeadHigh, tc.lang, 10*time.Second, tc.lines...)
		llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"UNDECIDED"}`)}}
		judgeOnce(t, newTestService(st, llm), st)
		verdict, lead, reason := st.state()
		if verdict != campaign.VerdictNoEvidence || lead != campaign.LeadNone || reason != onlySoundsReason(tc.lang) {
			t.Errorf("%s: verdict %s lead %s reason %q, want NO_EVIDENCE NONE", tc.lang, verdict, lead, reason)
		}
		if n := len(llm.requests()); n != 0 {
			t.Errorf("%s: %d requests, want none", tc.lang, n)
		}
	}

	st := newFakeStore(campaign.LeadHigh, "zh", 10*time.Second, bot("您好，我们这个月有半价流量套餐，您需要吗？"), callee("好"))
	llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"UNDECIDED","reason":"只说了好"}`)}}
	judgeOnce(t, newTestService(st, llm), st)
	if verdict, lead, _ := st.state(); verdict != campaign.VerdictUndecided || lead != campaign.LeadMedium {
		t.Errorf("an answer word: verdict %s lead %s, want UNDECIDED MEDIUM from the model", verdict, lead)
	}
	if n := len(llm.requests()); n != 1 {
		t.Errorf("an answer word: %d requests, want 1", n)
	}
}

// Tool calls and their results never reach the model, not their arguments,
// not their output, not even their name.
func TestToolRowsNeverReachThePrompt(t *testing.T) {
	t.Parallel()
	lines := []store.TranscriptLine{
		bot("Hello, would you like our half-price data plan?"),
		callee("Hmm, what is it?"),
		{Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolCall,
			Content: map[string]any{"name": "classify_answer", "args": `{"answer":"ARGUMENT_MARKER yes"}`}},
		{Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolResult,
			Content: map[string]any{"name": "classify_answer", "output": "RESULT_MARKER moved to close_high"}},
		bot("Great, I have noted that."),
	}
	st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, lines...)
	llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"UNDECIDED","reason":"Asked what it is."}`)}}
	judgeOnce(t, newTestService(st, llm), st)
	reqs := llm.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	for _, leaked := range []string{"ARGUMENT_MARKER", "RESULT_MARKER", "classify_answer", "close_high"} {
		if strings.Contains(reqs[0].User, leaked) || strings.Contains(reqs[0].System, leaked) {
			t.Errorf("the prompt carries %q:\n%s", leaked, reqs[0].User)
		}
	}
	for _, want := range []string{"[Callee] Hmm, what is it?", "[Bot] Great, I have noted that.", "- intent: HIGH", "Agreed to the offer"} {
		if !strings.Contains(reqs[0].User, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, reqs[0].User)
		}
	}
	if !reqs[0].JSON {
		t.Error("the request does not ask for a JSON object")
	}
}

// The one retry is made only while the deadline leaves time for it.
func TestOneRetryOnlyWithinTheDeadline(t *testing.T) {
	t.Parallel()
	t.Run("a failure then an answer is judged on the retry", func(t *testing.T) {
		t.Parallel()
		st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
		llm := &fakeLLM{replies: []func(context.Context) (string, error){
			fails(errors.New("textllm: status 429")), says(`{"verdict":"CONFIRMED","reason":"Said sign me up."}`)}}
		judgeOnce(t, newTestService(st, llm), st)
		if verdict, _, _ := st.state(); verdict != campaign.VerdictConfirmed || len(llm.requests()) != 2 {
			t.Errorf("verdict %s after %d requests, want CONFIRMED after 2", verdict, len(llm.requests()))
		}
	})
	t.Run("never more than one retry", func(t *testing.T) {
		t.Parallel()
		st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
		llm := &fakeLLM{replies: []func(context.Context) (string, error){fails(errors.New("down"))}}
		judgeOnce(t, newTestService(st, llm), st)
		if n := len(llm.requests()); n != 2 {
			t.Errorf("%d requests, want 2", n)
		}
	})
	t.Run("no retry once the deadline is too near", func(t *testing.T) {
		t.Parallel()
		// The first request fails with less than retryFloor left before the
		// job's own deadline.
		st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
		s := newTestService(st, nil)
		// Called on the job's goroutine, which is the one that reads the clock.
		nearly := func(context.Context) (string, error) {
			s.now = func() time.Time { return st.j.DeadlineAt.Add(-s.margin - retryFloor/2) }
			return "", errors.New("down")
		}
		llm := &fakeLLM{replies: []func(context.Context) (string, error){nearly}}
		s.llm = llm
		judgeOnce(t, s, st)
		if n := len(llm.requests()); n != 1 {
			t.Errorf("%d requests, want 1", n)
		}
		if verdict, lead, _ := st.state(); verdict != campaign.VerdictNotJudged || lead != campaign.LeadHigh {
			t.Errorf("verdict %s lead %s, want NOT_JUDGED HIGH", verdict, lead)
		}
	})
}

// The prompt is the call's language: zh calls get the Chinese prompt and are
// asked for a Chinese reason, every other language the English one.
func TestThePromptFollowsTheCallsLanguage(t *testing.T) {
	t.Parallel()
	zhLines := []store.TranscriptLine{bot("您好，这个月有半价流量套餐，您需要吗？"), callee("可以，那你帮我办一个吧。")}
	for _, c := range []struct {
		lang   string
		lines  []store.TranscriptLine
		system string
		user   []string
	}{
		{"zh", zhLines, systemPromptZH, []string{"[被叫] 可以，那你帮我办一个吧。", "reason 用简体中文写", "- 意向：HIGH"}},
		{"zh-CN", zhLines, systemPromptZH, []string{"[机器人] 您好"}},
		{"en", acceptedCall, systemPromptEN, []string{"Write the reason in English.", "[Callee] OK, sign me up for it."}},
		{"", zhLines, systemPromptZH, []string{"[被叫]"}},
		{"", acceptedCall, systemPromptEN, []string{"[Callee]"}},
	} {
		st := newFakeStore(campaign.LeadHigh, c.lang, 10*time.Second, c.lines...)
		llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"CONFIRMED","reason":"ok"}`)}}
		judgeOnce(t, newTestService(st, llm), st)
		reqs := llm.requests()
		if len(reqs) != 1 || reqs[0].System != c.system {
			t.Errorf("language %q: %d requests, wrong system prompt", c.lang, len(reqs))
			continue
		}
		for _, w := range c.user {
			if !strings.Contains(reqs[0].User, w) {
				t.Errorf("language %q: the prompt lacks %q:\n%s", c.lang, w, reqs[0].User)
			}
		}
	}
}

// A wake for an attempt that owes no judgement reads nothing; one for a
// judgement already settled asks nothing.
func TestOnlyAnOwedJudgementIsWorkedOn(t *testing.T) {
	t.Parallel()
	st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
	llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"CONFIRMED"}`)}}
	s := newTestService(st, llm)
	s.AttemptFinished(campaign.Finished{CallID: st.j.CallID})
	s.Wait()
	if st.reads != 0 {
		t.Errorf("an attempt that owes nothing was read %d times", st.reads)
	}
	st.j.Verdict = campaign.VerdictRefused
	judgeOnce(t, s, st)
	if n := len(llm.requests()); n != 0 || st.writes != 0 {
		t.Errorf("a settled judgement: %d requests, %d writes", n, st.writes)
	}
	var none *Service
	none.AttemptFinished(campaign.Finished{IsLeadJudgementOwed: true})
	none.Start()
	none.Stop()
}

// Start finds owed judgements nobody handed the service, and settles those
// past their deadline.
func TestStartPicksUpWhatIsOwedAndSettlesWhatIsLate(t *testing.T) {
	t.Parallel()
	owed := newFakeStore(campaign.LeadMedium, "en", 10*time.Second, acceptedCall...)
	llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"CONFIRMED","reason":"Said yes."}`)}}
	s := newTestService(owed, llm)
	s.Start()
	select {
	case v := <-owed.applied:
		if v != campaign.VerdictConfirmed {
			t.Errorf("verdict %s, want CONFIRMED", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owed judgement was not picked up")
	}
	s.Stop()

	late := newFakeStore(campaign.LeadHigh, "en", -time.Second, acceptedCall...)
	s = newTestService(late, nil) // no text model: deadlines only
	s.Start()
	select {
	case v := <-late.applied:
		if v != campaign.VerdictNotJudged {
			t.Errorf("verdict %s, want NOT_JUDGED", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late judgement was not settled")
	}
	s.Stop()
	if _, lead, _ := late.state(); lead != campaign.LeadHigh {
		t.Errorf("lead %s, want the declared HIGH", lead)
	}
}

// A restart onto a narrower AICC_LEAD_JUDGE_INTENTS (v0.9.2 judged HIGH and
// MEDIUM; unset, the list is now HIGH alone) finds judgements the run before
// left owed for a lead it no longer judges. Each is settled NOT_JUDGED at once,
// with no request and long before its deadline: the declared lead stands and
// nothing waits on it. A lead the list still names is judged as before.
func TestARestartThatNarrowsTheListSettlesWhatItNoLongerJudges(t *testing.T) {
	t.Parallel()
	high := campaign.LeadJudgeIntents{campaign.LeadHigh}
	for _, c := range []struct {
		name     string
		intents  campaign.LeadJudgeIntents
		declared campaign.LeadIntent
		verdict  campaign.LeadVerdict
		requests int
	}{
		{"a MEDIUM lead under the default HIGH", high, campaign.LeadMedium, campaign.VerdictNotJudged, 0},
		{"a HIGH lead under MEDIUM alone", campaign.LeadJudgeIntents{campaign.LeadMedium}, campaign.LeadHigh, campaign.VerdictNotJudged, 0},
		{"a HIGH lead under the default HIGH", high, campaign.LeadHigh, campaign.VerdictRefused, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// Owed by the run before, its whole deadline still ahead.
			st := newFakeStore(c.declared, "en", campaign.LeadJudgeDeadline, acceptedCall...)
			llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"REFUSED","reason":"Declined."}`)}}
			s := newTestService(st, llm)
			s.cfg.Intents = c.intents
			s.Start()
			select {
			case v := <-st.applied:
				if v != c.verdict {
					t.Errorf("verdict %s, want %s", v, c.verdict)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the owed judgement was not settled, so it waits for its deadline")
			}
			s.Stop()
			verdict, lead, reason := st.state()
			if want := campaign.LeadAfterVerdict(c.declared, c.verdict); verdict != c.verdict || lead != want {
				t.Errorf("verdict %s lead %s, want %s %s", verdict, lead, c.verdict, want)
			}
			if c.verdict == campaign.VerdictNotJudged && reason != "" {
				t.Errorf("reason %q, want none for a lead that was not judged", reason)
			}
			if n := len(llm.requests()); n != c.requests {
				t.Errorf("%d requests, want %d", n, c.requests)
			}
		})
	}
}

// The service tells the store which leads to owe a judgement for: the list it
// was given while it can judge, and none when it cannot.
func TestTheJudgedIntentsAreTheListOnlyWhileJudging(t *testing.T) {
	t.Parallel()
	llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"CONFIRMED"}`)}}
	high := campaign.LeadJudgeIntents{campaign.LeadHigh}
	for _, c := range []struct {
		name    string
		llm     textllm.Client
		intents campaign.LeadJudgeIntents
		want    campaign.LeadJudgeIntents
	}{
		{"a model and HIGH", llm, high, high},
		{"a model and both", llm, bothLeads, bothLeads},
		{"no model", nil, bothLeads, nil},
		{"no lead listed", llm, nil, nil},
	} {
		s := New(newFakeStore(campaign.LeadHigh, "en", time.Second), c.llm, Config{Intents: c.intents}, quiet())
		got := s.JudgedIntents()
		if !slices.Equal(got, c.want) || s.IsJudging() != (len(c.want) > 0) {
			t.Errorf("%s: judged %v judging %v, want %v", c.name, got, s.IsJudging(), c.want)
		}
	}
	var none *Service
	if none.IsJudging() || none.JudgedIntents() != nil {
		t.Error("a nil service judges")
	}
}

// The transcript is read Settle after the wake, not before.
func TestTheTranscriptIsReadAfterItSettles(t *testing.T) {
	t.Parallel()
	st := newFakeStore(campaign.LeadHigh, "en", 10*time.Second, acceptedCall...)
	var askedAt time.Time
	llm := &fakeLLM{replies: []func(context.Context) (string, error){func(context.Context) (string, error) {
		askedAt = time.Now()
		return `{"verdict":"CONFIRMED","reason":"ok"}`, nil
	}}}
	s := newTestService(st, llm)
	s.settle = 300 * time.Millisecond
	woke := time.Now()
	judgeOnce(t, s, st)
	if waited := askedAt.Sub(woke); waited < s.settle {
		t.Errorf("asked %s after the wake, want at least %s", waited, s.settle)
	}
}
