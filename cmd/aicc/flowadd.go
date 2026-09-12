// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// runFlowAdd implements `aicc flowadd`: load a flow file, publish it, and
// optionally point a number at it. It exists because a flow is useless until a
// number runs it, and the designer UI arrives after the voice leg does.
func runFlowAdd(args []string) error {
	fs := flag.NewFlagSet("flowadd", flag.ExitOnError)
	file := fs.String("file", "", "path to a flow spec (required)")
	slug := fs.String("slug", "", "stable identifier (defaults to the spec's id)")
	name := fs.String("name", "", "display name (defaults to the slug)")
	didNumber := fs.String("did", "", "point this number at the flow; the number is created if it does not exist")
	language := fs.String("language", "en", "greeting language for a number this command creates (a short subtag such as en or zh); an existing number keeps its own")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		fs.Usage()
		return errors.New("a flow file is required")
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	flows := st.Flows()
	resolvedSlug := orFirst(*slug, specID(data))

	// Create validates, so a broken file fails here with the loader's report.
	// A slug that already exists means this is an update: replace the draft
	// and publish a new revision, keeping the flow's identity — and every DID
	// pointing at it — intact.
	id, err := flows.Create(ctx, resolvedSlug, orFirst(*name, *slug, specID(data)), data)
	if err != nil {
		existing, lookupErr := st.Queries.GetFlowBySlug(ctx, resolvedSlug)
		if lookupErr != nil {
			return err
		}
		if err := flows.UpdateDraft(ctx, existing.ID, existing.Name, data); err != nil {
			return err
		}
		id = existing.ID
	}
	if err := flows.Publish(ctx, id, "flowadd"); err != nil {
		return err
	}
	fmt.Printf("flow %s published (%s)\n", specID(data), id)

	if *didNumber != "" {
		var existing *queries.Did
		did, err := st.Queries.GetDIDByNumber(ctx, *didNumber)
		switch {
		case err == nil:
			existing = &did
		case errors.Is(err, pgx.ErrNoRows):
			// A fresh deployment has no numbers at all. Creating one here is
			// what lets an operator make the product answer a call with
			// useradd and flowadd, before any API call.
		default:
			return fmt.Errorf("number %s: %w", *didNumber, err)
		}

		plan, err := planDIDBinding(*didNumber, *language, id, existing)
		if err != nil {
			return err
		}
		switch {
		case plan.Create != nil:
			if _, err := st.Queries.CreateDID(ctx, *plan.Create); err != nil {
				return fmt.Errorf("create number %s: %w", *didNumber, err)
			}
			fmt.Printf("number %s created, answers with %s\n", *didNumber, specID(data))
		default:
			if _, err := st.Queries.UpdateDID(ctx, *plan.Update); err != nil {
				return fmt.Errorf("assign flow to %s: %w", *didNumber, err)
			}
			fmt.Printf("number %s now answers with %s\n", *didNumber, specID(data))
		}
	}
	return nil
}

// didBinding is what flowadd does about -did: exactly one of Create and Update
// is set.
type didBinding struct {
	Create *queries.CreateDIDParams
	Update *queries.UpdateDIDParams
}

// planDIDBinding decides between creating the number and repointing the one
// that is already there, and builds the parameters either way. It is separate
// from the database so the decision and the columns can be tested without one.
//
// existing is nil when GetDIDByNumber found no row.
//
// The checks mirror catalog.DID.validate (internal/catalog/types.go), which is
// what the HTTP CreateDID operation applies: a number is digits only, and a
// language is a short subtag, defaulting to en. They are repeated rather than
// called because validate is unexported and the catalog service needs a switch
// connection this command does not have.
func planDIDBinding(number, language string, flowID uuid.UUID, existing *queries.Did) (didBinding, error) {
	number = strings.TrimSpace(number)
	language = strings.TrimSpace(language)
	if !digitsOnly(number) {
		return didBinding{}, fmt.Errorf("number %q: a number must be digits", number)
	}
	if language == "" {
		language = "en"
	}
	if len(language) > 8 {
		return didBinding{}, fmt.Errorf("language %q: must be a short subtag such as en or zh", language)
	}

	if existing == nil {
		// The same row the API's create would write: inbound, enabled, and
		// recording on, which is what catalog.NewDID seeds for a body that
		// says nothing.
		return didBinding{Create: &queries.CreateDIDParams{
			ID:                 uuid.Must(uuid.NewV7()),
			Number:             number,
			Language:           language,
			FlowID:             &flowID,
			FallbackQueueID:    nil,
			IsRecordingEnabled: true,
			Description:        "",
			IsEnabled:          true,
			AllowInbound:       true,
			AllowOutbound:      false,
			IsDefaultOutbound:  false,
		}}, nil
	}

	// Every column the update writes, carried over from the row itself.
	// UpdateDID rewrites the whole row, so a field left out is not left
	// alone — it is written as Go's zero value. Omitting the direction
	// flags set both to false, which the dids_go_somewhere CHECK refuses,
	// and this command could not point any number at a flow at all.
	return didBinding{Update: &queries.UpdateDIDParams{
		ID: existing.ID, Language: existing.Language, FlowID: &flowID,
		FallbackQueueID:    existing.FallbackQueueID,
		IsRecordingEnabled: existing.IsRecordingEnabled,
		Description:        existing.Description,
		IsEnabled:          existing.IsEnabled,
		AllowInbound:       existing.AllowInbound,
		AllowOutbound:      existing.AllowOutbound,
		IsDefaultOutbound:  existing.IsDefaultOutbound,
	}}, nil
}

// digitsOnly is catalog's rule for a dialable number: at least one digit, and
// nothing else.
func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// specID pulls the id out of a flow file.
func specID(data []byte) string {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &head); err != nil || head.ID == "" {
		return "flow"
	}
	return head.ID
}

func orFirst(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
