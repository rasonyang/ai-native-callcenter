// SPDX-License-Identifier: Apache-2.0

// Package seed fills an empty installation with a deterministic demo: a small
// team, two queues, six published bilingual flows each behind an English and a
// Chinese number, and seven days of synthetic history so the wallboard, the
// CDR explorer and the reports render alive on first sight.
package seed

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// flowFiles carries the demo flows into the binary, so a container with
// nothing mounted still answers a call. The same files are the ones an
// operator edits and reloads with `aicc flowadd -file …`.
//
//go:embed flows/*.json
var flowFiles embed.FS

// prngSeed pins the whole history: same seed, same rows, every install.
const prngSeed = 20260814

// demoPassword is the documented password of every seeded account and of the
// SIP extensions behind them — a softphone that cannot register is not a demo,
// and neither is an account nobody can sign in to. One password for both, so
// there is one thing to remember and one thing to change.
//
// The dataset only exists where AICC_SEED=demo was set deliberately, and the
// deployment doc says in as many words that it must not be a public host.
const demoPassword = "aicc@12345"

// demoFlows are the bundled flows the demo publishes, each behind one number
// per language. Every bundled flow carries English and Chinese personas, so one
// flow serves both numbers — the number's language picks the strings
// (phase1-decisions A1: language never selects a provider).
//
// The numbers follow a plan rather than a list: the Nth flow answers on 950N1
// in English and on 950N2 in Chinese. A number therefore says which flow picks
// up and in which language without looking anything up, and a seventh flow
// knows its own pair before anybody assigns one. 95001 / 95002 keep the meaning
// they have had since the first demo.
var demoFlows = []demoFlow{
	{"flows/novanet_support.json", "NovaNet support", "95001", "95002"},
	{"flows/mobile_support.json", "StarCom mobile after-sales", "95011", "95012"},
	{"flows/plan_change.json", "NovaNet broadband plan change", "95021", "95022"},
	{"flows/early_collections.json", "NovaNet early collections", "95031", "95032"},
	{"flows/field_service_appointment.json", "StarCom field-service appointment", "95041", "95042"},
	{"flows/lead_qualification.json", "StarCom lead qualification", "95051", "95052"},
}

// demoFlow is one bundled flow and the two numbers it answers on.
type demoFlow struct {
	file, name string // embedded spec, display name
	en, zh     string // the English and the Chinese number
}

// demoNumber is one DID the demo owns.
type demoNumber struct {
	number, language, queue, description string
}

// numbers are the two DIDs of a demo flow. The language decides the fallback
// queue — the demo has exactly one queue per language — and the description
// names the flow, because that is what an operator reads in the DID list.
func (f demoFlow) numbers() []demoNumber {
	return []demoNumber{
		{f.en, "en", queueForLanguage("en"), f.name + " (English)"},
		{f.zh, "zh", queueForLanguage("zh"), f.name + " (Chinese)"},
	}
}

// demoNumbers is every DID the demo owns, in flow order.
func demoNumbers() []demoNumber {
	out := make([]demoNumber, 0, 2*len(demoFlows))
	for _, f := range demoFlows {
		out = append(out, f.numbers()...)
	}
	return out
}

// demoPeople is the cast of the demo: one account per role that a visitor
// needs, plus the agents who make the wallboard worth looking at. An account
// with no extension is not an agent and gets no presence — the administrator
// and the supervisor watch.
//
// The usernames are the roles, because the first thing anybody does with this
// dataset is sign in as each of them in turn (owner directive 2026-08-19).
var demoPeople = []struct {
	username, display, role, ext, queue string
}{
	{"admin", "Ada Ops", "ADMIN", "", ""},
	{"supervisor", "Sam Reyes", "SUPERVISOR", "", ""},
	{"wei", "Wei Chen", "AGENT", "1001", "support-en"},
	{"amy", "Amy Zhang", "AGENT", "1000", "support-en"},
	{"ben", "Ben Liu", "AGENT", "1002", "support-zh"},
}

// demoQueues are the two queues the demo staffs, one per language: a bot that
// hands over, or cannot run at all, sends the caller to the queue that speaks
// the number's language.
var demoQueues = []struct{ language, name, ext, display string }{
	{"en", "support-en", "7001", "Support (EN)"},
	{"zh", "support-zh", "7002", "Support (ZH)"},
}

// queueForLanguage names the queue a number of that language falls back to.
func queueForLanguage(language string) string {
	for _, q := range demoQueues {
		if q.language == language {
			return q.name
		}
	}
	return ""
}

// Demo seeds the demo dataset. Existing data always wins: entities are
// inserted with on-conflict-do-nothing on their natural keys, and the
// history is generated only into an empty ledger.
func Demo(ctx context.Context, st *store.Store, log *slog.Logger) error {
	agents, queues, err := ensureEntities(ctx, st, log)
	if err != nil {
		return fmt.Errorf("seed entities: %w", err)
	}
	if err := ensureFlowAndNumbers(ctx, st, log); err != nil {
		return fmt.Errorf("seed flow: %w", err)
	}

	var cdrCount int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM cdrs`).Scan(&cdrCount); err != nil {
		return err
	}
	if cdrCount > 0 {
		log.Info("seed: the ledger already has calls; history untouched", "cdrs", cdrCount)
		return nil
	}

	ledger := st.Ledger()
	// The wrap-ups are filed under whatever vocabulary the installation has,
	// read rather than assumed: a demo that files a code the deployment does
	// not offer would show dispositions nobody can choose again.
	plan := planHistory(time.Now(), agents, queues,
		dispositionCodes(ctx, ledger, log), rand.New(rand.NewSource(prngSeed)))
	for _, cdr := range plan.CDRs {
		if err := ledger.InsertCDR(ctx, cdr); err != nil {
			return fmt.Errorf("seed cdr: %w", err)
		}
	}
	for _, ev := range plan.QueueEvents {
		if err := ledger.InsertQueueEvent(ctx, ev.OccurredAt, &ev.CallID, ev.QueueID,
			ev.Event, ev.AgentID, ev.WaitMs); err != nil {
			return fmt.Errorf("seed queue event: %w", err)
		}
	}
	for _, entry := range plan.StateLogs {
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO agent_state_logs (agent_id, state, reason, entered_at, exited_at)
			VALUES ($1, $2, $3, $4, $5)`,
			entry.AgentID, entry.State, entry.Reason, entry.EnteredAt, entry.ExitedAt); err != nil {
			return fmt.Errorf("seed state log: %w", err)
		}
	}

	// After the calls: a wrap-up is filed against a call, and a contact is
	// worth having because a call from that number exists.
	for _, row := range plan.WrapUps {
		if err := ledger.OpenWrapUp(ctx, row.CallID, row.AgentID); err != nil {
			return fmt.Errorf("seed wrap-up: %w", err)
		}
		// Not every call gets confirmed on a real day, and a demo where the
		// completion rate reads 100% teaches nobody what the number is for.
		if !row.IsConfirmed {
			continue
		}
		code, note := row.DispositionCode, row.Note
		if _, err := ledger.ConfirmWrapUp(ctx, row.CallID, row.AgentID, &code, &note); err != nil {
			return fmt.Errorf("seed wrap-up: %w", err)
		}
	}
	contacts := st.Contacts()
	for _, row := range plan.Contacts {
		name, company, notes := row.Name, row.Company, row.Notes
		tags := row.Tags
		if tags == nil {
			tags = []string{}
		}
		if _, err := contacts.Create(ctx, store.ContactWrite{
			PhoneNumber: row.PhoneNumber, Name: &name, Company: &company,
			Tags: &tags, Notes: &notes,
		}, nil); err != nil && !errors.Is(err, store.ErrContactExists) {
			return fmt.Errorf("seed contact: %w", err)
		}
	}

	log.Info("seed: demo history written",
		"cdrs", len(plan.CDRs), "queueEvents", len(plan.QueueEvents),
		"stateLogs", len(plan.StateLogs), "wrapUps", len(plan.WrapUps),
		"contacts", len(plan.Contacts))
	return nil
}

// dispositionCodes reads the installation's wrap-up vocabulary. An empty list
// is not a failure — the demo simply files no dispositions.
func dispositionCodes(ctx context.Context, ledger *store.LedgerStore, log *slog.Logger) []string {
	dispositions, err := ledger.ListDispositions(ctx)
	if err != nil {
		log.Warn("seed: no disposition vocabulary; history will carry no wrap-ups", "error", err)
		return nil
	}
	codes := make([]string, 0, len(dispositions))
	for _, d := range dispositions {
		codes = append(codes, d.Code)
	}
	return codes
}

// QueueRef is a queue the history can reference.
type QueueRef struct {
	ID   uuid.UUID
	Name string
}

// ensureEntities creates the demo team and queues where they do not already
// exist, and returns whatever agents and queues the database ends up with.
func ensureEntities(ctx context.Context, st *store.Store, log *slog.Logger) ([]uuid.UUID, []QueueRef, error) {
	demoHash, err := auth.HashPassword(demoPassword)
	if err != nil {
		return nil, nil, err
	}

	// Three agents to fill the wallboard, plus the two accounts a visitor
	// needs to see the whole product: administration and supervision are
	// role-gated, so a demo with agents only hides most of the screens.
	for _, p := range demoPeople {
		userID := uuid.New()
		// The password and the role are *reset* on every seed, which is the
		// one place this seeder overrules existing data (owner directive
		// 2026-08-19). A demo account whose password drifted is a demo
		// nobody can open, and "run the seed" is the answer an operator
		// should get. The display name is left alone: it is theirs.
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO users (id, username, password_hash, display_name, role)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (username) DO UPDATE
			SET password_hash = excluded.password_hash,
			    role          = excluded.role,
			    updated_at    = now()`,
			userID, p.username, demoHash, p.display, p.role); err != nil {
			return nil, nil, err
		}
		if p.ext == "" {
			continue
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO extensions (id, number, password, display_name)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (number) DO NOTHING`,
			uuid.New(), p.ext, demoPassword, p.display); err != nil {
			return nil, nil, err
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO agents (id, user_id, callcenter_name)
			SELECT $1, u.id, 'agent-' || $2::text FROM users u
			WHERE u.username = $2 AND NOT EXISTS (
				SELECT 1 FROM agents a WHERE a.user_id = u.id)`,
			uuid.New(), p.username); err != nil {
			return nil, nil, err
		}
		// Bind the agent to their phone. Without this the demo creates
		// agents and extensions that have never heard of each other, and
		// every seeded agent is refused at sign-in ("no extension is bound
		// to this agent") — a demo nobody can answer a call on.
		//
		// Only a binding that is missing is filled in, and only with a phone
		// nobody else holds: existing data wins here as everywhere in the
		// seeder, and the database refuses two agents on one extension.
		if _, err := st.Pool.Exec(ctx, `
			UPDATE agents a
			SET default_extension_id = e.id
			FROM users u, extensions e
			WHERE a.user_id = u.id AND u.username = $1 AND e.number = $2
			  AND a.default_extension_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM agents b WHERE b.default_extension_id = e.id)`,
			p.username, p.ext); err != nil {
			return nil, nil, err
		}
	}

	for _, q := range demoQueues {
		// Every column the demo depends on, named rather than left to the
		// table's defaults: a queue created through the API without them comes
		// out with zeros (C33), and one seeded queue already differed from the
		// other for exactly that reason. Two queues that are meant to be alike
		// should not be able to drift apart depending on which door they came
		// through.
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO queues (id, name, ext_number, display_name,
			                    sla_threshold_sec, discard_abandoned_after_sec)
			VALUES ($1, $2, $3, $4, 20, 60)
			ON CONFLICT (name) DO NOTHING`,
			uuid.New(), q.name, q.ext, q.display); err != nil {
			return nil, nil, err
		}
	}

	// Staff the queues. An agent with a phone and no tier is Available, in no
	// queue, and offered nothing: the caller waits out the timeout and
	// abandons while somebody sits ready. Nothing in the demo said which
	// queue anybody worked, so nothing routed — the other half of "the seed
	// leaves an agent who cannot take a call".
	//
	// The switch learns of it when the agent signs in (presence reconciles
	// tiers), so this is a database fact only.
	for i, p := range demoPeople {
		if p.queue == "" {
			continue
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO queue_agents (queue_id, agent_id, level, position)
			SELECT q.id, a.id, 1, $3 FROM queues q, agents a
			JOIN users u ON u.id = a.user_id
			WHERE q.name = $1 AND u.username = $2
			ON CONFLICT (queue_id, agent_id) DO NOTHING`,
			p.queue, p.username, i+1); err != nil {
			return nil, nil, err
		}
	}

	// The history references whatever actually exists, seeded or prior.
	var agents []uuid.UUID
	rows, err := st.Pool.Query(ctx, `SELECT id FROM agents ORDER BY created_at LIMIT 8`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		agents = append(agents, id)
	}

	var queues []QueueRef
	qrows, err := st.Pool.Query(ctx, `SELECT id, name FROM queues ORDER BY name LIMIT 8`)
	if err != nil {
		return nil, nil, err
	}
	defer qrows.Close()
	for qrows.Next() {
		var q QueueRef
		if err := qrows.Scan(&q.ID, &q.Name); err != nil {
			return nil, nil, err
		}
		queues = append(queues, q)
	}

	log.Info("seed: entities ensured", "agents", len(agents), "queues", len(queues))
	return agents, queues, nil
}

// ensureFlowAndNumbers publishes every bundled demo flow and points its two
// numbers at it. Without this a seeded install looks complete and still cannot
// take a call: dids.flow_id is NOT NULL, so a number exists only once a flow
// does.
//
// Existing data wins here too — an operator who has already published a flow
// under one of these slugs, or who owns one of these numbers, keeps what they
// have.
func ensureFlowAndNumbers(ctx context.Context, st *store.Store, log *slog.Logger) error {
	flows := st.Flows()
	var numbers int
	for _, f := range demoFlows {
		spec, err := flowFiles.ReadFile(f.file)
		if err != nil {
			return err
		}
		slug := specID(spec)

		flowID, err := flows.Create(ctx, slug, f.name, spec)
		if err != nil {
			existing, lookupErr := st.Queries.GetFlowBySlug(ctx, slug)
			if lookupErr != nil {
				return fmt.Errorf("create flow %s: %w", slug, err)
			}
			log.Info("seed: demo flow already present", "slug", slug)
			flowID = existing.ID
		} else if err := flows.Publish(ctx, flowID, "seed"); err != nil {
			return fmt.Errorf("publish flow %s: %w", slug, err)
		}

		for _, n := range f.numbers() {
			if _, err := st.Pool.Exec(ctx, `
				INSERT INTO dids (id, number, language, flow_id, fallback_queue_id, description)
				SELECT $1, $2, $3, $4, q.id, $6 FROM queues q WHERE q.name = $5
				ON CONFLICT (number) DO NOTHING`,
				uuid.New(), n.number, n.language, flowID, n.queue, n.description); err != nil {
				return fmt.Errorf("seed number %s: %w", n.number, err)
			}
			numbers++
		}
	}
	log.Info("seed: demo flows published", "flows", len(demoFlows), "numbers", numbers)
	return nil
}

// specID reads a flow spec's own identifier, which is also its slug.
func specID(spec []byte) string {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(spec, &head); err != nil || head.ID == "" {
		return "demo"
	}
	return head.ID
}
