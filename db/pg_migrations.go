package db

var pgMigrations = []struct {
	name string
	up   string
	down string
}{
	{
		name: "initial",
		up: `
CREATE TABLE tests (
	id uuid PRIMARY KEY,
	package varchar(255) NOT NULL,
	run_id uuid NOT NULL,
	result jsonb NOT NULL,
	logs jsonb NOT NULL
);
CREATE INDEX ON tests (package);
CREATE INDEX ON tests ((result->'started_at'));

CREATE TABLE runs (
	id uuid PRIMARY KEY,
	package varchar(255) NOT NULL,
	args varchar(255)[],
	enqueued_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
	started_at timestamptz,
	finished_at timestamptz,
	error text
);
CREATE INDEX ON runs (package);
CREATE INDEX ON runs (enqueued_at, started_at);
CREATE INDEX ON runs (finished_at);
`,
		down: `
DROP TABLE tests, runs;
`,
	},
	{
		name: "add index on runs started_at, finished_at",
		up: `
CREATE INDEX ON runs (started_at, finished_at);
`,
		down: `
DROP INDEX runs_started_at_finished_at_idx;
`,
	},
	{
		name: "add meta column to runs",
		up: `
ALTER TABLE runs ADD COLUMN meta jsonb NOT NULL DEFAULT '{}'::jsonb;
`,
		down: `
ALTER TABLE runs DROP COLUMN meta;
`,
	},
	{
		name: "add reset_count column to runs",
		up: `
ALTER TABLE runs ADD COLUMN reset_count integer NOT NULL DEFAULT 0;
`,
		down: `
ALTER TABLE runs DROP COLUMN reset_count;
`,
	},
	{
		name: "add tests run_id index and runs package covering index",
		up: `
-- Built out of band with CREATE INDEX CONCURRENTLY on the production
-- cluster (tern runs migrations in a transaction, which CONCURRENTLY does
-- not allow, and a plain CREATE INDEX would lock tests for minutes at
-- startup). IF NOT EXISTS makes this a no-op there; it creates the indexes
-- on fresh databases. Names must match the hand-built ones exactly.
CREATE INDEX IF NOT EXISTS tests_run_id_idx ON tests (run_id);
CREATE INDEX IF NOT EXISTS runs_package_enqueued_at_idx ON runs (package, enqueued_at DESC) INCLUDE (finished_at);
`,
		down: `
DROP INDEX IF EXISTS runs_package_enqueued_at_idx;
DROP INDEX IF EXISTS tests_run_id_idx;
`,
	},
	{
		name: "tune autovacuum for runs and tests",
		up: `
-- The default autovacuum scale factors (0.2 vacuum / 0.1 analyze / 0.2
-- insert) are relative to table size. At production sizes (runs ~4.4M rows
-- with ~900 non-HOT updates/day, tests ~12.6M insert-only rows) they would
-- take months to trigger, so the visibility map and statistics go stale and
-- index-only scans fall back to heap fetches. Use 1% plus a small absolute
-- floor instead. Already applied by hand in production; ALTER TABLE SET is
-- idempotent so re-applying there is a no-op.
ALTER TABLE runs SET (
  autovacuum_vacuum_scale_factor = 0.01,
  autovacuum_vacuum_threshold = 1000,
  autovacuum_analyze_scale_factor = 0.01,
  autovacuum_analyze_threshold = 1000
);
ALTER TABLE tests SET (
  autovacuum_vacuum_insert_scale_factor = 0.01,
  autovacuum_vacuum_insert_threshold = 10000,
  autovacuum_analyze_scale_factor = 0.01,
  autovacuum_analyze_threshold = 10000
);
`,
		down: `
ALTER TABLE runs RESET (autovacuum_vacuum_scale_factor, autovacuum_vacuum_threshold, autovacuum_analyze_scale_factor, autovacuum_analyze_threshold);
ALTER TABLE tests RESET (autovacuum_vacuum_insert_scale_factor, autovacuum_vacuum_insert_threshold, autovacuum_analyze_scale_factor, autovacuum_analyze_threshold);
`,
	},
	{
		name: "drop indexes superseded by tests_run_id_idx and runs_package_enqueued_at_idx",
		up: `
-- runs_package_idx (package) is a strict prefix of runs_package_enqueued_at_idx
-- (package, enqueued_at DESC) INCLUDE (finished_at), which serves every
-- package = $1 predicate on runs (ScheduleRun, ListRunsForPackage).
-- runs_enqueued_at_started_at_idx was only ever picked for the old
-- ListRunsForPackage plan (backward scan + package filter), which now uses
-- the covering index. ClaimRun uses runs_finished_at_idx.
-- tests_package_idx (package) is not needed by any production query: the
-- package page (ListTestsForPackageInRange) is served by tests_expr_idx on
-- (result->'started_at'); when the planner BitmapAnd-ed tests_package_idx
-- into that plan it was 10-100x slower than the tests_expr_idx-only plan.
--
-- In production these are dropped by hand first with DROP INDEX CONCURRENTLY
-- (tern runs migrations in a transaction, which CONCURRENTLY does not allow,
-- and a plain DROP INDEX takes an ACCESS EXCLUSIVE lock on the table -- brief,
-- but it would block inserts on tests at app startup). IF EXISTS makes this
-- a no-op there; it drops the indexes on databases created by "initial".
DROP INDEX IF EXISTS runs_package_idx;
DROP INDEX IF EXISTS runs_enqueued_at_started_at_idx;
DROP INDEX IF EXISTS tests_package_idx;
`,
		down: `
CREATE INDEX IF NOT EXISTS tests_package_idx ON tests (package);
CREATE INDEX IF NOT EXISTS runs_enqueued_at_started_at_idx ON runs (enqueued_at, started_at);
CREATE INDEX IF NOT EXISTS runs_package_idx ON runs (package);
`,
	},
}
