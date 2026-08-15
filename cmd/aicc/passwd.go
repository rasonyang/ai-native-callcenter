// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// runPasswd implements `aicc passwd`, the way an operator resets a forgotten
// password without deleting the account and everything that references it.
func runPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	username := fs.String("username", "", "login name (required)")
	password := fs.String("password", "", "new password, at least 8 characters (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || *password == "" {
		fs.Usage()
		return errors.New("username and password are required")
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

	user, err := st.Queries.GetUserByUsername(ctx, *username)
	if err != nil {
		return fmt.Errorf("no such user %q: %w", *username, err)
	}

	svc := auth.NewService(st.Queries, cfg.SessionTTL)
	if err := svc.SetPassword(ctx, user.ID, *password); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "password set for %s (%s)\n", user.Username, user.ID)
	return nil
}
