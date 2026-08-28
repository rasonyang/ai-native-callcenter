// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

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
	didNumber := fs.String("did", "", "point this number at the flow")
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
		did, err := st.Queries.GetDIDByNumber(ctx, *didNumber)
		if err != nil {
			return fmt.Errorf("number %s: %w", *didNumber, err)
		}
		// Every column the update writes, carried over from the row itself.
		// UpdateDID rewrites the whole row, so a field left out is not left
		// alone — it is written as Go's zero value. Omitting the direction
		// flags set both to false, which the dids_go_somewhere CHECK refuses,
		// and this command could not point any number at a flow at all.
		if _, err := st.Queries.UpdateDID(ctx, queries.UpdateDIDParams{
			ID: did.ID, Language: did.Language, FlowID: &id,
			FallbackQueueID:    did.FallbackQueueID,
			IsRecordingEnabled: did.IsRecordingEnabled,
			Description:        did.Description,
			IsEnabled:          did.IsEnabled,
			AllowInbound:       did.AllowInbound,
			AllowOutbound:      did.AllowOutbound,
			IsDefaultOutbound:  did.IsDefaultOutbound,
		}); err != nil {
			return fmt.Errorf("assign flow to %s: %w", *didNumber, err)
		}
		fmt.Printf("number %s now answers with %s\n", *didNumber, specID(data))
	}
	return nil
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
