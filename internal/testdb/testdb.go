// SPDX-License-Identifier: Apache-2.0

// Package testdb gives a test a throwaway PostgreSQL database of its own.
//
// Set AICC_TEST_DATABASE_URL to a superuser-capable DSN (the dev stack's is
// postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable) and the tests that
// use it run; without it they skip. It imports nothing from this module, so
// internal/store's own tests can use it without an import cycle.
package testdb

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const dsnEnv = "AICC_TEST_DATABASE_URL"

// ScratchDSN creates an empty database named aicc_<prefix>_<UnixNano>, drops
// it WITH (FORCE) when the test ends, and returns a DSN for it. Each test gets
// its own, because migrating is a whole-database act.
func ScratchDSN(t testing.TB, prefix string) string {
	t.Helper()
	admin := os.Getenv(dsnEnv)
	if admin == "" {
		t.Skipf("set %s to run this test", dsnEnv)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", dsnEnv, err)
	}
	name := fmt.Sprintf("aicc_%s_%d", prefix, time.Now().UnixNano())

	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u.Path = "/" + name
	return u.String()
}
