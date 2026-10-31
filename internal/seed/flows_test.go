// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/switchsyntax"
)

// Voice names belong to the engine that answers, and these flows ship to
// deployments that have not chosen one yet. A name here is therefore a name
// that is right for exactly one of them: `longanqian` was Qwen's, and on a
// deployment running the gateway every one of these flows fell silent — the
// synthesiser rejected the voice on its first frame, so the call connected,
// heard the caller, and never said a word.
//
// Empty is what a shipped flow says instead, and each profile supplies a voice
// its own engine offers (qwen's is now longanqian_v3.1).
// A deployment with a persona in mind still names one — it just knows which
// engine it runs.
func TestNoShippedFlowNamesAProviderSpecificVoice(t *testing.T) {
	t.Parallel()
	for file, spec := range shippedFlows(t) {
		if spec.Global.Voice != "" {
			t.Errorf("%s names the voice %q; a shipped flow cannot know which "+
				"engine answers, and a name the deployment's engine does not "+
				"offer is a call that connects and stays silent",
				file, spec.Global.Voice)
		}
	}
}

// A flow in the directory that no entry of demoFlows names ships in the binary
// and never answers: it is embedded, it is not published, and no number points
// at it. The reverse — a table entry naming a file that is not there — is a
// seed that fails at boot. Both are invisible to a reading of either side, so
// the directory and the table are compared here.
func TestEveryShippedFlowIsSeeded(t *testing.T) {
	t.Parallel()
	entries, err := flowFiles.ReadDir("flows")
	if err != nil {
		t.Fatalf("read the embedded flows: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no flows are embedded; this test would pass on an empty set")
	}

	seeded := map[string]bool{}
	for _, f := range demoFlows {
		seeded[f.file] = true
	}
	for _, entry := range entries {
		file := "flows/" + entry.Name()
		if !seeded[file] {
			t.Errorf("%s is embedded but no demoFlows entry publishes it, so it "+
				"ships in the binary and never answers a call", file)
		}
	}

	// The other direction, and the two identities a demo flow must not share
	// with another: its numbers and its slug.
	numbers := map[string]string{}
	slugs := map[string]string{}
	for _, f := range demoFlows {
		spec, err := flowFiles.ReadFile(f.file)
		if err != nil {
			t.Errorf("demoFlows names %s, which is not embedded: %v", f.file, err)
			continue
		}
		slug := specID(spec)
		if other, dup := slugs[slug]; dup {
			t.Errorf("%s and %s both carry the slug %q; the second would find the "+
				"first's flow and publish nothing", other, f.file, slug)
		}
		slugs[slug] = f.file
		for _, n := range f.numbers() {
			if other, dup := numbers[n.number]; dup {
				t.Errorf("%s and %s both claim %s; only the first to be seeded "+
					"gets it (ON CONFLICT DO NOTHING)", other, f.file, n.number)
			}
			numbers[n.number] = f.file
		}
	}
	if len(numbers) != len(demoNumbers()) {
		t.Errorf("%d distinct numbers for %d flows, want %d",
			len(numbers), len(demoFlows), len(demoNumbers()))
	}
}

// The toll-free numbers are a compliance boundary and a single main line, so
// the table is checked before any of it reaches a database: US demo numbers
// stay inside the fictional 800-555-01XX block (owner directive 2026-09-12),
// and exactly one number carries the outbound flags — the database allows one
// default outbound row, and a second entry setting isMainLine would make the
// seed unapplicable rather than merely wrong.
func TestOneTollFreeNumberIsTheMainLine(t *testing.T) {
	t.Parallel()
	var mainLines, defaults []string
	for _, f := range demoFlows {
		if f.isOutboundOnly() {
			continue
		}
		if len(f.toll) != 10 || f.toll[:7] != "8005550" || f.toll[7] != '1' {
			t.Errorf("%s answers toll-free on %s, which is outside the "+
				"fictional 800-555-01XX block", f.file, f.toll)
		}
		if f.isMainLine {
			mainLines = append(mainLines, f.toll)
		}
		for _, n := range f.numbers() {
			if n.isDefaultOutbound {
				defaults = append(defaults, n.number)
				if !n.allowOutbound {
					t.Errorf("%s is the default outbound number and is not allowed "+
						"to dial out (dids_default_outbound_dials_out)", n.number)
				}
			}
		}
	}
	if len(mainLines) != 1 || mainLines[0] != "8005550199" {
		t.Errorf("main lines = %v, want exactly [8005550199]", mainLines)
	}
	if len(defaults) != 1 || defaults[0] != "8005550199" {
		t.Errorf("default outbound numbers = %v, want exactly [8005550199]", defaults)
	}
}

// The seed writes its numbers and queues straight into the tables, past the
// API's checks, so the values it writes are held to the same rules here: a
// language is a lowercase BCP 47 tag, and a queue's name and fallback are
// nothing the switch would read as syntax.
func TestTheSeedWritesOnlyWhatTheAPIWouldAccept(t *testing.T) {
	t.Parallel()
	for _, f := range demoFlows {
		for _, n := range f.numbers() {
			if !catalog.IsLanguageTag(n.language) {
				t.Errorf("%s is seeded with language %q, which the API refuses", n.number, n.language)
			}
		}
	}
	for _, d := range demoDIDs {
		if !catalog.IsLanguageTag(d.language) {
			t.Errorf("history number %s has language %q, which the API refuses", d.number, d.language)
		}
	}
	for _, q := range demoQueues {
		if bad := switchsyntax.Unsafe(q.name); bad != "" || strings.Contains(q.name, "@") {
			t.Errorf("queue %q is a name the API refuses", q.name)
		}
		if !catalog.IsLanguageTag(q.language) {
			t.Errorf("queue %q serves language %q, which the API refuses", q.name, q.language)
		}
	}
}

// A shipped flow has to be publishable on a deployment whose speech engine
// only says what it is given, which means two things: the phase a call starts
// in carries the greeting, and every phase a call does not leave carries its
// closing line. Both languages, because the same flow answers an English
// number and a Chinese one.
//
// The demo is where this bites first. It publishes six flows at boot, and a
// seed that will not apply is an installation that comes up empty — with no
// accounts, no numbers and no history either, because the flows are seeded
// before any of it.
func TestEveryShippedFlowSpeaksItsOpeningAndItsEndings(t *testing.T) {
	t.Parallel()
	for file, spec := range shippedFlows(t) {
		entry, ok := spec.Node(spec.InitialNode)
		if !ok {
			t.Errorf("%s: initialNode %q is not a phase", file, spec.InitialNode)
			continue
		}
		hasBothLanguages(t, file, spec.InitialNode, entry.Announce)

		if err := flow.RequireTerminalAnnounce(spec); err != nil {
			t.Errorf("%s: %v", file, err)
		}
		for id, node := range spec.Nodes {
			if node.IsTerminal {
				hasBothLanguages(t, file, id, node.Announce)
			}
		}
	}
}

// Every shipped flow names a closing target, so the NO_INPUT default can end a
// call nobody is on any more (issue #9); Load already checks that the target
// is a terminal phase. The turns-without-a-tool wall is set only where a
// closing phase ends in an "anything else?" loop the model can keep running
// without a tool, at the value 02-ai-voice §6 justifies — a wall anywhere else
// could only cut a legitimate call short. early_collections is there too since
// #147: a model that closes the conversation in words and never hangs up keeps
// answering the caller without a tool, and the wall is what ends that call.
func TestEveryShippedFlowCanCloseACallOnItsOwn(t *testing.T) {
	t.Parallel()
	const wall = 10
	hasAnythingElseLoop := map[string]bool{
		"mobile_support.json":            true,
		"field_service_appointment.json": true,
		"plan_change.json":               true,
		"early_collections.json":         true,
	}
	flows := shippedFlows(t)
	for file := range hasAnythingElseLoop {
		if _, ok := flows[file]; !ok {
			t.Errorf("%s is no longer shipped; this test names it", file)
		}
	}
	for file, spec := range flows {
		if spec.Global.ClosingTarget == "" {
			t.Errorf("%s: global.closingTarget is empty", file)
		}
		want := 0
		if hasAnythingElseLoop[file] {
			want = wall
		}
		if got := spec.Global.MaxTurnsWithoutTool; got != want {
			t.Errorf("%s: maxTurnsWithoutTool = %d, want %d", file, got, want)
		}
	}
}

// The greeting used to be prose inside the entry instruction — "Open with:
// ..." — and the model obliged by saying it. Now that the phase carries the
// line, an instruction still asking for it would have the caller greeted
// twice: once as written, once again in the model's own words.
func TestNoEntryInstructionStillAsksForTheGreeting(t *testing.T) {
	t.Parallel()
	for file, spec := range shippedFlows(t) {
		entry, ok := spec.Node(spec.InitialNode)
		if !ok {
			continue
		}
		for _, lang := range []string{flow.LangEN, flow.LangZH} {
			instruction := entry.Instruction.For(lang)
			for _, leftover := range []string{"Open with", "开场说"} {
				if strings.Contains(instruction, leftover) {
					t.Errorf("%s: the %s instruction of %q still says %q, so the caller "+
						"is greeted twice", file, lang, spec.InitialNode, leftover)
				}
			}
		}
	}
}

// Every shipped flow ends a call in a terminal phase whose announce is the
// goodbye, so a rule or instruction that still tells the model to say goodbye
// after hangup has the caller hear two closing lines: the model's own, often a
// thank-you in the response that calls the tool, and then the line as written
// (issue #350).
func TestNoInstructionStillAsksForAGoodbyeAfterHangup(t *testing.T) {
	t.Parallel()
	leftovers := map[string][]string{
		flow.LangEN: {"then say goodbye", "say goodbye after"},
		flow.LangZH: {"再道别"},
	}
	for file, spec := range shippedFlows(t) {
		for _, lang := range []string{flow.LangEN, flow.LangZH} {
			check := func(where, text string) {
				for _, leftover := range leftovers[lang] {
					if strings.Contains(text, leftover) {
						t.Errorf("%s: the %s %s still says %q, so the caller hears "+
							"two closing lines: the model's own, then the terminal "+
							"phase's announce", file, lang, where, leftover)
					}
				}
			}
			for _, rule := range spec.Global.Rules.For(lang) {
				check("global rules", rule)
			}
			for id, node := range spec.Nodes {
				check("instruction of "+strconv.Quote(id), node.Instruction.For(lang))
			}
		}
	}
}

// shippedFlows loads every embedded flow, keyed by file name.
func shippedFlows(t *testing.T) map[string]*flow.Spec {
	t.Helper()
	entries, err := flowFiles.ReadDir("flows")
	if err != nil {
		t.Fatalf("read the embedded flows: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no flows are embedded; this test would pass on an empty set")
	}

	specs := make(map[string]*flow.Spec, len(entries))
	for _, entry := range entries {
		data, err := flowFiles.ReadFile("flows/" + entry.Name())
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		spec, err := flow.Load(data)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		specs[entry.Name()] = spec
	}
	return specs
}

// hasBothLanguages fails when a line exists in one language only. The flow
// falls back to the other rather than to silence, so the caller would be
// greeted in a language they did not dial.
func hasBothLanguages(t *testing.T, file, node string, line flow.Text) {
	t.Helper()
	if line.EN == "" || line.ZH == "" {
		t.Errorf("%s: phase %q has no line to speak in %s", file, node,
			map[bool]string{true: "English", false: "Chinese"}[line.EN == ""])
	}
}

// The campaign flow's global rules let a customer change their mind ("call it
// again when their stance clearly changes; the last call counts"). A `talk`
// instruction that orders hangup right after recording an ordinary stance
// makes that rule unreachable: the line is gone before the customer can speak
// (issue #206). Only the sentence that names DO_NOT_CALL may order an
// immediate hangup; it also covers a clear no (NONE), which has nothing left to
// confirm and ends at once (owner decision after the r3 live round).
func TestCampaignTalkDoesNotHangUpRightAfterRecordingAStance(t *testing.T) {
	t.Parallel()
	spec := shippedFlows(t)["campaign_lead_followup.json"]
	if spec == nil {
		t.Fatal("campaign_lead_followup.json is not embedded")
	}
	talk, ok := spec.Nodes["talk"]
	if !ok {
		t.Fatal("campaign flow has no talk phase")
	}

	type lang struct {
		name        string
		instruction string
		rules       []string
		hurried     *regexp.Regexp // hangup ordered with no room for a reply
		sentences   *regexp.Regexp
		changeRule  string
		confirmAsk  string
	}
	langs := []lang{
		{"en", talk.Instruction.EN, spec.Global.Rules.EN,
			regexp.MustCompile(`(?i)immediately|right after|straight away|at once`),
			regexp.MustCompile(`[.;:]\s+`), "again", "anything else"},
		{"zh", talk.Instruction.ZH, spec.Global.Rules.ZH,
			regexp.MustCompile(`立即|马上|立刻|随即`),
			regexp.MustCompile(`[。;;]`), "再调用一次", "还有"},
	}
	for _, l := range langs {
		if l.instruction == "" {
			t.Fatalf("%s: talk has no instruction", l.name)
		}
		allowsChange := false
		for _, r := range l.rules {
			if strings.Contains(r, "record_lead") && strings.Contains(r, l.changeRule) {
				allowsChange = true
			}
		}
		if !allowsChange {
			t.Fatalf("%s: the global rules no longer let a stance be recorded again; "+
				"update this test with the rule", l.name)
		}
		for _, s := range l.sentences.Split(l.instruction, -1) {
			if !strings.Contains(s, "hangup") || strings.Contains(s, "DO_NOT_CALL") {
				continue
			}
			if l.hurried.MatchString(s) {
				t.Errorf("%s: talk orders an immediate hangup outside DO_NOT_CALL, "+
					"so a changed stance can never be recorded: %q", l.name, s)
			}
		}
		if !strings.Contains(l.instruction, l.confirmAsk) {
			t.Errorf("%s: talk no longer asks the customer once whether there is "+
				"anything else before it hangs up", l.name)
		}
	}
}
