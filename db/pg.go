package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/digitalocean/tester"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

var psq = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

type pger interface {
	Exec(ctx context.Context, sql string, arguments ...interface{}) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row
}

type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ DB = (*PG)(nil)

func NewPG(pool *pgxpool.Pool) *PG {
	return &PG{
		pool: pool,
		now:  time.Now,
	}
}

// migrator builds a tern Migrator on conn with pgMigrations appended. Every
// migration that actually runs is logged, so both `tester serve` (when it
// migrates at startup) and `tester migrate` show what they applied.
func (p *PG) migrator(ctx context.Context, conn *pgx.Conn) (*migrate.Migrator, error) {
	m, err := migrate.NewMigrator(ctx, conn, "versions")
	if err != nil {
		return nil, err
	}
	m.OnStart = func(sequence int32, name, direction, _ string) {
		log.Printf("db: applying migration %d %q (%s)", sequence, name, direction)
	}
	for _, migration := range pgMigrations {
		m.AppendMigration(migration.name, migration.up, migration.down)
	}
	return m, nil
}

// Init applies any pending migrations from pgMigrations, under tern's
// advisory lock and each in its own transaction. It is a no-op when the
// schema is already current.
func (p *PG) Init(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	m, err := p.migrator(ctx, conn.Conn())
	if err != nil {
		return err
	}
	return m.Migrate(ctx)
}

// CheckSchema verifies that every migration in pgMigrations has been applied
// without applying anything. It is for processes that must not migrate (a
// server whose migrations run in a separate pre-deploy job): the returned
// error names the current and expected versions so a deploy that started the
// server before migrating fails fast instead of serving a stale schema.
//
// A schema newer than this binary knows about is tolerated so that rolling
// back to a previous image does not take the server down; migrations are
// expected to stay backwards compatible for one release. Building the
// migrator takes tern's advisory lock briefly, so this blocks while another
// process is mid-migration and reports the post-migration version.
func (p *PG) CheckSchema(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	m, err := p.migrator(ctx, conn.Conn())
	if err != nil {
		return err
	}
	current, err := m.GetCurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	expected := int32(len(pgMigrations))
	if current < expected {
		return fmt.Errorf("database schema is at version %d, this binary expects %d: run `tester migrate`", current, expected)
	}
	return nil
}

func (p *PG) tx(ctx context.Context, f func(tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	err = f(tx)
	if err != nil {
		return err
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

func (p *PG) AddTest(ctx context.Context, test *tester.Test) error {
	t := (*pgTest)(test)
	q := psq.Insert("tests").
		Columns(t.Columns()...).
		Values(t.Values()...)

	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}

	_, err = p.pool.Exec(ctx, sql, args...)
	return err
}

func (p *PG) GetTest(ctx context.Context, id uuid.UUID) (*tester.Test, error) {
	test := &pgTest{}
	q := psq.Select(test.Columns()...).
		From("tests").
		Where("id = ?", id)

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, err
	}

	row := p.pool.QueryRow(ctx, sql, args...)

	err = test.Scan(row)
	if err != nil {
		return nil, err
	}
	return (*tester.Test)(test), nil
}

func (p *PG) listTests(ctx context.Context, pg pger, pred interface{}, limit int) ([]*tester.Test, error) {
	var tests []*tester.Test
	q := psq.Select((&pgTest{}).Columns()...).
		From("tests").
		OrderBy("result->'started_at' ASC")

	if pred != nil {
		q = q.Where(pred)
	}

	if limit > 0 {
		q = q.Limit(uint64(limit))
	}

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, err
	}

	rows, err := pg.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		t := &pgTest{}
		err := t.Scan(rows)
		if err != nil {
			return nil, err
		}
		tests = append(tests, (*tester.Test)(t))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tests, nil
}

func (p *PG) ListTests(ctx context.Context, limit int) ([]*tester.Test, error) {
	return p.listTests(ctx, p.pool, nil, limit)
}

func (p *PG) ListTestsForPackage(ctx context.Context, pkg string, limit int) ([]*tester.Test, error) {
	return p.listTests(ctx, p.pool, sq.Eq{"package": pkg}, limit)
}

func (p *PG) ListTestsInDateRange(ctx context.Context, from, to time.Time) ([]*tester.Test, error) {
	return p.listTests(ctx, p.pool, nil, 0)
}

func (p *PG) ListTestsForPackageInRange(ctx context.Context, pkg string, from, to time.Time) ([]*tester.Test, error) {
	return p.listTests(ctx, p.pool, sq.And{
		sq.Eq{"package": pkg},
		sq.Expr("result->'started_at' >= ?", from),
		sq.Expr("result->'started_at' <= ?", to),
	}, 0)
}

func (p *PG) EnqueueRun(ctx context.Context, run *tester.Run) error {
	r := (*pgRun)(run)
	q := psq.Insert("runs").
		Columns(r.Columns()...).
		Values(r.Values()...)

	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}

	_, err = p.pool.Exec(ctx, sql, args...)
	return err
}

func (p *PG) StartRun(ctx context.Context, id uuid.UUID, runner string) error {
	return p.tx(ctx, func(tx pgx.Tx) error {
		r := &pgRun{}
		q := psq.Select(r.Columns()...).
			From("runs").
			Where("id = ?", id)

		sql, args, err := q.ToSql()
		if err != nil {
			return err
		}

		row := p.pool.QueryRow(ctx, sql, args...)
		err = r.Scan(row)
		if err != nil {
			return err
		}

		r.Meta.Runner = runner

		uq := psq.Update("runs").
			Set("started_at", p.now()).
			Set("meta", jsonb(r.Meta)).
			Where("id = ?", id)

		sql, args, err = uq.ToSql()
		if err != nil {
			return err
		}

		_, err = p.pool.Exec(ctx, sql, args...)
		return err
	})

}

// ScheduleRun enqueues run only if the package has no unfinished run and its
// most recent run was enqueued at least minInterval ago. The check and the
// insert happen under a per-package transaction-scoped advisory lock, so any
// number of server replicas calling this concurrently converge on exactly one
// run per package per interval without process-local state.
func (p *PG) ScheduleRun(ctx context.Context, run *tester.Run, minInterval time.Duration) (bool, error) {
	scheduled := false
	err := p.tx(ctx, func(tx pgx.Tx) error {
		// hashtext returns int4, which pg_advisory_xact_lock(bigint) accepts.
		// The lock is released automatically at commit/rollback.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", run.Package); err != nil {
			return fmt.Errorf("locking package %q: %w", run.Package, err)
		}

		var (
			pending        int
			lastEnqueuedAt sql.NullTime
		)
		err := tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE finished_at IS NULL), max(enqueued_at) FROM runs WHERE package = $1`,
			run.Package,
		).Scan(&pending, &lastEnqueuedAt)
		if err != nil {
			return fmt.Errorf("inspecting runs for package %q: %w", run.Package, err)
		}
		if pending > 0 {
			return nil
		}
		if lastEnqueuedAt.Valid && p.now().Sub(lastEnqueuedAt.Time) < minInterval {
			return nil
		}

		r := (*pgRun)(run)
		q := psq.Insert("runs").
			Columns(r.Columns()...).
			Values(r.Values()...)
		sql, args, err := q.ToSql()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return err
		}
		scheduled = true
		return nil
	})
	return scheduled, err
}

// ClaimRun atomically hands the oldest claimable run to runner. A run is
// claimable when it has not started, has not finished, its package is in
// include, and its package is not in exclude. The row is selected FOR UPDATE
// SKIP LOCKED so concurrent claimers never receive the same run. Returns
// ErrNotFound when nothing is claimable.
func (p *PG) ClaimRun(ctx context.Context, runner string, include, exclude []string) (*tester.Run, error) {
	if include == nil {
		include = []string{}
	}
	if exclude == nil {
		exclude = []string{}
	}

	r := &pgRun{}
	query := fmt.Sprintf(`
WITH candidate AS (
	SELECT id FROM runs
	WHERE started_at IS NULL
	  AND finished_at IS NULL
	  AND package = ANY($1)
	  AND NOT (package = ANY($2))
	ORDER BY enqueued_at ASC
	LIMIT 1
	FOR UPDATE SKIP LOCKED
)
UPDATE runs
SET started_at = $3,
    meta = jsonb_set(meta, '{runner}', to_jsonb($4::text))
FROM candidate
WHERE runs.id = candidate.id
RETURNING %s`, strings.Join(r.qualifiedColumns(), ", "))

	row := p.pool.QueryRow(ctx, query, include, exclude, p.now(), runner)
	if err := r.Scan(row); err != nil {
		return nil, err
	}
	return (*tester.Run)(r), nil
}

// ResetRun returns a started-but-unfinished run to the queue. The run is
// re-enqueued at the back (enqueued_at = now) rather than keeping its original
// position, and its reset_count is incremented so the scheduler can stop
// retrying it after a bounded number of attempts.
func (p *PG) ResetRun(ctx context.Context, id uuid.UUID) error {
	q := psq.Update("runs").
		SetMap(map[string]interface{}{
			"started_at":  sql.NullTime{},
			"finished_at": sql.NullTime{},
			"error":       sql.NullString{},
			"meta":        jsonb(tester.RunMeta{}),
			"enqueued_at": p.now(),
		}).
		Set("reset_count", sq.Expr("reset_count + 1")).
		Where("id = ?", id).
		Where("finished_at IS NULL")

	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}

	res, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PG) DeleteRun(ctx context.Context, id uuid.UUID) error {
	q := psq.Delete("runs").
		Where("id = ?", id)

	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}

	_, err = p.pool.Exec(ctx, sql, args...)
	return err
}

func (p *PG) CompleteRun(ctx context.Context, id uuid.UUID) error {
	q := psq.Update("runs").
		Set("finished_at", sql.NullTime{Valid: true, Time: p.now()}).
		Where("id = ?", id)

	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}

	_, err = p.pool.Exec(ctx, sql, args...)
	return err
}

func (p *PG) FailRun(ctx context.Context, id uuid.UUID, error string) error {
	q := psq.Update("runs").
		SetMap(map[string]interface{}{
			"finished_at": sql.NullTime{Valid: true, Time: p.now()},
			"error":       sql.NullString{Valid: true, String: error},
		}).
		Where("id = ?", id)

	sql, args, err := q.ToSql()
	if err != nil {
		return err
	}

	_, err = p.pool.Exec(ctx, sql, args...)
	return err
}

func (p *PG) GetRun(ctx context.Context, id uuid.UUID) (*tester.Run, error) {
	var run *tester.Run
	err := p.tx(ctx, func(tx pgx.Tx) error {
		r := &pgRun{}
		q := psq.Select(r.Columns()...).
			From("runs").
			Where("id = ?", id)

		sql, args, err := q.ToSql()
		if err != nil {
			return err
		}

		row := p.pool.QueryRow(ctx, sql, args...)
		err = r.Scan(row)
		if err != nil {
			return err
		}
		run = (*tester.Run)(r)
		tests, err := p.listTests(ctx, tx, sq.Eq{"run_id": id}, 0)
		if err != nil {
			return err
		}

		run.Tests = tests
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (p *PG) listRuns(ctx context.Context, pg pger, pred interface{}, order string, limit int) ([]*tester.Run, error) {
	var runs []*tester.Run
	q := psq.Select((&pgRun{}).Columns()...).
		From("runs")

	if pred != nil {
		q = q.Where(pred)
	}
	if order != "" {
		q = q.OrderBy(order)
	}
	if limit > 0 {
		q = q.Limit(uint64(limit))
	}

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, err
	}

	rows, err := pg.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runMap := make(map[uuid.UUID]*tester.Run)
	for rows.Next() {
		r := &pgRun{}
		err := r.Scan(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, (*tester.Run)(r))
		runMap[r.ID] = (*tester.Run)(r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var runIDs []uuid.UUID
	for id := range runMap {
		runIDs = append(runIDs, id)
	}

	tests, err := p.listTests(ctx, pg, sq.Eq{"run_id": runIDs}, 0)
	if err != nil {
		return nil, err
	}

	for _, test := range tests {
		runMap[test.RunID].Tests = append(runMap[test.RunID].Tests, test)
	}

	return runs, nil
}

func (p *PG) ListPendingRuns(ctx context.Context) ([]*tester.Run, error) {
	var runs []*tester.Run
	err := p.tx(ctx, func(tx pgx.Tx) error {
		var err error
		runs, err = p.listRuns(ctx, tx, "finished_at IS NULL", "enqueued_at ASC", 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

func (p *PG) ListFinishedRuns(ctx context.Context, limit int) ([]*tester.Run, error) {
	var runs []*tester.Run
	err := p.tx(ctx, func(tx pgx.Tx) error {
		var err error
		runs, err = p.listRuns(ctx, tx, "finished_at IS NOT NULL", "finished_at DESC", limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

func (p *PG) ListRunsForPackage(ctx context.Context, pkg string, limit int) ([]*tester.Run, error) {
	var runs []*tester.Run
	err := p.tx(ctx, func(tx pgx.Tx) error {
		var err error
		runs, err = p.listRuns(ctx, tx, sq.Eq{"package": pkg}, "enqueued_at DESC", limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

func (p *PG) ListRunSummariesInRange(ctx context.Context, begin, end time.Time, window time.Duration) ([]*tester.RunSummary, error) {
	begin = begin.UTC()
	end = end.UTC()

	buckets := int(math.Ceil(float64(end.Sub(begin)) / float64(window)))
	summaries := make([]*tester.RunSummary, buckets)
	for i := 0; i < buckets; i++ {
		summaries[i] = &tester.RunSummary{
			Time:           begin.Add(time.Duration(i) * window),
			Duration:       window,
			PackageSummary: make(map[string]*tester.PackageSummary),
		}
	}

	err := p.tx(ctx, func(tx pgx.Tx) error {
		q := psq.Select("runs.package", "runs.id", "runs.started_at", "runs.error", "tests.id", "tests.result").
			From("tests").
			Join("runs ON tests.run_id = runs.id").
			Where("runs.started_at IS NOT NULL").
			Where("runs.started_at >= ?", begin).
			Where("runs.started_at <= ?", end).
			Where("runs.finished_at IS NOT NULL").
			OrderBy("runs.started_at ASC")

		query, args, err := q.ToSql()
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				packageName  string
				runID        uuid.UUID
				runStartedAt time.Time
				runError     sql.NullString
				testID       uuid.UUID
				result       tester.T
			)
			err := rows.Scan(&packageName, &runID, &runStartedAt, &runError, &testID, &result)
			if err != nil {
				return err
			}
			runStartedAt = runStartedAt.UTC()

			bucketIndex := int(runStartedAt.Sub(begin) / window)
			summary := summaries[bucketIndex]

			packageSummary, ok := summary.PackageSummary[packageName]
			if !ok {
				packageSummary = &tester.PackageSummary{
					Package:      packageName,
					PassedTests:  make(map[string][]uuid.UUID),
					FailedTests:  make(map[string][]uuid.UUID),
					SkippedTests: make(map[string][]uuid.UUID),
				}
				summary.PackageSummary[packageName] = packageSummary
			}

			// NOTE(nan) we blindly add here and uniquify later.
			if runError.Valid {
				packageSummary.ErrorRunIDs = append(packageSummary.ErrorRunIDs, runID)
				continue
			}
			packageSummary.RunIDs = append(packageSummary.RunIDs, runID)

			switch result.State {
			case tester.TBStatePassed:
				packageSummary.PassedTests[result.Name] = append(packageSummary.PassedTests[result.Name], testID)
			case tester.TBStateFailed:
				packageSummary.FailedTests[result.Name] = append(packageSummary.FailedTests[result.Name], testID)
			case tester.TBStateSkipped:
				packageSummary.SkippedTests[result.Name] = append(packageSummary.SkippedTests[result.Name], testID)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	for _, summary := range summaries {
		for _, packageSummary := range summary.PackageSummary {
			if len(packageSummary.RunIDs) == 0 {
				continue
			}

			var runIDs []uuid.UUID
			uniqueRunIDs := make(map[uuid.UUID]struct{})
			for _, id := range packageSummary.RunIDs {
				if _, exists := uniqueRunIDs[id]; exists {
					continue
				}
				uniqueRunIDs[id] = struct{}{}
				runIDs = append(runIDs, id)
			}
			packageSummary.RunIDs = runIDs

			var errorRunIDs []uuid.UUID
			uniqueErrorRunIDs := make(map[uuid.UUID]struct{})
			for _, id := range packageSummary.ErrorRunIDs {
				if _, exists := uniqueErrorRunIDs[id]; exists {
					continue
				}
				uniqueErrorRunIDs[id] = struct{}{}
				errorRunIDs = append(errorRunIDs, id)
			}
			packageSummary.ErrorRunIDs = errorRunIDs
		}
	}
	return summaries, nil
}
