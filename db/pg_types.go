package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/digitalocean/tester"
	"github.com/jackc/pgx/v5"
)

// jsonb marshals v for a jsonb NOT NULL column. pgx v5 encodes a nil slice or
// pointer as SQL NULL (v4 sent JSON null), which would violate the NOT NULL
// constraint on tests.result, tests.logs and runs.meta for e.g. a test that
// produced no log lines. Pre-marshalling keeps the v4 behaviour.
func jsonb(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// tester.T, []tester.TBLog and tester.RunMeta are plain structs of
		// strings, times and bytes; failing to marshal them is a programming
		// error, not a runtime condition.
		panic(fmt.Sprintf("db: marshalling %T for jsonb column: %s", v, err))
	}
	return b
}

type pgTest tester.Test

func (t *pgTest) Columns() []string {
	return []string{
		"id",
		"package",
		"run_id",
		"result",
		"logs",
	}
}

func (t *pgTest) Values() []interface{} {
	return []interface{}{
		t.ID,
		t.Package,
		t.RunID,
		jsonb(t.Result),
		jsonb(t.Logs),
	}
}

func (t *pgTest) Scan(row pgx.Row) error {
	err := row.Scan(
		&t.ID,
		&t.Package,
		&t.RunID,
		&t.Result,
		&t.Logs,
	)
	if err != nil && err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return err
}

type pgRun tester.Run

func (r *pgRun) Columns() []string {
	return []string{
		"id",
		"package",
		"args",
		"meta",
		"enqueued_at",
		"started_at",
		"finished_at",
		"error",
		"reset_count",
	}
}

// qualifiedColumns returns Columns() prefixed with the runs table name, for
// statements that join or use UPDATE ... FROM.
func (r *pgRun) qualifiedColumns() []string {
	cols := r.Columns()
	for i, c := range cols {
		cols[i] = "runs." + c
	}
	return cols
}

func (r *pgRun) Values() []interface{} {
	startedAt := sql.NullTime{Valid: !r.StartedAt.IsZero(), Time: r.StartedAt}
	finishedAt := sql.NullTime{Valid: !r.FinishedAt.IsZero(), Time: r.FinishedAt}
	error := sql.NullString{Valid: r.Error != "", String: r.Error}

	return []interface{}{
		r.ID,
		r.Package,
		r.Args,
		jsonb(r.Meta),
		r.EnqueuedAt,
		startedAt,
		finishedAt,
		error,
		r.ResetCount,
	}
}

func (r *pgRun) Scan(row pgx.Row) error {
	var (
		startedAt  sql.NullTime
		finishedAt sql.NullTime
		error      sql.NullString
	)

	err := row.Scan(
		&r.ID,
		&r.Package,
		&r.Args,
		&r.Meta,
		&r.EnqueuedAt,
		&startedAt,
		&finishedAt,
		&error,
		&r.ResetCount,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			err = ErrNotFound
		}
		return err
	}

	if startedAt.Valid {
		r.StartedAt = startedAt.Time
	}
	if finishedAt.Valid {
		r.FinishedAt = finishedAt.Time
	}
	if error.Valid {
		r.Error = error.String
	}
	return nil
}
