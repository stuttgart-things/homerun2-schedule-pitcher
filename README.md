# homerun2-schedule-pitcher

homerun2 pitcher that runs scheduled checks (token & certificate expiry, probes) and user-defined reminders, and pitches the results into homerun2

> **Status: design phase.** Nothing here runs scheduled checks yet. The design
> (check types, reminders, web UI, config/storage, delivery and deployment) is
> tracked in the design issue:
> [#1 Design: homerun2-schedule-pitcher](https://github.com/stuttgart-things/homerun2-schedule-pitcher/issues/1).

The code in this repository is the output of the Backstage
scaffolder template `homerun2-service` (service type `pitcher`), rendered for
this repo (plus `go mod tidy`, `gofmt`, and the empty catcher-only stub
packages dropped). It is a generic "HTTP `POST /pitch` -> Redis Streams" pitcher and
builds (`go build .`; `dagger/` is the Dagger module and is
built by `dagger`, as in the sibling repos), but it is only the starting point: the scheduler,
the check types, reminders and the UI described in #1 do not exist yet.

The scaffold's GitHub Actions workflows are intentionally **not** committed yet
(see #1): they would run Dagger lint/build and semantic-release against a
service that is not implemented. They will be added together with the first
real code.

The sections below describe the scaffold as generated.

## API Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/health` | `GET` | None | Health check (returns version, commit, date) |
| `/pitch` | `POST` | Bearer token | Submit a message to Redis Streams |

<details>
<summary><b>Pitch a message</b></summary>

```bash
curl -X POST http://localhost:8080/pitch \
  -H "Authorization: Bearer <YOUR_AUTH_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Test Notification",
    "message": "Hello from homerun2-schedule-pitcher",
    "severity": "info",
    "author": "test"
  }'
```

</details>

## Deployment

<details>
<summary><b>Container image (ko / ghcr.io)</b></summary>

```bash
docker pull ghcr.io/stuttgart-things/homerun2-schedule-pitcher:<tag>

docker run \
  -e REDIS_ADDR=redis -e REDIS_PORT=6379 \
  -e REDIS_STREAM=messages \
  -e AUTH_TOKEN=mysecret \
  -p 8080:8080 \
  ghcr.io/stuttgart-things/homerun2-schedule-pitcher:<tag>
```

</details>

## Development

<details>
<summary><b>Configuration reference</b></summary>

| Variable | Description | Default |
|----------|-------------|---------|
| `REDIS_ADDR` | Redis server address | `localhost` |
| `REDIS_PORT` | Redis server port | `6379` |
| `REDIS_PASSWORD` | Redis password | (empty) |
| `REDIS_STREAM` | Redis stream name | `messages` |
| `PORT` | HTTP server port | `8080` |
| `AUTH_TOKEN` | Bearer token for auth | (required) |
| `PITCHER_MODE` | Backend: `redis` or `file` | `redis` |
| `LOG_FORMAT` | `json` or `text` | `json` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |

</details>

## Testing

```bash
# Unit tests (no Redis needed)
go test ./...

# Integration tests (Dagger + Redis)
task build-test-binary

# Lint
task lint

# Build + scan image
task build-scan-image-ko
```

## Links

- [Releases](https://github.com/stuttgart-things/homerun2-schedule-pitcher/releases)
- [homerun-library](https://github.com/stuttgart-things/homerun-library)
