package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitalocean/tester"
	"github.com/digitalocean/tester/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestNewScheduler_Options(t *testing.T) {
	s := NewScheduler(nil, nil)
	assert.Equal(t, DefaultRunDelay, s.RunDelay())
	assert.Equal(t, DefaultRunTimeout, s.RunTimeout())
	assert.Equal(t, DefaultMaxResets, s.MaxResets())

	s = NewScheduler(nil, nil, WithRunDelay(time.Minute), WithRunTimeout(45*time.Minute), WithMaxResets(0))
	assert.Equal(t, time.Minute, s.RunDelay())
	assert.Equal(t, 45*time.Minute, s.RunTimeout())
	assert.Equal(t, 0, s.MaxResets())
}

func TestScheduleRuns(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockDB := db.NewMockDB(ctrl)

	pkgs := []*tester.Package{
		{Name: "fast", RunDelay: 2 * time.Minute, Options: []tester.Option{{Name: "test.timeout", Default: "5m"}}},
		{Name: "slow"},
	}
	s := NewScheduler(mockDB, pkgs, WithRunDelay(10*time.Minute))

	// Each package is offered to the DB exactly once per tick, with the
	// package's own run_delay when set and the scheduler default otherwise.
	// The scheduler never consults ListPendingRuns or keeps local state.
	mockDB.EXPECT().
		ScheduleRun(gomock.Any(), gomock.AssignableToTypeOf(&tester.Run{}), 2*time.Minute).
		DoAndReturn(func(_ context.Context, run *tester.Run, _ time.Duration) (bool, error) {
			assert.Equal(t, "fast", run.Package)
			assert.Equal(t, []string{"-test.timeout=5m"}, run.Args)
			assert.NotEqual(t, uuid.Nil, run.ID)
			assert.False(t, run.EnqueuedAt.IsZero())
			return true, nil
		})
	mockDB.EXPECT().
		ScheduleRun(gomock.Any(), gomock.AssignableToTypeOf(&tester.Run{}), 10*time.Minute).
		DoAndReturn(func(_ context.Context, run *tester.Run, _ time.Duration) (bool, error) {
			assert.Equal(t, "slow", run.Package)
			assert.Empty(t, run.Args)
			return false, nil
		})

	require.NoError(t, s.scheduleRuns(context.Background()))
}

func TestScheduleRuns_ErrorsAreAggregatedNotFatal(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockDB := db.NewMockDB(ctrl)

	pkgs := []*tester.Package{{Name: "a"}, {Name: "b"}}
	s := NewScheduler(mockDB, pkgs)

	boom := errors.New("boom")
	// Both packages are attempted even though one fails.
	mockDB.EXPECT().ScheduleRun(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, boom)
	mockDB.EXPECT().ScheduleRun(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)

	err := s.scheduleRuns(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

func TestResetStaleRuns(t *testing.T) {
	now := time.Now()
	stale := now.Add(-30 * time.Minute)

	runs := []*tester.Run{
		// Not started: ignored.
		{ID: uuid.New(), Package: "queued"},
		// Started recently: ignored.
		{ID: uuid.New(), Package: "fresh", StartedAt: now.Add(-time.Minute)},
		// Stale, under the reset limit: reset.
		{ID: uuid.New(), Package: "stale-first", StartedAt: stale, ResetCount: 0},
		{ID: uuid.New(), Package: "stale-second", StartedAt: stale, ResetCount: 1},
		// Stale, at the reset limit: failed, not reset.
		{ID: uuid.New(), Package: "stale-exhausted", StartedAt: stale, ResetCount: 2, Meta: tester.RunMeta{Runner: "worker-3"}},
	}

	ctrl := gomock.NewController(t)
	mockDB := db.NewMockDB(ctrl)
	s := NewScheduler(mockDB, nil, WithRunTimeout(15*time.Minute), WithMaxResets(2))

	mockDB.EXPECT().ListPendingRuns(gomock.Any()).Return(runs, nil)
	mockDB.EXPECT().ResetRun(gomock.Any(), runs[2].ID).Return(nil)
	mockDB.EXPECT().ResetRun(gomock.Any(), runs[3].ID).Return(nil)
	mockDB.EXPECT().
		FailRun(gomock.Any(), runs[4].ID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _ uuid.UUID, msg string) error {
			assert.Contains(t, msg, "15m0s")
			assert.Contains(t, msg, "3 time(s)")
			assert.Contains(t, msg, `"worker-3"`)
			return nil
		})

	require.NoError(t, s.resetStaleRuns(context.Background()))
}

func TestResetStaleRuns_MaxResetsZeroFailsImmediately(t *testing.T) {
	run := &tester.Run{ID: uuid.New(), Package: "p", StartedAt: time.Now().Add(-time.Hour)}

	ctrl := gomock.NewController(t)
	mockDB := db.NewMockDB(ctrl)
	s := NewScheduler(mockDB, nil, WithMaxResets(0))

	mockDB.EXPECT().ListPendingRuns(gomock.Any()).Return([]*tester.Run{run}, nil)
	mockDB.EXPECT().FailRun(gomock.Any(), run.ID, gomock.Any()).Return(nil)

	require.NoError(t, s.resetStaleRuns(context.Background()))
}

func TestResetStaleRuns_NotFoundIsTolerated(t *testing.T) {
	// Another replica may have reset or completed the run between our list
	// and our reset; that is not an error.
	run := &tester.Run{ID: uuid.New(), Package: "p", StartedAt: time.Now().Add(-time.Hour)}

	ctrl := gomock.NewController(t)
	mockDB := db.NewMockDB(ctrl)
	s := NewScheduler(mockDB, nil)

	mockDB.EXPECT().ListPendingRuns(gomock.Any()).Return([]*tester.Run{run}, nil)
	mockDB.EXPECT().ResetRun(gomock.Any(), run.ID).Return(db.ErrNotFound)

	require.NoError(t, s.resetStaleRuns(context.Background()))
}
