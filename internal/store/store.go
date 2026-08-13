// SPDX-License-Identifier: Apache-2.0

// Package store owns database access: the connection pool, embedded
// migrations, and the sqlc-generated query layer.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockKey guards against two application instances driving one
// FreeSWITCH and one database. It is a footgun guard, not leader election.
const advisoryLockKey int64 = 0x41494343 // "AICC"

// Store is the database facade handed to services.
type Store struct {
	Pool *pgxpool.Pool
	// Queries is the sqlc-generated query set bound to the pool.
	Queries *queries.Queries
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
