// Package schemamigrate is KMail's zero-downtime schema-migration
// runner (WS4 Task 4). It replaces the bare `psql`-loop in
// scripts/migrate.sh with a runner that:
//
//   - applies pending `migrations/NNN_*.sql` files in version order,
//   - records applied files in the existing `schema_migrations`
//     bookkeeping table (filename-keyed — backward compatible with
//     the old shell runner so a half-migrated DB keeps working),
//   - serialises runs behind a Postgres advisory lock so two
//     concurrent deploys can't apply the same migration twice,
//   - supports rollback via optional `migrations/NNN_*.down.sql`
//     companions.
//
// Migration files keep their existing names; the leading integer is
// the version and `.down.sql` marks a rollback companion. No file
// rename is required, which keeps this additive for sibling
// workstreams that own individual migration files.
package schemamigrate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is a fixed, arbitrary 64-bit key identifying the
// schema-migration critical section. Any process running migrations
// takes pg_advisory_lock(advisoryLockKey) first, so concurrent
// `migrate up`/`down` invocations queue rather than race. The constant
// is derived from "kmail.schema_migrations" and must stay stable.
const advisoryLockKey int64 = 0x6b6d61696c5f6d67 // "kmail_mg"

// upRe matches an up migration: NNN_description.sql but NOT .down.sql.
var upRe = regexp.MustCompile(`^(\d+)_.*\.sql$`)

// Migration is one discovered migration and its optional down file.
type Migration struct {
	Version  int
	Filename string // up filename, e.g. "006_feature_flags.sql"
	UpPath   string
	DownPath string // "" when no rollback companion exists
}

// Runner applies and rolls back migrations against a pool.
type Runner struct {
	pool   *pgxpool.Pool
	dir    string
	logger *log.Logger
}

// NewRunner builds a Runner. dir is the migrations directory.
func NewRunner(pool *pgxpool.Pool, dir string, logger *log.Logger) *Runner {
	if logger == nil {
		logger = log.Default()
	}
	return &Runner{pool: pool, dir: dir, logger: logger}
}

// Discover scans the migrations directory and returns up migrations in
// ascending version order, each paired with its `.down.sql` companion
// when present. Duplicate version numbers are an error — they make
// apply order ambiguous.
func Discover(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("schemamigrate: read dir: %w", err)
	}
	byVersion := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".down.sql") {
			continue // handled when pairing below
		}
		m := upRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if existing, ok := byVersion[version]; ok {
			return nil, fmt.Errorf("schemamigrate: duplicate migration version %d (%s and %s)", version, existing.Filename, name)
		}
		byVersion[version] = &Migration{
			Version:  version,
			Filename: name,
			UpPath:   filepath.Join(dir, name),
		}
	}
	// Pair down files: same version prefix + ".down.sql".
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".down.sql") {
			continue
		}
		dm := regexp.MustCompile(`^(\d+)_`).FindStringSubmatch(name)
		if dm == nil {
			continue
		}
		version, err := strconv.Atoi(dm[1])
		if err != nil {
			continue
		}
		if mig, ok := byVersion[version]; ok {
			mig.DownPath = filepath.Join(dir, name)
		}
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// withLock acquires a dedicated connection, takes the advisory lock,
// runs fn on that connection, then releases the lock and connection.
// Holding the lock on one session for the whole operation serialises
// concurrent runners.
func (r *Runner) withLock(ctx context.Context, fn func(conn *pgxpool.Conn) error) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("schemamigrate: acquire conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("schemamigrate: acquire advisory lock: %w", err)
	}
	defer func() {
		// Best-effort unlock; a context-cancelled unlock still frees
		// the lock when the session ends on Release.
		if _, uerr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey); uerr != nil {
			r.logger.Printf("schemamigrate: advisory unlock: %v", uerr)
		}
	}()
	return fn(conn)
}

// ensureTable creates the bookkeeping table if missing. Identical shape
// to scripts/migrate.sh's table so the two runners interoperate.
func ensureTable(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("schemamigrate: ensure bookkeeping table: %w", err)
	}
	return nil
}

func appliedSet(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT filename FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("schemamigrate: query applied: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out[f] = true
	}
	return out, rows.Err()
}

// execFile runs an entire SQL file via the simple query protocol so
// multi-statement files (and `$$`-quoted plpgsql) execute as one batch.
// Each migration file wraps its own BEGIN/COMMIT, so the file is
// atomic; bookkeeping is updated separately afterwards.
func execFile(ctx context.Context, conn *pgxpool.Conn, path string) error {
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("schemamigrate: read %s: %w", filepath.Base(path), err)
	}
	mrr := conn.Conn().PgConn().Exec(ctx, string(sqlBytes))
	if _, err := mrr.ReadAll(); err != nil {
		return fmt.Errorf("schemamigrate: exec %s: %w", filepath.Base(path), err)
	}
	if err := mrr.Close(); err != nil {
		return fmt.Errorf("schemamigrate: close %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Up applies all pending migrations in version order.
func (r *Runner) Up(ctx context.Context) error {
	migs, err := Discover(r.dir)
	if err != nil {
		return err
	}
	return r.withLock(ctx, func(conn *pgxpool.Conn) error {
		if err := ensureTable(ctx, conn); err != nil {
			return err
		}
		applied, err := appliedSet(ctx, conn)
		if err != nil {
			return err
		}
		var pending int
		for _, m := range migs {
			if applied[m.Filename] {
				continue
			}
			r.logger.Printf("schemamigrate: applying %s", m.Filename)
			if err := execFile(ctx, conn, m.UpPath); err != nil {
				return err
			}
			if _, err := conn.Exec(ctx,
				"INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT (filename) DO NOTHING",
				m.Filename); err != nil {
				return fmt.Errorf("schemamigrate: record %s: %w", m.Filename, err)
			}
			pending++
		}
		r.logger.Printf("schemamigrate: up complete (%d applied, %d already present)", pending, len(migs)-pending)
		return nil
	})
}

// ErrNoDownFile is returned when a rollback is requested for a
// migration that has no `.down.sql` companion.
var ErrNoDownFile = errors.New("schemamigrate: migration has no down file")

// Down rolls back the most recently applied `steps` migrations (highest
// version first). Every migration being rolled back MUST have a down
// file or the whole operation aborts before mutating anything.
func (r *Runner) Down(ctx context.Context, steps int) error {
	if steps <= 0 {
		return fmt.Errorf("schemamigrate: steps must be positive")
	}
	migs, err := Discover(r.dir)
	if err != nil {
		return err
	}
	byFilename := map[string]Migration{}
	for _, m := range migs {
		byFilename[m.Filename] = m
	}
	return r.withLock(ctx, func(conn *pgxpool.Conn) error {
		if err := ensureTable(ctx, conn); err != nil {
			return err
		}
		// Applied migrations, newest first, restricted to those we
		// know how to order (present on disk).
		rows, err := conn.Query(ctx, "SELECT filename FROM schema_migrations")
		if err != nil {
			return fmt.Errorf("schemamigrate: query applied: %w", err)
		}
		var appliedKnown []Migration
		var appliedNames []string
		for rows.Next() {
			var f string
			if err := rows.Scan(&f); err != nil {
				rows.Close()
				return err
			}
			appliedNames = append(appliedNames, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, f := range appliedNames {
			if m, ok := byFilename[f]; ok {
				appliedKnown = append(appliedKnown, m)
			}
		}
		sort.Slice(appliedKnown, func(i, j int) bool { return appliedKnown[i].Version > appliedKnown[j].Version })
		if steps > len(appliedKnown) {
			steps = len(appliedKnown)
		}
		target := appliedKnown[:steps]
		// Pre-flight: every target must have a down file.
		for _, m := range target {
			if m.DownPath == "" {
				return fmt.Errorf("%w: %s", ErrNoDownFile, m.Filename)
			}
		}
		for _, m := range target {
			r.logger.Printf("schemamigrate: rolling back %s", m.Filename)
			if err := execFile(ctx, conn, m.DownPath); err != nil {
				return err
			}
			if _, err := conn.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", m.Filename); err != nil {
				return fmt.Errorf("schemamigrate: unrecord %s: %w", m.Filename, err)
			}
		}
		r.logger.Printf("schemamigrate: down complete (%d rolled back)", len(target))
		return nil
	})
}

// Status reports each discovered migration and whether it is applied.
func (r *Runner) Status(ctx context.Context) ([]StatusRow, error) {
	migs, err := Discover(r.dir)
	if err != nil {
		return nil, err
	}
	var out []StatusRow
	err = r.withLock(ctx, func(conn *pgxpool.Conn) error {
		if err := ensureTable(ctx, conn); err != nil {
			return err
		}
		applied, err := appliedSet(ctx, conn)
		if err != nil {
			return err
		}
		for _, m := range migs {
			out = append(out, StatusRow{
				Version:  m.Version,
				Filename: m.Filename,
				Applied:  applied[m.Filename],
				HasDown:  m.DownPath != "",
			})
		}
		return nil
	})
	return out, err
}

// StatusRow is one row of Status output.
type StatusRow struct {
	Version  int
	Filename string
	Applied  bool
	HasDown  bool
}
