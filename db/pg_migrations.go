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
}
