# digitalocean/tester: fork bootstrap, queue reliability, native OTLP metrics

Epic: MARSOHS-1585. Children covered here: MARSOHS-1592 (bootstrap), MARSOHS-1586/1587/1588/1589 (scheduler and claim), MARSOHS-1593 (OTLP metrics).

## Context

`digitalocean/tester` is a fork of the archived `nanzhong/tester`. It runs the scheduled e2e canaries behind `e2e.digitalocean.tools` that MARS uses to monitor customer journeys. The `#e2e` analysis of 2026-09-10 found that median enqueue-to-start latency is ~140 min, longer than the OHP outages the canary was supposed to catch. The structural causes are in this codebase:

- each `web` replica runs its own scheduler with an in-memory `lastScheduledAt`, so two replicas double-enqueue every package;
- `serve.go` builds `schedulerOpts` and never passes it to `NewScheduler`, so `run_timeout` is always the 15m default;
- `ResetRun` keeps `enqueued_at`, so a timed-out run returns to the head of the queue;
- the claim endpoint is `ListPendingRuns` then `StartRun` with no lock and a discarded error, so the worker cannot be scaled past one replica;
- metrics are Prometheus pull on `/metrics`, which App Platform cannot scrape per pod, which is the other reason the worker is pinned at one replica.

## Goals

1. Own the build: the fork builds, tests and publishes an image from `main` without any nanzhong infrastructure.
2. Scheduling and claiming are correct with N `web` replicas and M `worker` replicas.
3. Metrics are pushed over OTLP/HTTP to the DigitalOcean observability edge, the same path the OHP cluster's otel-gateway uses, from both `web` and `worker`.

## Non-goals

- Replacing tester with `tstr` or a GitHub Actions cron.
- Runner silent-failure fixes beyond what metrics need (MARSOHS-1590 is a follow-up).
- Changing the e2e test suites themselves.
- Keeping the Prometheus `/metrics` endpoint. App Platform could never scrape it usefully; it is removed rather than kept behind a flag.

## Delivery: three stacked draft PRs

### PR 1: bootstrap (MARSOHS-1592)

- Module path `github.com/nanzhong/tester` -> `github.com/digitalocean/tester`.
- `go 1.25`; dependencies upgraded in place (pgx stays on v4, tern on v1, to keep the DB layer diff small). `github.com/golang/mock` is archived; switch to `go.uber.org/mock` and regenerate `db/db_mock.go`.
- `pkger` (archived) replaced with `embed` for `http/templates`.
- Dockerfiles move to `golang:1.25`. The runtime image stays a `golang` image because the runner shells out to `go tool test2json`.
- CI: `test.yml` runs `go vet` and `go test -race` against a Postgres service on `main`/PRs. `image.yml` publishes `registry.digitalocean.com/do-e2e-canaries/tester:sha-<7>` and `:edge` from `main` (PRs build only); needs the `DOCR_E2E_CANARIES_REGISTRY_DOCKERPULLJSON` secret (Docker config.json). `digitalocean/e2e`'s `Dockerfile` will switch its `FROM` to this image in a follow-up PR there.
- `README.md` replaces `README.org`: fork purpose, how it is deployed, link to the epic.

### PR 2: scheduler and claim (MARSOHS-1586, 1587, 1588, 1589)

DB layer (`db/pg.go`, new migration):

- `runs.reset_count int NOT NULL DEFAULT 0`.
- `ScheduleRun(ctx, run, minInterval) (scheduled bool, err)`: in one transaction, `pg_advisory_xact_lock(hashtext(package))`, then insert only if no unfinished run for the package exists and `max(enqueued_at)` for the package is older than `minInterval`. This is the only path the periodic scheduler uses, so any number of `web` replicas converge on exactly one run per package per interval, with no process-local state.
- `ClaimRun(ctx, runner, include, exclude) (*Run, err)`: single `UPDATE ... WHERE id = (SELECT id ... WHERE started_at IS NULL AND finished_at IS NULL ... ORDER BY enqueued_at LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING`. Returns `ErrNotFound` when nothing is claimable. Concurrent claimers never receive the same run.
- `ResetRun` sets `enqueued_at = now()` and `reset_count = reset_count + 1` so a reset run goes to the back of the queue.

Scheduler (`scheduler/scheduler.go`):

- `lastScheduledAt` removed; `scheduleRuns` calls `ScheduleRun` per package with the package's `run_delay` (or the scheduler default).
- `resetStaleRuns` fails a run with a clear error once `reset_count` reaches `max_resets` (default 2) instead of resetting it forever.
- `WithMaxResets` option; `serve.go` passes all options and logs the effective values.

Config: `scheduler.run_delay` and `scheduler.max_resets` added alongside `run_timeout`.

API (`http/api.go`): `claimRun` uses `ClaimRun`; 404 on `ErrNotFound`, 500 on any other error. `StartRun` is removed from the `DB` interface.

### PR 3: native OTLP metrics (MARSOHS-1593)

New package `telemetry`:

- `Setup(ctx, defaultServiceName) (shutdown func(context.Context) error, error)`. If `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` is set, installs an `otlpmetrichttp` exporter behind a periodic reader as the global `MeterProvider`; otherwise installs a no-op provider and logs that export is disabled. All exporter settings come from the standard `OTEL_*` env vars, so the App Platform spec carries the endpoint, `OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer <token>`, `OTEL_EXPORTER_OTLP_COMPRESSION=gzip` and `OTEL_SERVICE_NAME`. Resource is built from env plus defaults `service.name=<default>`, `service.instance.id=<hostname>`.
- Both `tester serve` and `tester run` call `Setup` and flush on shutdown.

Instruments (all under meter `github.com/digitalocean/tester`):

| name | kind | unit | attributes | emitted by |
| --- | --- | --- | --- | --- |
| `tester.test.duration` | histogram | s | `name`, `state` | server, on test submit (replaces `tester_tb_run_duration_s`) |
| `tester.test.last_run` | gauge | s (unix) | `name`, `state` | server (replaces `testr_tb_run_last_timestamp`) |
| `tester.run.queue_wait` | histogram | s | `package` | server, on claim |
| `tester.run.duration` | histogram | s | `package`, `result` | server, on complete/fail |
| `tester.run.claims` | counter | {claim} | `result` = claimed/empty/error | server |
| `tester.run.scheduled` | counter | {run} | `package` | server |
| `tester.run.reset` | counter | {run} | `package`, `result` = reset/failed | server |
| `tester.runs.pending` | observable gauge | {run} | `package`, `state` = queued/running | server, from DB |
| `tester.runner.polls` | counter | {poll} | `result` = claimed/empty/error | runner |
| `tester.runner.run.duration` | histogram | s | `package`, `result` | runner |

The Prometheus client and `/metrics` route are removed. Existing dashboards keyed on `tester_tb_run_duration_s` move to `tester_test_duration_seconds` (the edge's Prometheus naming of the OTel metric).

## Testing

- `db` tests run against Postgres (`PG_DSN`), including a concurrent-claim test with several goroutines and a scheduler idempotency test.
- `scheduler` gets unit tests against the mock DB for the reset/fail threshold.
- `http` API tests updated for `ClaimRun`.
- `telemetry` has a smoke test that `Setup` returns a working provider with and without the env var.

## Rollout

Ship PR 1, publish an image, point `digitalocean/e2e`'s `Dockerfile` at it, verify the app still runs. Then PR 2 and PR 3 in order. Raising `worker` replicas (MARSOHS-1594) happens only after PR 2 and PR 3 are live.
