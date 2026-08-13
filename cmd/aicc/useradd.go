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

// runUserAdd implements `aicc useradd`, the bootstrap path for the first
// administrator. It applies migrations first so it works on an empty database.
func runUserAdd(args []string) error {
	fs := flag.NewFlagSet("useradd", flag.ExitOnError)
	username := fs.String("username", "", "login name (required)")
	password := fs.String("password", "", "password, at least 8 characters (required)")
	display := fs.String("display-name", "", "display name (defaults to the username)")
	role := fs.String("role", string(auth.RoleAdmin), "AGENT, SUPERVISOR or ADMIN")
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

	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	svc := auth.NewService(st.Queries, cfg.SessionTTL)
	id, err := svc.CreateUser(ctx, auth.NewUser{
		Username:    *username,
		Password:    *password,
		DisplayName: *display,
		Role:        auth.Role(*role),
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "created user %s (%s) with role %s\n", id.Username, id.UserID, id.Role)
	return nil
}
