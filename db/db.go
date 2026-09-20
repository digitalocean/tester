package db

import (
	"context"
	"errors"
	"time"

	"github.com/digitalocean/tester"
	"github.com/google/uuid"
)

// ErrNotFound is returned when the requested item could not be found.
var ErrNotFound = errors.New("not found")

//go:generate go run go.uber.org/mock/mockgen@v0.6.0 -package=db -destination=db_mock.go github.com/digitalocean/tester/db DB

// DB is the interface for a persistence store implementation.
type DB interface {
	Init(ctx context.Context) error

	AddTest(ctx context.Context, test *tester.Test) error
	GetTest(ctx context.Context, id uuid.UUID) (*tester.Test, error)
	ListTests(ctx context.Context, limit int) ([]*tester.Test, error)
	ListTestsForPackage(ctx context.Context, pkg string, limit int) ([]*tester.Test, error)
	ListTestsForPackageInRange(ctx context.Context, pkg string, begin, end time.Time) ([]*tester.Test, error)

	// EnqueueRun unconditionally adds a run to the queue (manual triggers).
	EnqueueRun(ctx context.Context, run *tester.Run) error
	// ScheduleRun adds a run only if the package has no unfinished run and
	// its last run was enqueued at least minInterval ago. Safe to call from
	// any number of server replicas concurrently.
	ScheduleRun(ctx context.Context, run *tester.Run, minInterval time.Duration) (scheduled bool, err error)
	// ClaimRun atomically assigns the oldest claimable run to runner, or
	// returns ErrNotFound. Concurrent claimers never receive the same run.
	ClaimRun(ctx context.Context, runner string, include, exclude []string) (*tester.Run, error)
	// ResetRun returns a timed-out run to the back of the queue and
	// increments its reset count.
	ResetRun(ctx context.Context, id uuid.UUID) error
	DeleteRun(ctx context.Context, id uuid.UUID) error
	CompleteRun(ctx context.Context, id uuid.UUID) error
	FailRun(ctx context.Context, id uuid.UUID, errorMessage string) error
	GetRun(ctx context.Context, id uuid.UUID) (*tester.Run, error)
	ListPendingRuns(ctx context.Context) ([]*tester.Run, error)
	ListFinishedRuns(ctx context.Context, limit int) ([]*tester.Run, error)
	ListRunsForPackage(ctx context.Context, pkg string, limit int) ([]*tester.Run, error)
	ListRunSummariesInRange(ctx context.Context, begin, end time.Time, window time.Duration) ([]*tester.RunSummary, error)
}
