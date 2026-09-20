# tester

Scheduling, running and reporting for Go test binaries. This is DigitalOcean's
fork of [nanzhong/tester](https://github.com/nanzhong/tester), which upstream
archived in favour of [tstr](https://github.com/nanzhong/tstr).

It runs the scheduled end-to-end canaries at `e2e.digitalocean.tools`. The test
suites themselves live in [digitalocean/e2e](https://github.com/digitalocean/e2e),
whose image is built `FROM` the image published by this repository. Work on the
fork is tracked under Jira epic
[MARSOHS-1585](https://do-internal.atlassian.net/browse/MARSOHS-1585).

## Components

- **Server** (`tester serve`): holds the package configuration, schedules runs,
  serves the UI and the runner API, alerts on failures. State is in Postgres.
- **Runner** (`tester run`): claims runs from the server, executes the test
  binary, streams results back. Any number of runners can share a server.
- **Configuration**: one JSON file shared by server and runners.

```jsonc
{
  "packages": [
    {
      "name": "pkg",                       // test package name
      "path": "/opt/tester/bin/pkg.test",  // path to the compiled test binary
      "run_delay": "15m",                  // optional: minimum time between scheduled runs of this package
      "options": [                         // test binary flags the UI/Slack command may set
        { "name": "test.timeout", "description": "Maximum time tests can run for", "default": "1m" }
      ]
    }
  ],
  "scheduler": {
    "run_timeout": "30m"                   // a run older than this with no result is reset
  },
  "slack": {
    "default_channels": ["alerts"],
    "custom_channels": { "pkg": ["pkg-alerts"] }
  }
}
```

## Usage

```sh
go install github.com/digitalocean/tester/cmd/tester@latest
tester --help
```

Every push to `main` publishes `ghcr.io/digitalocean/tester:sha-<8>` and
`:edge` via [`.github/workflows/image.yml`](.github/workflows/image.yml).
`digitalocean/e2e` pins one of the `sha-*` tags in its `Dockerfile`.

### Server

```sh
tester serve \
  --addr 0.0.0.0:8080 \
  --base-url https://e2e.example.com \
  --api-key "$API_KEY" \
  --config /etc/tester/config.json \
  --pg-dsn "$PG_DSN"
```

Every flag can also be set as an environment variable prefixed with `SERVE_`
(`SERVE_API_KEY`, `SERVE_PG_DSN`, ...).

Slack alerting and the `/tester` slash command need `--slack-access-token` and
`--slack-signing-secret`. Okta login for the UI needs `--okta-client-id`,
`--okta-client-secret`, `--okta-issuer`, `--okta-redirect-uri` and
`--okta-session-key`.

### Runner

```sh
tester run \
  --tester-addr http://web:8080 \
  --api-key "$API_KEY" \
  --test-bins-path /bin \
  --local-test-bins-only \
  --packages-include pkg1,pkg2 \
  --packages-exclude pkg3
```

Flags can be set as `RUN_*` environment variables.

## Development

```sh
make dev/up        # postgres:16 in docker on 127.0.0.1:5432
make dev/test      # go test ./... with PG_DSN pointed at it
make dev/down
make generate      # regenerate db/db_mock.go after changing db.DB
```

`db` tests are skipped when `PG_DSN` is unset.
