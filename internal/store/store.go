// SPDX-License-Identifier: Apache-2.0

// Package store owns database access: the connection pool, embedded
// migrations, and the sqlc-generated query layer.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockKey guards against two application instances driving one
// FreeSWITCH and one database. It is a footgun guard, not leader election.
const advisoryLockKey int64 = 0x41494343 // "AICC"

// extensionPoolLockKey serialises extension-number allocation.
//
// It must not be advisoryLockKey, and the reason is easy to miss: session-level
// and transaction-level advisory locks share one keyspace, and this process
// holds the instance lock on a dedicated connection for its whole life.
// Reusing that key would make the first allocation wait for a lock we are
// never going to release — a hang with no error and no timeout.
const extensionPoolLockKey int64 = 0x414943430001 // "AICC" + 1

// Store is the database facade handed to services.
type Store struct {
	Pool *pgxpool.Pool
	// Queries is the sqlc-generated query set bound to the pool.
	Queries *queries.Queries
	// OnCallbackSettled, when set, is told about every callback whose last
	// attempt just learned how it went — from a CDR landing. The store cannot
	// reach the event stream, so whoever wires it up hands in the announcer.
	OnCallbackSettled func(Callback)
	// OnAttemptFinished, when set, is told about every campaign attempt whose
	// outcome was just written or revised, after the transaction holds. The
	// dialer's actor wakes on it; the store cannot reach the actor, so whoever
	// wires it up hands it in.
	OnAttemptFinished func(campaign.Finished)
	// OutboundHours are the deployment's outer calling hours
	// (AICC_OUTBOUND_HOURS). A campaign contact's retry is never scheduled
	// outside them. The zero value means no outer limit.
	OutboundHours dayrange.Range
	// FlowPublishRules are the checks a flow must pass, beyond loading, before
	// this deployment will let it answer a call. They depend on what the
	// installation runs — which speech provider, today — and the store has no
	// way to know that, so whoever wires it up hands them in. Every path that
	// publishes goes through FlowStore, which is what makes one place enough.
	FlowPublishRules []flow.Rule
}

// Open creates the pool, verifies connectivity and takes the single-instance
// advisory lock. The lock is held on a dedicated connection for process life.
func Open(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Store{Pool: pool, Queries: queries.New(pool)}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Migrate applies all embedded migrations.
func (s *Store) Migrate(ctx context.Context) error {
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrationState is where the database's schema stands against the
// migrations embedded in this binary.
type MigrationState struct {
	// DBVersion is the highest version the database records as applied; 0
	// when it has never been migrated.
	DBVersion int64
	// LatestVersion is the highest migration this binary carries.
	LatestVersion int64
	// HasPending is whether this binary has a migration the database lacks.
	HasPending bool
}

// MigrationStatus reports the schema version without changing anything.
//
// It is what `aicc doctor` asks, so it must be safe against a database that a
// running server owns, and against one nobody has migrated yet. The goose
// Provider's GetVersions and HasPending are not safe on their own for the
// second case: both create goose_db_version when it is missing (v3.27.3,
// Provider.initialize → ensureVersionTable). So the table's existence is
// asked first, through goose's own query, and the Provider is only consulted
// once there is a table for it to read — at which point it writes nothing.
func (s *Store) MigrationStatus(ctx context.Context) (MigrationState, error) {
	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return MigrationState{}, fmt.Errorf("migrations directory: %w", err)
	}
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()

	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations,
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return MigrationState{}, fmt.Errorf("read migrations: %w", err)
	}
	sources := p.ListSources()
	state := MigrationState{LatestVersion: sources[len(sources)-1].Version}

	versions, err := database.NewStore(database.DialectPostgres, goose.DefaultTablename)
	if err != nil {
		return MigrationState{}, fmt.Errorf("version store: %w", err)
	}
	lookup, ok := versions.(database.StoreExtender)
	if !ok {
		return MigrationState{}, errors.New("version store cannot look for its own table")
	}
	exists, err := lookup.TableExists(ctx, db)
	if err != nil {
		return MigrationState{}, fmt.Errorf("look for the version table: %w", err)
	}
	if !exists {
		state.HasPending = true
		return state, nil
	}

	if state.DBVersion, _, err = p.GetVersions(ctx); err != nil {
		return MigrationState{}, fmt.Errorf("read schema version: %w", err)
	}
	if state.HasPending, err = p.HasPending(ctx); err != nil {
		return MigrationState{}, fmt.Errorf("compare schema version: %w", err)
	}
	return state, nil
}

// ErrInstanceLocked reports that another instance holds the advisory lock.
var ErrInstanceLocked = errors.New("another aicc instance is already running against this database")

// InstanceLock holds the single-instance advisory lock for process lifetime.
type InstanceLock struct{ conn *pgxpool.Conn }

// AcquireInstanceLock takes the advisory lock, or returns ErrInstanceLocked.
func (s *Store) AcquireInstanceLock(ctx context.Context) (*InstanceLock, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, fmt.Errorf("advisory lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, ErrInstanceLocked
	}
	return &InstanceLock{conn: conn}, nil
}

// Release drops the advisory lock.
func (l *InstanceLock) Release(ctx context.Context) {
	if l == nil || l.conn == nil {
		return
	}
	_, _ = l.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	l.conn.Release()
	l.conn = nil
}
