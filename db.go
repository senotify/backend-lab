package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	TotalRows      = 10_000_000
	PageSize       = 20
	TotalPages     = TotalRows / PageSize
	CheckpointStep = 1000 // one checkpoint row per 1,000 items
	seedVersion    = 2    // bump to force a reseed with a new dataset shape
)

// DB holds two pools: admin (setup, seeding, the sweep) and a read-only pool
// for visitor-triggered queries, which runs as a role with a statement
// timeout and read-only transactions, so a visitor can never write or hang it.
type DB struct {
	admin *pgxpool.Pool
	ro    *pgxpool.Pool
	ready atomic.Bool
	state atomic.Value // string: human status while seeding
}

func connString(user, pass string) string {
	host := envOr("PGHOST", "lab-postgres")
	return fmt.Sprintf("postgres://%s:%s@%s:5432/postgres?sslmode=disable", user, pass, host)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func OpenDB(ctx context.Context) (*DB, error) {
	pass := os.Getenv("LAB_DB_PASSWORD")
	if pass == "" {
		return nil, fmt.Errorf("LAB_DB_PASSWORD is not set")
	}
	acfg, err := pgxpool.ParseConfig(connString("postgres", pass))
	if err != nil {
		return nil, err
	}
	acfg.MaxConns = 2
	var admin *pgxpool.Pool
	// Postgres may still be starting when the app boots.
	for i := 0; i < 60; i++ {
		admin, err = pgxpool.NewWithConfig(ctx, acfg)
		if err == nil {
			if err = admin.Ping(ctx); err == nil {
				break
			}
			admin.Close()
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres not reachable: %w", err)
	}
	d := &DB{admin: admin}
	d.state.Store("starting")
	return d, nil
}

// Setup creates the schema, the read-only role and (once) seeds the dataset.
// It runs in the background; the API answers 503 until it's done.
func (d *DB) Setup(ctx context.Context) error {
	pass := os.Getenv("LAB_DB_PASSWORD")
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS lab_meta (k text PRIMARY KEY, v text NOT NULL)`,
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lab_ro') THEN
		     CREATE ROLE lab_ro LOGIN;
		   END IF;
		 END $$`,
		fmt.Sprintf(`ALTER ROLE lab_ro PASSWORD %s`, quoteLiteral(pass)),
		`ALTER ROLE lab_ro SET statement_timeout = '5s'`,
		`ALTER ROLE lab_ro SET default_transaction_read_only = on`,
		`ALTER ROLE lab_ro SET work_mem = '4MB'`,
	}
	for _, s := range stmts {
		if _, err := d.admin.Exec(ctx, s); err != nil {
			return fmt.Errorf("setup %q: %w", firstLine(s), err)
		}
	}

	var have string
	_ = d.admin.QueryRow(ctx, `SELECT v FROM lab_meta WHERE k = 'seed_version'`).Scan(&have)
	// items is UNLOGGED (fast to load, no WAL), so Postgres empties it after a
	// crash. Check the last row actually exists, not just the version marker.
	var present bool
	_ = d.admin.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM items WHERE id = %d)`, TotalRows)).Scan(&present)
	if have != fmt.Sprint(seedVersion) || !present {
		if err := d.seed(ctx); err != nil {
			return err
		}
	}
	for _, s := range []string{
		`GRANT SELECT ON items, item_checkpoints TO lab_ro`,
	} {
		if _, err := d.admin.Exec(ctx, s); err != nil {
			return err
		}
	}

	rcfg, err := pgxpool.ParseConfig(connString("lab_ro", pass))
	if err != nil {
		return err
	}
	// Deliberately tiny: if 50 people click "OFFSET to the last page" at once,
	// they queue here instead of starving every other app on the server.
	rcfg.MaxConns = 3
	ro, err := pgxpool.NewWithConfig(ctx, rcfg)
	if err != nil {
		return err
	}
	d.ro = ro
	d.ready.Store(true)
	d.state.Store("ready")
	return nil
}

func (d *DB) seed(ctx context.Context) error {
	start := time.Now()
	d.state.Store("seeding 10,000,000 rows (first boot only, a few minutes)")
	log.Println("seeding dataset")
	steps := []string{
		`DROP TABLE IF EXISTS item_checkpoints`,
		`DROP TABLE IF EXISTS items`,
		// created_at repeats in groups of three, so the (created_at, id)
		// tiebreaker genuinely matters, like real timestamps do.
		`CREATE UNLOGGED TABLE items (
		   id         bigint PRIMARY KEY,
		   created_at timestamptz NOT NULL,
		   title      text NOT NULL,
		   body       text NOT NULL
		 )`,
		fmt.Sprintf(`INSERT INTO items (id, created_at, title, body)
		 SELECT g,
		        timestamptz '2026-11-01 00:00:00+00' - ((%d - g) / 3) * interval '17 seconds',
		        'Post #' || g,
		        md5(g::text) || md5((g * 7)::text) || md5((g * 13)::text)
		 FROM generate_series(1, %d) AS g`, TotalRows, TotalRows),
		`SET maintenance_work_mem = '256MB'`,
		`CREATE INDEX items_feed ON items (created_at DESC, id DESC)`,
		// The dataset is static, so checkpoints are exact and never stale.
		// (On live data you'd refresh this periodically; see the lab page.)
		fmt.Sprintf(`CREATE UNLOGGED TABLE item_checkpoints AS
		 SELECT ((rn - 1) / %d)::int AS bucket, created_at, id
		 FROM (SELECT created_at, id,
		              row_number() OVER (ORDER BY created_at DESC, id DESC) AS rn
		       FROM items) t
		 WHERE rn %% %d = 1`, CheckpointStep, CheckpointStep),
		`ALTER TABLE item_checkpoints ADD PRIMARY KEY (bucket)`,
		`VACUUM (ANALYZE) items`,
		`ANALYZE item_checkpoints`,
		fmt.Sprintf(`INSERT INTO lab_meta VALUES ('seed_version', '%d')
		 ON CONFLICT (k) DO UPDATE SET v = excluded.v`, seedVersion),
	}
	// One connection for the whole seed, so the session SET applies to the
	// CREATE INDEX that follows it.
	conn, err := d.admin.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	for _, s := range steps {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("seed %q: %w", firstLine(s), err)
		}
	}
	log.Printf("seeded in %s", time.Since(start).Round(time.Millisecond))
	return nil
}

// quoteLiteral makes a SQL string literal. Only used for our own password in
// ALTER ROLE, which can't take a bind parameter.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
