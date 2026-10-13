package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitalocean/tester"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

func TestSetup_DisabledWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	assert.False(t, Enabled())
	shutdown, err := Setup(context.Background(), "tester")
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))

	// Instruments remain safe to use against the no-op provider.
	RunClaims.Add(context.Background(), 1, metric.WithAttributes(AttrResult.String(ResultEmpty)))
}

func TestSetup_ExportsOverOTLPHTTP(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" && r.Method == http.MethodPost {
			requests.Add(1)
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer test-token")
	t.Setenv("OTEL_SERVICE_NAME", "tester-test")

	assert.True(t, Enabled())
	shutdown, err := Setup(context.Background(), "tester")
	require.NoError(t, err)

	RunClaims.Add(context.Background(), 1, metric.WithAttributes(AttrResult.String(ResultClaimed)))

	// Shutdown flushes the periodic reader, which must produce at least one
	// export to the fake edge.
	require.NoError(t, shutdown(context.Background()))
	assert.GreaterOrEqual(t, requests.Load(), int32(1), "expected at least one OTLP metrics POST")

	// Leave the global provider in a known state for other tests.
	otel.SetMeterProvider(sdkmetric.NewMeterProvider())
}

func TestSetup_ResourceDefaultsAndEnvPrecedence(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1") // never contacted: no export before shutdown w/o data
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=override")

	shutdown, err := Setup(context.Background(), "tester-default")
	require.NoError(t, err)
	defer func() {
		_ = shutdown(context.Background())
		otel.SetMeterProvider(sdkmetric.NewMeterProvider())
	}()

	_, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider)
	require.True(t, ok, "Setup must install an SDK MeterProvider")

	// Re-derive the resource the same way Setup does and check precedence.
	// (The SDK does not expose the provider's resource directly.)
	res, err := buildResource(context.Background(), "tester-default")
	require.NoError(t, err)
	attrs := res.Set()
	name, _ := attrs.Value(semconv.ServiceNameKey)
	assert.Equal(t, "tester-default", name.AsString(), "default service.name applies when env is empty")
	inst, _ := attrs.Value(semconv.ServiceInstanceIDKey)
	assert.Equal(t, "override", inst.AsString(), "OTEL_RESOURCE_ATTRIBUTES wins over the hostname default")
}

func TestInstruments_RecordExpectedSeries(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	bind(provider.Meter(ScopeName))
	defer func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider())
		bind(otel.Meter(ScopeName))
	}()

	ctx := context.Background()
	TestDuration.Record(ctx, 1.5, metric.WithAttributes(AttrName.String("TestA"), AttrState.String("passed")))
	TestLastRun.Record(ctx, 1700000000, metric.WithAttributes(AttrName.String("TestA"), AttrState.String("passed")))
	RunQueueWait.Record(ctx, 42, metric.WithAttributes(AttrPackage.String("agents")))
	RunClaims.Add(ctx, 1, metric.WithAttributes(AttrResult.String(ResultClaimed)))
	RunnerPolls.Add(ctx, 2, metric.WithAttributes(AttrResult.String(ResultEmpty)))

	src := &fakePending{runs: []*tester.Run{
		{ID: uuid.New(), Package: "agents"},
		{ID: uuid.New(), Package: "agents", StartedAt: time.Now()},
		{ID: uuid.New(), Package: "dbaas"},
	}}
	require.NoError(t, RegisterPendingRunsGauge(src, []string{"agents", "dbaas", "idle"}))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		assert.Equal(t, ScopeName, sm.Scope.Name)
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}

	for _, name := range []string{
		"tester.test.duration", "tester.test.last_run", "tester.run.queue_wait",
		"tester.run.claims", "tester.runner.polls", "tester.runs.pending",
	} {
		assert.Contains(t, byName, name)
	}
	assert.Equal(t, "s", byName["tester.test.duration"].Unit)
	assert.Equal(t, "{claim}", byName["tester.run.claims"].Unit)

	hist, ok := byName["tester.test.duration"].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, testDurationBuckets, hist.DataPoints[0].Bounds, "must keep the legacy Prometheus buckets")

	pending, ok := byName["tester.runs.pending"].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	got := map[string]int64{}
	for _, dp := range pending.DataPoints {
		pkg, _ := dp.Attributes.Value(AttrPackage)
		state, _ := dp.Attributes.Value(AttrState)
		got[pkg.AsString()+"/"+state.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{
		"agents/queued": 1, "agents/running": 1,
		"dbaas/queued": 1, "dbaas/running": 0,
		"idle/queued": 0, "idle/running": 0,
	}, got, "every configured package reports both states, zero when empty")
}

type fakePending struct{ runs []*tester.Run }

func (f *fakePending) ListPendingRuns(context.Context) ([]*tester.Run, error) { return f.runs, nil }
