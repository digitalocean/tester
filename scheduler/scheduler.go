package scheduler

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/digitalocean/tester"
	"github.com/digitalocean/tester/db"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// Option is used to inject dependencies into a Scheduler on creation.
type Option func(*Scheduler)

// WithRunDelay allows configuring a minimum delay between runs of a package.
func WithRunDelay(d time.Duration) Option {
	return func(s *Scheduler) {
		s.runDelay = d
	}
}

// WithRunTimeout allows configuring a maximum timeout before runs are deemed
// stale and reset.
func WithRunTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		s.runTimeout = d
	}
}

// WithMaxResets allows configuring how many times a run may be reset after
// exceeding the run timeout before it is failed instead. 0 fails on the first
// timeout.
func WithMaxResets(n int) Option {
	return func(s *Scheduler) {
		s.maxResets = n
	}
}

// DefaultRunDelay is the minimum interval between scheduled runs of a package
// when neither the package nor the scheduler configures one.
const DefaultRunDelay = 5 * time.Minute

// DefaultRunTimeout is how long a claimed run may go without finishing before
// the scheduler resets it.
const DefaultRunTimeout = 15 * time.Minute

// DefaultMaxResets is how many times a run is reset for exceeding the run
// timeout before it is failed.
const DefaultMaxResets = 2

// Scheduler schedules runs.
//
// The scheduler keeps no state between ticks: whether a package is due is
// decided by the database (see db.DB.ScheduleRun), so any number of server
// replicas may run a scheduler concurrently without double-enqueuing.
type Scheduler struct {
	Packages map[string]*tester.Package

	stop       chan struct{}
	runDelay   time.Duration
	runTimeout time.Duration
	maxResets  int
	db         db.DB
}

// NewScheduler constructs a new scheduler.
func NewScheduler(db db.DB, packages []*tester.Package, opts ...Option) *Scheduler {
	scheduler := &Scheduler{
		db:         db,
		Packages:   make(map[string]*tester.Package),
		stop:       make(chan struct{}),
		runDelay:   DefaultRunDelay,
		runTimeout: DefaultRunTimeout,
		maxResets:  DefaultMaxResets,
	}
	for _, pkg := range packages {
		scheduler.Packages[pkg.Name] = pkg
	}

	for _, opt := range opts {
		opt(scheduler)
	}

	return scheduler
}

// RunDelay returns the effective default minimum interval between runs.
func (s *Scheduler) RunDelay() time.Duration { return s.runDelay }

// RunTimeout returns the effective run timeout.
func (s *Scheduler) RunTimeout() time.Duration { return s.runTimeout }

// MaxResets returns the effective reset limit.
func (s *Scheduler) MaxResets() int { return s.maxResets }

func (s *Scheduler) Schedule(ctx context.Context, packageName string, args ...string) (*tester.Run, error) {
	pkg, exists := s.Packages[packageName]
	if !exists {
		return nil, fmt.Errorf("unknown package: %s", packageName)
	}

	fs := flag.NewFlagSet(packageName, flag.ContinueOnError)
	runPkgOptions := map[string]*string{}
	for _, option := range pkg.Options {
		runPkgOptions[option.Name] = fs.String(option.Name, option.Default, option.Description)
	}
	err := fs.Parse(args)
	if err != nil {
		return nil, fmt.Errorf("parsing run options: %w", err)
	}

	var runArgs []string
	for _, opt := range pkg.Options {
		if value, set := runPkgOptions[opt.Name]; set && value != nil && *value != "" {
			runArgs = append(runArgs, fmt.Sprintf("-%s=%s", opt.Name, *value))
		}

	}

	run := &tester.Run{
		ID:         uuid.New(),
		Package:    pkg.Name,
		Args:       runArgs,
		EnqueuedAt: time.Now(),
	}
	err = s.db.EnqueueRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("scheduling package: %w", err)
	}

	log.Printf("scheduled run %s with args: %q", pkg.Name, strings.Join(runArgs, ", "))
	return run, nil
}

// Run starts the scheduler.
func (s *Scheduler) Run() {
	wait := 0 * time.Second
	for {
		select {
		case <-s.stop:
			return
		case <-time.After(wait):
		}
		wait = time.Duration((rand.Int() % 10)) * time.Second

		ctx := context.Background()
		var eg errgroup.Group
		eg.Go(func() error {
			return s.scheduleRuns(ctx)
		})
		eg.Go(func() error {
			return s.resetStaleRuns(ctx)
		})
		eg.Go(func() error {
			return s.cleanupUnprocessableRuns(ctx)
		})
		err := eg.Wait()
		if err != nil {
			log.Printf("scheduling error: %s", err)
		}
	}
}

// Stop stops the scheduler.
func (s *Scheduler) Stop() {
	close(s.stop)
}

func (s *Scheduler) scheduleRuns(ctx context.Context) error {
	var errs []error
	for _, pkg := range s.Packages {
		runDelay := s.runDelay
		if pkg.RunDelay > 0 {
			runDelay = pkg.RunDelay
		}

		var args []string
		for _, option := range pkg.Options {
			if option.Default != "" {
				o := tester.Option{
					Name:  option.Name,
					Value: option.Default,
				}
				args = append(args, o.String())
			}
		}
		scheduled, err := s.db.ScheduleRun(ctx, &tester.Run{
			ID:         uuid.New(),
			Package:    pkg.Name,
			Args:       args,
			EnqueuedAt: time.Now(),
		}, runDelay)
		if err != nil {
			errs = append(errs, fmt.Errorf("scheduling %s: %w", pkg.Name, err))
			continue
		}
		if scheduled {
			log.Printf("scheduled run %s", pkg.Name)
		}
	}

	return errors.Join(errs...)
}

func (s *Scheduler) cleanupUnprocessableRuns(ctx context.Context) error {
	runs, err := s.db.ListPendingRuns(ctx)
	if err != nil {
		return err
	}

	for _, run := range runs {
		// Cleanup runs that haven't been picked up for 1 day.
		// This usually indicates an old run/package that is no longer runnable.
		if !run.StartedAt.IsZero() || time.Now().Sub(run.EnqueuedAt) < 24*time.Hour {
			continue
		}

		err := s.db.DeleteRun(ctx, run.ID)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Scheduler) resetStaleRuns(ctx context.Context) error {
	runs, err := s.db.ListPendingRuns(ctx)
	if err != nil {
		return err
	}

	for _, run := range runs {
		if run.StartedAt.IsZero() || !run.FinishedAt.IsZero() {
			continue
		}

		if time.Since(run.StartedAt) <= s.runTimeout {
			continue
		}

		if run.ResetCount >= s.maxResets {
			msg := fmt.Sprintf("Run exceeded the %s run timeout %d time(s) and was not retried again (runner: %q).",
				s.runTimeout, run.ResetCount+1, run.Meta.Runner)
			err = s.db.FailRun(ctx, run.ID, msg)
			if err != nil {
				return fmt.Errorf("failing timed out run %s (%s): %w", run.ID, run.Package, err)
			}
			log.Printf("failed run %s (%s): exceeded run timeout %d time(s)", run.Package, run.ID, run.ResetCount+1)
			continue
		}

		err = s.db.ResetRun(ctx, run.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			return err
		}
		log.Printf("reset run %s (%s): exceeded run timeout, attempt %d of %d", run.Package, run.ID, run.ResetCount+2, s.maxResets+1)
	}

	return nil
}
