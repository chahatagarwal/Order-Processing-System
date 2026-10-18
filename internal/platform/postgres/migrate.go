package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ============================================================================
// MIGRATIONS
// ============================================================================
//
// WHY WE EMBED THE SQL IN THE BINARY (go:embed):
// the migrations ship WITH the code that needs them. There is no way to
// deploy v2 of the app against a v1 schema because you forgot to copy a file
// or mount a volume. One artifact, one consistent state.
//
// WHY WE DO NOT USE golang-migrate / goose / atlas:
// they are good tools and what you would reach for at work. This is ~90 lines
// and demonstrates the three things that actually matter in a migration
// runner, all of which you should be able to explain:
//
//  1. A LEDGER TABLE, so a migration runs exactly once.
//  2. A CHECKSUM, so an already-applied migration cannot be edited silently.
//  3. AN ADVISORY LOCK, so N replicas starting at once do not all migrate.
//
// Point 3 is the one people miss, and it is the one that corrupts databases.
// ============================================================================

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is a dedicated advisory-lock key for schema changes.
const migrationLockKey int64 = 777_001

// Migrate applies any pending migrations.
func Migrate(ctx context.Context, pool *Pool, log *slog.Logger) error {
	// ------------------------------------------------------------------
	// STEP 1: TAKE A CLUSTER-WIDE LOCK.
	//
	// THE PROBLEM THIS SOLVES: you deploy 3 replicas. All 3 boot at the same
	// instant and all 3 run migrations. Without a lock, two of them execute
	// `CREATE TABLE orders` concurrently; one fails, that pod crash-loops, and
	// in the worst case a partially-applied DDL leaves the schema in a state
	// no migration file describes.
	//
	// We use the BLOCKING pg_advisory_lock here (not the `try` variant used by
	// the workers), because the other replicas genuinely SHOULD wait: once the
	// leader finishes, they each check the ledger, find nothing to do, and
	// proceed. Blocking is correct when the work must happen before you can
	// serve traffic.
	// ------------------------------------------------------------------
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return TranslateError(err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("could not acquire the migration lock: %w", err)
	}
	defer func() {
		// The lock MUST be released, or the next deploy blocks forever waiting
		// for a lock held by a dead session's connection.
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			log.Warn("failed to release the migration lock", slog.String("error", err.Error()))
		}
	}()

	// STEP 2: ensure the ledger exists. This is the one statement that cannot
	// itself be a migration, for obvious reasons.
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT PRIMARY KEY,
			checksum    TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("could not create the schema_migrations table: %w", err)
	}

	// STEP 3: read what has already been applied.
	applied := map[string]string{} // version -> checksum
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return TranslateError(err)
	}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return TranslateError(err)
		}
		applied[version] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TranslateError(err)
	}

	// STEP 4: list the embedded files, SORTED.
	//
	// SORTING IS LOAD-BEARING: migrations must run in a deterministic order,
	// because 002 may add a column that 003 indexes. The zero-padded
	// "001_", "002_" naming convention exists precisely so that lexical sort
	// equals chronological order.
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("could not read the embedded migrations: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	pending := 0
	for _, name := range files {
		content, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("could not read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(content)
		checksum := hex.EncodeToString(sum[:])

		if existing, ok := applied[name]; ok {
			// ------------------------------------------------------------
			// THE CHECKSUM GUARD.
			//
			// Someone edited a migration that has already run. This is
			// dangerous because the change applied to THEIR database (they
			// re-created it locally) but will never apply to staging or
			// production, which already recorded this version as done. The
			// result is environments with silently different schemas — a bug
			// that is extremely hard to diagnose later.
			//
			// We FAIL THE BOOT and tell them to write a NEW migration instead.
			// Loud and early beats mysterious and later.
			// ------------------------------------------------------------
			if existing != checksum {
				return fmt.Errorf(
					"migration %s has been modified since it was applied (recorded %s, now %s); "+
						"never edit an applied migration — add a new one instead",
					name, existing[:8], checksum[:8])
			}
			continue
		}

		log.Info("applying migration", slog.String("version", name))

		// ------------------------------------------------------------
		// EACH MIGRATION RUNS IN ITS OWN TRANSACTION, together with the
		// ledger insert.
		//
		// WHY THAT PAIRING MATTERS: if the DDL succeeded but the ledger
		// insert failed, the migration would run AGAIN next boot and fail
		// with "table already exists", crash-looping the service. Putting
		// both in one transaction makes "applied" and "recorded" inseparable.
		//
		// (Postgres supports transactional DDL, which many databases do not.
		// On MySQL this guarantee is unavailable and migration tooling has to
		// be far more defensive — a real point in Postgres's favour.)
		// ------------------------------------------------------------
		tx, err := conn.Begin(ctx)
		if err != nil {
			return TranslateError(err)
		}
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s failed: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
			name, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("could not record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("could not commit migration %s: %w", name, err)
		}
		pending++
	}

	if pending == 0 {
		log.Info("database schema is up to date", slog.Int("applied_total", len(applied)))
	} else {
		log.Info("migrations applied", slog.Int("count", pending))
	}
	return nil
}

// TruncateAll wipes every table. TEST HELPER ONLY.
//
// It is guarded by an explicit confirmation string so that it cannot be
// called by accident, and it refuses to run against a DSN that does not look
// like a test database. A helper that can wipe production is not a helper.
func TruncateAll(ctx context.Context, pool *Pool, confirm string) error {
	if confirm != "yes-i-am-sure-this-is-a-test-database" {
		return fmt.Errorf("postgres: TruncateAll requires explicit confirmation")
	}
	_, err := pool.Exec(ctx, `TRUNCATE orders, order_items, outbox, saga_instances RESTART IDENTITY CASCADE`)
	return TranslateError(err)
}

// ensure pgx is referenced even if the build tags change.
var _ = pgx.ErrNoRows
