// Package telemetry wires tester's metrics to OpenTelemetry and pushes them
// over OTLP/HTTP.
//
// tester used to expose Prometheus metrics on /metrics for something to
// scrape. On App Platform nothing can scrape a specific instance, which is
// one reason the worker was pinned to a single replica. Pushing from every
// process removes that constraint and matches how the OHP cluster ships
// metrics to DigitalOcean's observability edge.
//
// All exporter configuration comes from the standard OTEL_* environment
// variables, so the deploy spec, not the code, owns endpoint and credentials:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-nyc3.digitalocean.com:443
//	OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer <DO API token>
//	OTEL_EXPORTER_OTLP_COMPRESSION=gzip
//	OTEL_SERVICE_NAME=tester
//	OTEL_METRIC_EXPORT_INTERVAL=60000   (ms, optional)
//
// When no endpoint is configured Setup leaves the no-op global provider in
// place and logs once; instruments stay usable and cost nothing.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Enabled reports whether an OTLP endpoint is configured in the environment.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// Setup installs the global MeterProvider. defaultServiceName is used when
// OTEL_SERVICE_NAME is not set; the process hostname becomes
// service.instance.id unless OTEL_RESOURCE_ATTRIBUTES overrides it. The
// returned shutdown flushes pending metrics and must be called on exit.
func Setup(ctx context.Context, defaultServiceName string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	if !Enabled() {
		log.Printf("telemetry: OTEL_EXPORTER_OTLP_ENDPOINT not set, metrics export disabled")
		return noop, nil
	}

	res, err := buildResource(ctx, defaultServiceName)
	if err != nil {
		return noop, err
	}

	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return noop, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	otel.SetMeterProvider(provider)
	bind(provider.Meter(ScopeName))

	log.Printf("telemetry: exporting metrics over OTLP/HTTP to %s as service.name=%q service.instance.id=%q",
		endpointForLog(), attr(res, semconv.ServiceNameKey), attr(res, semconv.ServiceInstanceIDKey))

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return provider.Shutdown(ctx)
	}, nil
}

// buildResource describes this process. Defaults are applied first and the
// environment second, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win
// over defaultServiceName and the hostname.
func buildResource(ctx context.Context, defaultServiceName string) (*resource.Resource, error) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(defaultServiceName),
			semconv.ServiceInstanceID(hostname),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("building resource: %w", err)
	}
	return res, nil
}

func attr(res *resource.Resource, key attribute.Key) string {
	v, ok := res.Set().Value(key)
	if !ok {
		return ""
	}
	return v.AsString()
}

func endpointForLog() string {
	if v := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); v != "" {
		return v
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
}
