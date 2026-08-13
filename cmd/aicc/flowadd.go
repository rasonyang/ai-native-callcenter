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
	// Create validates, so a broken file fails here with the loader's report.
	id, err := flows.Create(ctx, orFirst(*slug, specID(data)), orFirst(*name, *slug, specID(data)), data)
	if err != nil {
		return err
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
		if _, err := st.Queries.UpdateDID(ctx, queries.UpdateDIDParams{
			ID: did.ID, Language: did.Language, FlowID: &id,
			FallbackQueueID:    did.FallbackQueueID,
			IsRecordingEnabled: did.IsRecordingEnabled,
			Description:        did.Description,
			IsEnabled:          did.IsEnabled,
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
