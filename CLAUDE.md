# CLAUDE.md

## Project

homerun2-schedule-pitcher — homerun2 pitcher that runs scheduled checks (token & certificate expiry, probes) and user-defined reminders, and pitches the results into homerun2

## Status

MVP in progress. The design lives in issue #1; implement against it and keep
it updated when decisions change. Done: scheduler core (#8) with profile
loader, `github-token-expiry` and `tls-endpoint` checks, state machine,
omni-pitcher delivery (`grafana` / `generic`), `/api/checks`; Redis store with
history and locks (#10); findings ingest with office-hours delivery and
acknowledge (#14); check results as findings and agent mode (#16): agents on
the clusters run checks and send results to the central `POST /findings`.
Next (MVP 2 in #1): discovery in the agent, CI workflows + KCL, web UI,
reminders. The
instance runs on platform and uses the homerun2 `redis-stack`. The Dagger module in
`dagger/` still holds the scaffold's integration test and needs updating with
the CI task.

## Layout

- `main.go`: `serve` (scheduler + HTTP) and `run` (one pass, `--dry-run`)
- `internal/profile`: `SchedulePitcherProfile` types, defaults, validation
- `internal/checks`: one file per check type, `Result` / error = could not check
- `internal/state`: bands and the pitch decision (pure, unit-tested)
- `internal/scheduler`: cron, locks, runs checks, pitches, stores state
- `internal/pitcher`: rendering and delivery (HTTP grafana/generic, file, stdout)
- `internal/store`: `Store` interface, `Memory` (no-Redis mode), `Redis`; one
  contract test runs against both (miniredis)
- `internal/findings`: `POST /findings` model and `Apply` (pure), `Build` of
  office-hours messages (pure), `Service` (ingest, ack, hourly `Tick`), stores
- `internal/report`: check results -> findings (`FromStatuses`), senders to
  the local findings service (central) or a central `/findings` (agent)

## Tech Stack

- **Language**: Go 1.26+
- **HTTP**: stdlib `net/http` (no framework)
- **Delivery**: HTTP to omni-pitcher; `homerun-library/v4` for the message model
- **Build**: ko (`.ko.yaml`), no Dockerfile
- **CI**: Dagger modules (`dagger/main.go`), Taskfile
- **Infra**: GitHub Actions, semantic-release, renovate

## Git Workflow

**Branch-per-issue with PR and merge.**

### Branch naming

- `fix/<issue-number>-<short-description>` for bugs
- `feat/<issue-number>-<short-description>` for features
- `test/<issue-number>-<short-description>` for test-only changes

### Commit messages

- Use conventional commits: `fix:`, `feat:`, `test:`, `chore:`, `docs:`
- End with `Co-Authored-By: Claude <model> <noreply@anthropic.com>` when Claude authored
- Include `Closes #<issue-number>` to auto-close issues

## Code Conventions

- No Dockerfile — use ko for image builds
- Config via environment variables, loaded once at startup
- Tests: `go test ./...` — unit tests must not require Redis

## Testing

```bash
go test ./...
task build-test-binary
task lint
task build-scan-image-ko
```
