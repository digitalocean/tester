package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/tester"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withPG(tb testing.TB, fn func(tb testing.TB, pg *PG)) {
	pgDSN := os.Getenv("PG_DSN")
	if pgDSN == "" {
		tb.Skip("PG_DSN not set, skipping PG tests. Set PG_DSN to run this test.")
	}
	conn, err := pgx.Connect(context.Background(), pgDSN)
	require.NoError(tb, err)

	defer conn.Close(context.Background())

	cfg := conn.Config()
	testDB := fmt.Sprintf("tester_%d", time.Now().UnixNano())

	_, err = conn.Exec(context.Background(), fmt.Sprintf("CREATE DATABASE %s WITH OWNER = %s", pgx.Identifier{testDB}.Sanitize(), pgx.Identifier{cfg.User}.Sanitize()))
	require.NoError(tb, err)
	defer func() {
		_, err := conn.Exec(context.Background(), fmt.Sprintf("DROP DATABASE %s", pgx.Identifier{testDB}.Sanitize()))
		require.NoError(tb, err)
	}()

	pgDSN = fmt.Sprintf("postgres://%s:%s@%s:%d/%s", cfg.User, cfg.Password, cfg.Host, cfg.Port, testDB)
	pool, err := pgxpool.New(context.Background(), pgDSN)
	require.NoError(tb, err)
	defer pool.Close()
	require.NoError(tb, pool.Ping(context.Background()))

	pg := NewPG(pool)
	err = pg.Init(context.Background())
	require.NoError(tb, err)

	tb.Log("test")
	fn(tb, pg)
}

// TestPG_Init_Indexes pins the names of the indexes that are built out of band
// with CREATE INDEX CONCURRENTLY in production; the migration declaring them
// uses IF NOT EXISTS and is only a no-op there if the names match exactly.
func TestPG_Init_Indexes(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		for _, idx := range []struct{ table, name string }{
			{"tests", "tests_run_id_idx"},
			{"runs", "runs_package_enqueued_at_idx"},
		} {
			var def string
			err := pg.pool.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE tablename = $1 AND indexname = $2", idx.table, idx.name).Scan(&def)
			require.NoError(t, err, "index %s on %s must exist after Init", idx.name, idx.table)
			tb.Log(def)
		}

		// Indexes superseded by the ones above are dropped by a later
		// migration (by hand with DROP INDEX CONCURRENTLY in production, where
		// the migration is a no-op). They are created by "initial", so a fresh
		// database exercises the DROP.
		for _, idx := range []struct{ table, name string }{
			{"runs", "runs_package_idx"},
			{"runs", "runs_enqueued_at_started_at_idx"},
			{"tests", "tests_package_idx"},
		} {
			var n int
			err := pg.pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE tablename = $1 AND indexname = $2", idx.table, idx.name).Scan(&n)
			require.NoError(t, err)
			assert.Zero(t, n, "index %s on %s must not exist after Init", idx.name, idx.table)
		}

		// Init is run at every server start; re-running it must be a no-op.
		require.NoError(t, pg.Init(ctx))
	})
}

// TestPG_Init_Autovacuum asserts the per-table autovacuum reloptions declared
// by the "tune autovacuum for runs and tests" migration are present after Init.
func TestPG_Init_Autovacuum(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		for _, tc := range []struct {
			table string
			want  []string
		}{
			{"runs", []string{
				"autovacuum_vacuum_scale_factor=0.01",
				"autovacuum_vacuum_threshold=1000",
				"autovacuum_analyze_scale_factor=0.01",
				"autovacuum_analyze_threshold=1000",
			}},
			{"tests", []string{
				"autovacuum_vacuum_insert_scale_factor=0.01",
				"autovacuum_vacuum_insert_threshold=10000",
				"autovacuum_analyze_scale_factor=0.01",
				"autovacuum_analyze_threshold=10000",
			}},
		} {
			var opts []string
			err := pg.pool.QueryRow(ctx, "SELECT coalesce(reloptions, '{}') FROM pg_class WHERE relname = $1 AND relkind = 'r'", tc.table).Scan(&opts)
			require.NoError(t, err)
			tb.Logf("%s reloptions: %v", tc.table, opts)
			assert.ElementsMatch(t, tc.want, opts, "reloptions on %s", tc.table)
		}
	})
}

func TestPG_CheckSchema(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		// withPG has already run Init: the schema is current.
		require.NoError(t, pg.CheckSchema(ctx))

		// Roll back the last migration to simulate a server started before
		// the migrate job ran; CheckSchema must fail and say why.
		conn, err := pg.pool.Acquire(ctx)
		require.NoError(t, err)
		m, err := pg.migrator(ctx, conn.Conn())
		require.NoError(t, err)
		require.NoError(t, m.MigrateTo(ctx, int32(len(pgMigrations)-1)))
		conn.Release()

		err = pg.CheckSchema(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("version %d", len(pgMigrations)-1))
		assert.Contains(t, err.Error(), fmt.Sprintf("expects %d", len(pgMigrations)))

		// Migrating brings it back.
		require.NoError(t, pg.Init(ctx))
		require.NoError(t, pg.CheckSchema(ctx))

		// A schema from a newer binary (rollback of the image) is tolerated.
		_, err = pg.pool.Exec(ctx, "UPDATE versions SET version = version + 1")
		require.NoError(t, err)
		assert.NoError(t, pg.CheckSchema(ctx))
	})
}

func TestPG_Test(t *testing.T) {
	testTime := time.Now().Truncate(time.Millisecond)

	withPG(t, func(tb testing.TB, pg *PG) {
		ctx := context.Background()

		test1 := &tester.Test{
			ID:      uuid.New(),
			Package: "pkg-1",
			RunID:   uuid.New(),

			Result: &tester.T{
				TB: tester.TB{
					StartedAt:  testTime,
					FinishedAt: testTime,
					State:      tester.TBStatePassed,
				},
				SubTs: []*tester.T{
					{
						TB: tester.TB{
							StartedAt:  testTime,
							FinishedAt: testTime,
							State:      tester.TBStatePassed,
						},
						SubTs: []*tester.T{
							{
								TB: tester.TB{
									StartedAt:  testTime,
									FinishedAt: testTime,
									State:      tester.TBStatePassed,
								},
							},
						},
					},
					{
						TB: tester.TB{
							StartedAt:  testTime,
							FinishedAt: testTime,
							State:      tester.TBStatePassed,
						},
					},
				},
			},
			Logs: []tester.TBLog{
				{Time: testTime, Name: "name", Output: []byte("output")},
			},
		}
		test2 := &tester.Test{
			ID:      uuid.New(),
			Package: "pkg-2",
			RunID:   uuid.New(),

			Result: &tester.T{
				TB: tester.TB{
					StartedAt:  testTime,
					FinishedAt: testTime,
					State:      tester.TBStatePassed,
				},
			},
			Logs: []tester.TBLog{
				{Time: testTime, Name: "name", Output: []byte("output")},
			},
		}

		t.Run("AddTest", func(t *testing.T) {
			err := pg.AddTest(ctx, test1)
			require.NoError(t, err)

			err = pg.AddTest(ctx, test2)
			require.NoError(t, err)
		})

		t.Run("GetTest", func(t *testing.T) {
			getTest, err := pg.GetTest(ctx, test1.ID)
			require.NoError(t, err)
			assert.True(
				t,
				cmp.Equal(test1, getTest),
				"expected to be equal", cmp.Diff(test1, getTest),
			)
		})

		t.Run("list", func(t *testing.T) {
			listAllTests, err := pg.ListTests(ctx, 0)
			require.NoError(t, err)
			assert.True(
				t,
				cmp.Equal([]*tester.Test{test1, test2}, listAllTests),
				"expected to be equal", cmp.Diff([]*tester.Test{test1, test2}, listAllTests),
			)

			t.Run("ListTestsForPackage", func(t *testing.T) {
				listPkgTests, err := pg.ListTestsForPackage(ctx, "pkg-2", 0)
				require.NoError(t, err)
				assert.True(
					t,
					cmp.Equal([]*tester.Test{test2}, listPkgTests),
					"expected to be equal", cmp.Diff([]*tester.Test{test2}, listPkgTests),
				)
			})

			t.Run("ListTestsForPackageInRange", func(t *testing.T) {
				listPkgTestsInRange, err := pg.ListTestsForPackageInRange(ctx, "pkg-2", testTime, testTime)
				require.NoError(t, err)
				assert.True(
					t,
					cmp.Equal([]*tester.Test{test2}, listPkgTestsInRange),
					"expected to be equal", cmp.Diff([]*tester.Test{test2}, listPkgTestsInRange),
				)
			})
		})
	})
}

// TestPG_AddTest_NoLogs pins that a test with no log lines (nil Logs, as the
// runner produces for a silent test) round-trips: pgx v5 would otherwise send
// SQL NULL for the nil slice and violate the NOT NULL constraint on logs.
func TestPG_AddTest_NoLogs(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		test := &tester.Test{
			ID:      uuid.New(),
			Package: "pkg",
			RunID:   uuid.New(),
			Result:  &tester.T{TB: tester.TB{Name: "quiet", State: tester.TBStatePassed}},
		}
		require.NoError(t, pg.AddTest(ctx, test))

		got, err := pg.GetTest(ctx, test.ID)
		require.NoError(t, err)
		assert.Nil(t, got.Logs)
		assert.Equal(t, test.Result, got.Result)
	})
}

func TestPG_EnqueueRun_GetRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		run := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
			Args:    []string{"one", "two"},
		}

		err := pg.EnqueueRun(ctx, run)
		require.NoError(t, err)

		run, err = pg.GetRun(ctx, run.ID)
		require.NoError(t, err)
		assert.NotEmpty(t, run.EnqueuedAt)
	})
}

func TestPG_StartRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		run := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
			Args:    []string{"one", "two"},
		}

		err := pg.EnqueueRun(ctx, run)
		require.NoError(t, err)

		err = pg.StartRun(ctx, run.ID, "runner")
		require.NoError(t, err)

		getRun, err := pg.GetRun(ctx, run.ID)
		require.NoError(t, err)
		assert.NotEmpty(t, getRun.StartedAt)
		assert.Equal(t, "runner", getRun.Meta.Runner)
	})
}

func TestPG_ResetRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		run := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
			Args:    []string{"one", "two"},
		}

		err := pg.EnqueueRun(ctx, run)
		require.NoError(t, err)

		err = pg.StartRun(ctx, run.ID, "runner")
		require.NoError(t, err)

		before, err := pg.GetRun(ctx, run.ID)
		require.NoError(t, err)

		pg.now = func() time.Time { return before.EnqueuedAt.Add(time.Minute) }
		err = pg.ResetRun(ctx, run.ID)
		require.NoError(t, err)

		getRun, err := pg.GetRun(ctx, run.ID)
		require.NoError(t, err)
		assert.Empty(t, getRun.StartedAt)
		assert.Equal(t, "", getRun.Meta.Runner)
		// A reset run goes to the back of the queue, not the front.
		assert.True(t, getRun.EnqueuedAt.After(before.EnqueuedAt), "enqueued_at %s should be after %s", getRun.EnqueuedAt, before.EnqueuedAt)
		assert.Equal(t, 1, getRun.ResetCount)

		err = pg.ResetRun(ctx, run.ID)
		require.NoError(t, err)
		getRun, err = pg.GetRun(ctx, run.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, getRun.ResetCount)
	})
}

func TestPG_ScheduleRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		t0 := time.Now().UTC().Truncate(time.Millisecond)
		pg.now = func() time.Time { return t0 }
		newRun := func() *tester.Run {
			return &tester.Run{ID: uuid.New(), Package: "pkg", EnqueuedAt: pg.now()}
		}

		scheduled, err := pg.ScheduleRun(ctx, newRun(), 10*time.Minute)
		require.NoError(t, err)
		assert.True(t, scheduled, "first schedule for a package must succeed")

		scheduled, err = pg.ScheduleRun(ctx, newRun(), 10*time.Minute)
		require.NoError(t, err)
		assert.False(t, scheduled, "must not schedule while a run for the package is pending")

		pending, err := pg.ListPendingRuns(ctx)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		first := pending[0]

		// Finish the pending run; still inside the min interval -> no schedule.
		err = pg.StartRun(ctx, first.ID, "runner")
		require.NoError(t, err)
		err = pg.CompleteRun(ctx, first.ID)
		require.NoError(t, err)

		pg.now = func() time.Time { return t0.Add(5 * time.Minute) }
		scheduled, err = pg.ScheduleRun(ctx, newRun(), 10*time.Minute)
		require.NoError(t, err)
		assert.False(t, scheduled, "must not schedule within min interval of the last enqueue")

		// Past the min interval -> schedules.
		pg.now = func() time.Time { return t0.Add(11 * time.Minute) }
		scheduled, err = pg.ScheduleRun(ctx, newRun(), 10*time.Minute)
		require.NoError(t, err)
		assert.True(t, scheduled, "must schedule once min interval has elapsed")

		// Another package is independent.
		scheduled, err = pg.ScheduleRun(ctx, &tester.Run{ID: uuid.New(), Package: "other", EnqueuedAt: pg.now()}, 10*time.Minute)
		require.NoError(t, err)
		assert.True(t, scheduled)
	})
}

func TestPG_ScheduleRun_Concurrent(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		// Simulates N web replicas all deciding to schedule the same package
		// at the same instant: exactly one must win.
		const replicas = 8
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			wins int
			errs []error
		)
		for i := 0; i < replicas; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				scheduled, err := pg.ScheduleRun(ctx, &tester.Run{ID: uuid.New(), Package: "pkg", EnqueuedAt: time.Now()}, time.Minute)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				if scheduled {
					wins++
				}
			}()
		}
		wg.Wait()
		require.Empty(t, errs)
		assert.Equal(t, 1, wins)

		pending, err := pg.ListPendingRuns(ctx)
		require.NoError(t, err)
		assert.Len(t, pending, 1)
	})
}

func TestPG_ClaimRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		t0 := time.Now().UTC().Truncate(time.Millisecond)
		enqueue := func(pkg string, at time.Time) *tester.Run {
			run := &tester.Run{ID: uuid.New(), Package: pkg, EnqueuedAt: at}
			require.NoError(t, pg.EnqueueRun(ctx, run))
			return run
		}
		b := enqueue("b", t0)
		a := enqueue("a", t0.Add(time.Second))
		c := enqueue("c", t0.Add(2*time.Second))

		// Oldest claimable run wins, exclusions apply, runner is recorded.
		got, err := pg.ClaimRun(ctx, "runner-1", []string{"a", "b", "c"}, []string{"b"})
		require.NoError(t, err)
		assert.Equal(t, a.ID, got.ID)
		assert.Equal(t, "runner-1", got.Meta.Runner)
		assert.False(t, got.StartedAt.IsZero())

		// Already-started runs are not claimable again.
		got, err = pg.ClaimRun(ctx, "runner-2", []string{"a", "c"}, nil)
		require.NoError(t, err)
		assert.Equal(t, c.ID, got.ID)

		// Nothing left for these packages.
		_, err = pg.ClaimRun(ctx, "runner-2", []string{"a", "c"}, nil)
		assert.Equal(t, ErrNotFound, err)

		// b is still there for a runner that accepts it.
		got, err = pg.ClaimRun(ctx, "runner-3", []string{"b"}, nil)
		require.NoError(t, err)
		assert.Equal(t, b.ID, got.ID)

		// Finished runs are never claimable.
		require.NoError(t, pg.CompleteRun(ctx, b.ID))
		_, err = pg.ClaimRun(ctx, "runner-3", []string{"a", "b", "c"}, nil)
		assert.Equal(t, ErrNotFound, err)
	})
}

func TestPG_ClaimRun_Concurrent(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		const (
			runs    = 40
			workers = 8
		)
		for i := 0; i < runs; i++ {
			require.NoError(t, pg.EnqueueRun(ctx, &tester.Run{ID: uuid.New(), Package: "pkg", EnqueuedAt: time.Now()}))
		}

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			claimed []uuid.UUID
			errs    []error
		)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for {
					run, err := pg.ClaimRun(ctx, fmt.Sprintf("worker-%d", w), []string{"pkg"}, nil)
					if err == ErrNotFound {
						return
					}
					mu.Lock()
					if err != nil {
						errs = append(errs, err)
						mu.Unlock()
						return
					}
					claimed = append(claimed, run.ID)
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		require.Empty(t, errs)

		seen := make(map[uuid.UUID]struct{}, len(claimed))
		for _, id := range claimed {
			_, dup := seen[id]
			assert.False(t, dup, "run %s claimed twice", id)
			seen[id] = struct{}{}
		}
		assert.Len(t, seen, runs, "every run must be claimed exactly once")
	})
}

func TestPG_DeleteRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		run := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
			Args:    []string{"one", "two"},
		}

		err := pg.EnqueueRun(ctx, run)
		require.NoError(t, err)

		err = pg.DeleteRun(ctx, run.ID)
		require.NoError(t, err)

		_, err = pg.GetRun(ctx, run.ID)
		require.Error(t, err)
		assert.Equal(t, ErrNotFound, err)
	})
}

func TestPG_CompleteRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		run := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
			Args:    []string{"one", "two"},
		}

		err := pg.EnqueueRun(ctx, run)
		require.NoError(t, err)

		err = pg.StartRun(ctx, run.ID, "")
		require.NoError(t, err)

		err = pg.CompleteRun(ctx, run.ID)
		require.NoError(t, err)

		getRun, err := pg.GetRun(ctx, run.ID)
		require.NoError(t, err)
		assert.NotEmpty(t, getRun.FinishedAt)
	})
}

func TestPG_FailRun(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		run := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
			Args:    []string{"one", "two"},
		}

		err := pg.EnqueueRun(ctx, run)
		require.NoError(t, err)

		err = pg.StartRun(ctx, run.ID, "")
		require.NoError(t, err)

		err = pg.FailRun(ctx, run.ID, "error")
		require.NoError(t, err)

		getRun, err := pg.GetRun(ctx, run.ID)
		require.NoError(t, err)
		assert.NotEmpty(t, getRun.FinishedAt)
		assert.NotEmpty(t, getRun.Error)
	})
}

func TestPG_ListRuns(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		runPending := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
		}

		runComplete := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
		}

		runFail := &tester.Run{
			ID:      uuid.New(),
			Package: "pkg",
		}

		for _, r := range []*tester.Run{runPending, runComplete, runFail} {
			err := pg.EnqueueRun(ctx, r)
			require.NoError(t, err)

			err = pg.StartRun(ctx, r.ID, "")
			require.NoError(t, err)
		}

		runPending, err := pg.GetRun(ctx, runPending.ID)
		require.NoError(t, err)

		err = pg.CompleteRun(ctx, runComplete.ID)
		require.NoError(t, err)
		runComplete, err = pg.GetRun(ctx, runComplete.ID)
		require.NoError(t, err)

		err = pg.FailRun(ctx, runFail.ID, "error")
		require.NoError(t, err)
		runFail, err = pg.GetRun(ctx, runFail.ID)
		require.NoError(t, err)

		t.Run("ListPendingRuns", func(t *testing.T) {
			runs, err := pg.ListPendingRuns(ctx)
			require.NoError(t, err)
			assert.ElementsMatch(t, []*tester.Run{runPending}, runs)
		})

		t.Run("ListPendingRuns", func(t *testing.T) {
			runs, err := pg.ListFinishedRuns(ctx, 0)
			require.NoError(t, err)
			assert.ElementsMatch(t, []*tester.Run{runComplete, runFail}, runs)
		})
	})
}

func TestPG_ListRunsForPackage(t *testing.T) {
	ctx := context.Background()

	withPG(t, func(tb testing.TB, pg *PG) {
		runs := []*tester.Run{
			{
				ID:      uuid.New(),
				Package: "pkg-1",
			},
			{
				ID:      uuid.New(),
				Package: "pkg-2",
			},
		}

		for _, r := range runs {
			err := pg.EnqueueRun(ctx, r)
			require.NoError(t, err)
			r, err = pg.GetRun(ctx, r.ID)
			require.NoError(t, err)
		}

		runs, err := pg.ListRunsForPackage(ctx, "pkg-1", 0)
		require.NoError(t, err)
		assert.ElementsMatch(t, []*tester.Run{runs[0]}, runs)
	})
}

func TestPG_ListRunSummariesInRange(t *testing.T) {
	ctx := context.Background()

	t.Run("creates empty buckets", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			now := time.Now().UTC()
			summaries, err := pg.ListRunSummariesInRange(ctx, now, now.Add(3*time.Minute+15*time.Second), time.Minute)
			require.NoError(t, err)
			assert.Len(t, summaries, 4)
			for i, summary := range summaries {
				assert.Equal(t, now.Add(time.Duration(i)*time.Minute), summary.Time)
				assert.Equal(t, time.Minute, summary.Duration)
			}
		})
	})

	t.Run("places runs in correct buckets", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			begin := time.Now().UTC()
			end := begin.Add(3 * time.Minute).UTC()
			window := time.Minute

			pkg1run1 := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg-1",
				EnqueuedAt: begin,
				StartedAt:  begin,
				FinishedAt: begin,
			}
			err := pg.EnqueueRun(ctx, pkg1run1)
			require.NoError(t, err)

			pkg1run1.Tests = []*tester.Test{
				{
					ID:      uuid.New(),
					RunID:   pkg1run1.ID,
					Package: pkg1run1.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed},
					},
				},
				{
					ID:      uuid.New(),
					RunID:   pkg1run1.ID,
					Package: pkg1run1.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-fail", State: tester.TBStateFailed},
					},
				},
				{
					ID:      uuid.New(),
					RunID:   pkg1run1.ID,
					Package: pkg1run1.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-skip", State: tester.TBStateSkipped},
					},
				},
			}

			pkg1run2 := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg-1",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(15 * time.Second),
				FinishedAt: begin,
			}
			err = pg.EnqueueRun(ctx, pkg1run2)
			require.NoError(t, err)

			pkg1run2.Tests = []*tester.Test{
				{
					ID:      uuid.New(),
					RunID:   pkg1run2.ID,
					Package: pkg1run2.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed},
					},
				},
				{
					ID:      uuid.New(),
					RunID:   pkg1run2.ID,
					Package: pkg1run2.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-fail", State: tester.TBStateFailed},
					},
				},
				{
					ID:      uuid.New(),
					RunID:   pkg1run2.ID,
					Package: pkg1run2.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-skip", State: tester.TBStateSkipped},
					},
				},
			}

			pkg1run3 := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg-1",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(2*time.Minute + 15*time.Second),
				FinishedAt: begin.Add(2*time.Minute + 15*time.Second),
			}
			err = pg.EnqueueRun(ctx, pkg1run3)
			require.NoError(t, err)

			pkg1run3.Tests = []*tester.Test{
				{
					ID:      uuid.New(),
					RunID:   pkg1run3.ID,
					Package: pkg1run3.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed},
					},
				},
				{
					ID:      uuid.New(),
					RunID:   pkg1run3.ID,
					Package: pkg1run3.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-fail", State: tester.TBStateFailed},
					},
				},
				{
					ID:      uuid.New(),
					RunID:   pkg1run3.ID,
					Package: pkg1run3.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-skip", State: tester.TBStateSkipped},
					},
				},
			}

			pkg2run1 := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg-2",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(2*time.Minute + 15*time.Second),
				FinishedAt: begin.Add(2*time.Minute + 15*time.Second),
			}
			err = pg.EnqueueRun(ctx, pkg2run1)
			require.NoError(t, err)

			pkg2run1.Tests = []*tester.Test{
				{
					ID:      uuid.New(),
					RunID:   pkg2run1.ID,
					Package: pkg2run1.Package,
					Result: &tester.T{
						TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed},
					},
				},
			}

			pkg2run2 := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg-2",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(2*time.Minute + 15*time.Second),
				FinishedAt: begin.Add(2*time.Minute + 15*time.Second),
				Error:      "failed",
			}
			err = pg.EnqueueRun(ctx, pkg2run2)
			require.NoError(t, err)

			allTests := append(pkg1run1.Tests, pkg1run2.Tests...)
			allTests = append(allTests, pkg1run3.Tests...)
			allTests = append(allTests, pkg2run1.Tests...)
			for _, test := range allTests {
				err := pg.AddTest(ctx, test)
				require.NoError(t, err)
			}

			summaries, err := pg.ListRunSummariesInRange(ctx, begin, end, window)
			require.NoError(t, err)
			assert.Len(t, summaries, 3)
			assert.Equal(t, &tester.RunSummary{
				Time:     begin,
				Duration: window,
				PackageSummary: map[string]*tester.PackageSummary{
					"pkg-1": {
						Package:      "pkg-1",
						RunIDs:       []uuid.UUID{pkg1run1.ID, pkg1run2.ID},
						ErrorRunIDs:  nil,
						PassedTests:  map[string][]uuid.UUID{"test-pass": {pkg1run1.Tests[0].ID, pkg1run2.Tests[0].ID}},
						FailedTests:  map[string][]uuid.UUID{"test-fail": {pkg1run1.Tests[1].ID, pkg1run2.Tests[1].ID}},
						SkippedTests: map[string][]uuid.UUID{"test-skip": {pkg1run1.Tests[2].ID, pkg1run2.Tests[2].ID}},
					},
				},
			}, summaries[0])

			// pkg2run2 errored without submitting any tests; it must still
			// appear in its bucket (the join to tests is a LEFT JOIN).
			assert.Equal(t, &tester.PackageSummary{
				Package:      "pkg-2",
				RunIDs:       []uuid.UUID{pkg2run1.ID},
				ErrorRunIDs:  []uuid.UUID{pkg2run2.ID},
				PassedTests:  map[string][]uuid.UUID{"test-pass": {pkg2run1.Tests[0].ID}},
				FailedTests:  map[string][]uuid.UUID{},
				SkippedTests: map[string][]uuid.UUID{},
			}, summaries[2].PackageSummary["pkg-2"])
		})
	})

	t.Run("errored run with no tests appears in ErrorRunIDs", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			begin := time.Now().UTC()
			end := begin.Add(2 * time.Minute)
			window := time.Minute

			run := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(10 * time.Second),
				FinishedAt: begin.Add(20 * time.Second),
				Error:      "killed",
			}
			require.NoError(t, pg.EnqueueRun(ctx, run))

			summaries, err := pg.ListRunSummariesInRange(ctx, begin, end, window)
			require.NoError(t, err)
			require.Len(t, summaries, 2)
			assert.Equal(t, &tester.PackageSummary{
				Package:      "pkg",
				ErrorRunIDs:  []uuid.UUID{run.ID},
				PassedTests:  map[string][]uuid.UUID{},
				FailedTests:  map[string][]uuid.UUID{},
				SkippedTests: map[string][]uuid.UUID{},
			}, summaries[0].PackageSummary["pkg"])
			assert.Empty(t, summaries[1].PackageSummary)
		})
	})

	t.Run("errored run's tests are not counted", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			begin := time.Now().UTC()
			end := begin.Add(time.Minute)
			window := time.Minute

			run := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin,
				StartedAt:  begin,
				FinishedAt: begin.Add(10 * time.Second),
				Error:      "exit status 2",
			}
			require.NoError(t, pg.EnqueueRun(ctx, run))
			require.NoError(t, pg.AddTest(ctx, &tester.Test{
				ID: uuid.New(), RunID: run.ID, Package: run.Package,
				Result: &tester.T{TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed}},
			}))

			summaries, err := pg.ListRunSummariesInRange(ctx, begin, end, window)
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			ps := summaries[0].PackageSummary["pkg"]
			require.NotNil(t, ps)
			assert.Equal(t, []uuid.UUID{run.ID}, ps.ErrorRunIDs)
			assert.Empty(t, ps.RunIDs)
			assert.Equal(t, 0, ps.NumTotalTests())
		})
	})

	t.Run("running run spans buckets and its tests are counted once", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			begin := time.Now().UTC()
			end := begin.Add(4 * time.Minute)
			window := time.Minute

			// "now" is 2m30s in: the run has been executing through buckets
			// 0, 1 and 2 and must not appear in bucket 3.
			pg.now = func() time.Time { return begin.Add(2*time.Minute + 30*time.Second) }

			run := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(30 * time.Second),
			}
			require.NoError(t, pg.EnqueueRun(ctx, run))
			test := &tester.Test{
				ID: uuid.New(), RunID: run.ID, Package: run.Package,
				Result: &tester.T{TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed}},
			}
			require.NoError(t, pg.AddTest(ctx, test))

			summaries, err := pg.ListRunSummariesInRange(ctx, begin, end, window)
			require.NoError(t, err)
			require.Len(t, summaries, 4)

			assert.Equal(t, &tester.PackageSummary{
				Package:       "pkg",
				RunIDs:        []uuid.UUID{run.ID},
				RunningRunIDs: []uuid.UUID{run.ID},
				PassedTests:   map[string][]uuid.UUID{"test-pass": {test.ID}},
				FailedTests:   map[string][]uuid.UUID{},
				SkippedTests:  map[string][]uuid.UUID{},
			}, summaries[0].PackageSummary["pkg"], "start bucket has the run and its tests")

			for _, i := range []int{1, 2} {
				assert.Equal(t, &tester.PackageSummary{
					Package:       "pkg",
					RunIDs:        []uuid.UUID{run.ID},
					RunningRunIDs: []uuid.UUID{run.ID},
					PassedTests:   map[string][]uuid.UUID{},
					FailedTests:   map[string][]uuid.UUID{},
					SkippedTests:  map[string][]uuid.UUID{},
				}, summaries[i].PackageSummary["pkg"], "bucket %d has the run but not its tests", i)
			}
			assert.Empty(t, summaries[3].PackageSummary, "bucket after now is empty")

			total := 0
			for _, s := range summaries {
				total += s.NumTotalTests()
			}
			assert.Equal(t, 1, total, "tests are counted exactly once across buckets")
			assert.Equal(t, 1, summaries[1].NumRunningRuns())
		})
	})

	t.Run("finished run spans the buckets it executed in", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			begin := time.Now().UTC()
			end := begin.Add(4 * time.Minute)
			window := time.Minute

			// Finishes exactly on the bucket 2 boundary: [started, finished)
			// is half-open so it must not be attributed to bucket 2.
			run := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin,
				StartedAt:  begin.Add(30 * time.Second),
				FinishedAt: begin.Add(2 * time.Minute),
			}
			require.NoError(t, pg.EnqueueRun(ctx, run))
			test := &tester.Test{
				ID: uuid.New(), RunID: run.ID, Package: run.Package,
				Result: &tester.T{TB: tester.TB{Name: "test-fail", State: tester.TBStateFailed}},
			}
			require.NoError(t, pg.AddTest(ctx, test))

			summaries, err := pg.ListRunSummariesInRange(ctx, begin, end, window)
			require.NoError(t, err)
			require.Len(t, summaries, 4)

			assert.Equal(t, []uuid.UUID{run.ID}, summaries[0].PackageSummary["pkg"].RunIDs)
			assert.Equal(t, 1, summaries[0].NumFailedTests())
			assert.Equal(t, []uuid.UUID{run.ID}, summaries[1].PackageSummary["pkg"].RunIDs)
			assert.Empty(t, summaries[1].PackageSummary["pkg"].RunningRunIDs)
			assert.Equal(t, 0, summaries[1].NumTotalTests())
			assert.Empty(t, summaries[2].PackageSummary)
			assert.Empty(t, summaries[3].PackageSummary)
		})
	})

	t.Run("run that started before the range is attributed but its tests are not", func(t *testing.T) {
		withPG(t, func(tb testing.TB, pg *PG) {
			begin := time.Now().UTC()
			end := begin.Add(2 * time.Minute)
			window := time.Minute

			// Started 30s before the range and finished 30s into it.
			overlapping := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin.Add(-time.Minute),
				StartedAt:  begin.Add(-30 * time.Second),
				FinishedAt: begin.Add(30 * time.Second),
			}
			require.NoError(t, pg.EnqueueRun(ctx, overlapping))
			require.NoError(t, pg.AddTest(ctx, &tester.Test{
				ID: uuid.New(), RunID: overlapping.ID, Package: overlapping.Package,
				Result: &tester.T{TB: tester.TB{Name: "test-pass", State: tester.TBStatePassed}},
			}))

			// Finished before the range: must not appear at all.
			earlier := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin.Add(-time.Minute),
				StartedAt:  begin.Add(-40 * time.Second),
				FinishedAt: begin.Add(-10 * time.Second),
			}
			require.NoError(t, pg.EnqueueRun(ctx, earlier))

			// Started exactly at end: the range is half-open, must not appear.
			atEnd := &tester.Run{
				ID:         uuid.New(),
				Package:    "pkg",
				EnqueuedAt: begin,
				StartedAt:  end,
				FinishedAt: end.Add(time.Second),
			}
			require.NoError(t, pg.EnqueueRun(ctx, atEnd))

			summaries, err := pg.ListRunSummariesInRange(ctx, begin, end, window)
			require.NoError(t, err)
			require.Len(t, summaries, 2)
			assert.Equal(t, &tester.PackageSummary{
				Package:      "pkg",
				RunIDs:       []uuid.UUID{overlapping.ID},
				PassedTests:  map[string][]uuid.UUID{},
				FailedTests:  map[string][]uuid.UUID{},
				SkippedTests: map[string][]uuid.UUID{},
			}, summaries[0].PackageSummary["pkg"])
			assert.Empty(t, summaries[1].PackageSummary)
		})
	})
}

func TestBucketIndex(t *testing.T) {
	for _, tc := range []struct {
		offset time.Duration
		want   int
	}{
		{0, 0},
		{59 * time.Second, 0},
		{time.Minute, 1},
		{90 * time.Second, 1},
		{-time.Nanosecond, -1},
		{-30 * time.Second, -1},
		{-time.Minute, -1},
		{-61 * time.Second, -2},
	} {
		assert.Equal(t, tc.want, bucketIndex(tc.offset, time.Minute), "offset %s", tc.offset)
	}
}
