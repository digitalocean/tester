package telemetry

import (
	"context"
	"log"
	"time"

	"github.com/digitalocean/tester"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ScopeName is the instrumentation scope for every tester instrument.
const ScopeName = "github.com/digitalocean/tester"

// Attribute keys shared by the instruments below. Kept short and stable: they
// become Prometheus labels on the far side of the edge.
const (
	AttrPackage = attribute.Key("package")
	AttrName    = attribute.Key("name")
	AttrState   = attribute.Key("state")
	AttrResult  = attribute.Key("result")
)

// Result values for AttrResult.
const (
	ResultClaimed   = "claimed"
	ResultEmpty     = "empty"
	ResultError     = "error"
	ResultCompleted = "completed"
	ResultFailed    = "failed"
	ResultReset     = "reset"
)

// testDurationBuckets mirrors the buckets of the Prometheus histogram this
// replaces (tester_tb_run_duration_s), so history stays comparable.
var testDurationBuckets = []float64{
	0.001, 0.01,
	0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
	1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5,
	6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120,
	180, 240, 300, 360, 420, 480, 540, 600, 660, 720, 780, 840, 900,
}

// runDurationBuckets covers whole-run timescales: seconds to hours. Queue
// wait was measured at ~140 min median before the scheduler fixes, so the
// upper buckets exist to make regressions visible, not because they are
// acceptable.
var runDurationBuckets = []float64{
	1, 5, 15, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200, 10800, 14400,
}

// Server-side instruments (tester serve).
var (
	// TestDuration is the wall time of one test as reported by the runner.
	TestDuration metric.Float64Histogram
	// TestLastRun is the unix time of the most recent report for a test.
	TestLastRun metric.Float64Gauge
	// RunQueueWait is enqueued_at -> started_at for a claimed run.
	RunQueueWait metric.Float64Histogram
	// RunDuration is started_at -> finished_at for a completed or failed run.
	RunDuration metric.Float64Histogram
	// RunClaims counts claim requests by result.
	RunClaims metric.Int64Counter
	// RunScheduled counts runs enqueued by the scheduler.
	RunScheduled metric.Int64Counter
	// RunReset counts runs the scheduler reset or failed for exceeding the
	// run timeout.
	RunReset metric.Int64Counter
)

// Runner-side instruments (tester run).
var (
	// RunnerPolls counts claim attempts by result. Its presence at a steady
	// rate is the runner heartbeat; ResultError distinguishes a runner that
	// cannot reach the server from one that is idle.
	RunnerPolls metric.Int64Counter
	// RunnerRunDuration is claim -> report for a run as seen by the runner.
	RunnerRunDuration metric.Float64Histogram
)

func init() {
	// otel's global MeterProvider delegates to whatever Setup installs later,
	// so instruments created here work whether or not export is enabled.
	// Setup re-binds them to the real provider once it exists.
	bind(otel.Meter(ScopeName))
}

// bind (re)creates every instrument on m. Called from init against the
// global delegating meter, from Setup against the installed SDK provider,
// and from tests against a provider with a manual reader.
func bind(m metric.Meter) {
	var err error

	TestDuration, err = m.Float64Histogram("tester.test.duration",
		metric.WithDescription("Wall time of a test as reported by the runner."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(testDurationBuckets...))
	must(err)
	TestLastRun, err = m.Float64Gauge("tester.test.last_run",
		metric.WithDescription("Unix time of the most recent report for a test."),
		metric.WithUnit("s"))
	must(err)
	RunQueueWait, err = m.Float64Histogram("tester.run.queue_wait",
		metric.WithDescription("Time a run waited in the queue before a runner claimed it."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(runDurationBuckets...))
	must(err)
	RunDuration, err = m.Float64Histogram("tester.run.duration",
		metric.WithDescription("Time from claim to completion or failure of a run."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(runDurationBuckets...))
	must(err)
	RunClaims, err = m.Int64Counter("tester.run.claims",
		metric.WithDescription("Claim requests handled by the server, by result."),
		metric.WithUnit("{claim}"))
	must(err)
	RunScheduled, err = m.Int64Counter("tester.run.scheduled",
		metric.WithDescription("Runs enqueued by the scheduler."),
		metric.WithUnit("{run}"))
	must(err)
	RunReset, err = m.Int64Counter("tester.run.reset",
		metric.WithDescription("Runs reset or failed by the scheduler for exceeding the run timeout."),
		metric.WithUnit("{run}"))
	must(err)

	RunnerPolls, err = m.Int64Counter("tester.runner.polls",
		metric.WithDescription("Claim attempts made by the runner, by result."),
		metric.WithUnit("{poll}"))
	must(err)
	RunnerRunDuration, err = m.Float64Histogram("tester.runner.run.duration",
		metric.WithDescription("Time from claim to report of a run as seen by the runner."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(runDurationBuckets...))
	must(err)
}

// PendingRunsSource is the subset of db.DB the pending-runs gauge needs.
type PendingRunsSource interface {
	ListPendingRuns(ctx context.Context) ([]*tester.Run, error)
}

// RegisterPendingRunsGauge observes tester.runs.pending{package,state} from
// the database on every export. state is "queued" (not started) or "running".
// Every configured package is reported in both states, with 0 when empty, so
// "queue depth for package X" is always a present series and alert rules do
// not have to reason about absent data.
func RegisterPendingRunsGauge(src PendingRunsSource, packages []string) error {
	m := otel.Meter(ScopeName)
	pending, err := m.Int64ObservableGauge("tester.runs.pending",
		metric.WithDescription("Runs that have not finished, by package and state."),
		metric.WithUnit("{run}"))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		runs, err := src.ListPendingRuns(ctx)
		if err != nil {
			return err
		}
		type key struct{ pkg, state string }
		counts := make(map[key]int64, 2*len(packages))
		for _, pkg := range packages {
			counts[key{pkg, "queued"}] = 0
			counts[key{pkg, "running"}] = 0
		}
		for _, run := range runs {
			state := "queued"
			if !run.StartedAt.IsZero() {
				state = "running"
			}
			counts[key{run.Package, state}]++
		}
		for k, n := range counts {
			o.ObserveInt64(pending, n, metric.WithAttributes(AttrPackage.String(k.pkg), AttrState.String(k.state)))
		}
		return nil
	}, pending)
	return err
}

// Seconds converts a duration to the float seconds every duration instrument
// here records.
func Seconds(d time.Duration) float64 { return d.Seconds() }

func must(err error) {
	if err != nil {
		log.Fatalf("telemetry: creating instrument: %s", err)
	}
}
