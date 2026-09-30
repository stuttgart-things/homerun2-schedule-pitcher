# CLAUDE.md

## Project

homerun2-schedule-pitcher — homerun2 pitcher that runs scheduled checks (token & certificate expiry, probes) and user-defined reminders, and pitches the results into homerun2

## Status

Design phase. The design lives in issue #1; implement against it and keep it
updated when decisions change. The current code is the rendered
`homerun2-service` scaffold (pitcher), not the schedule-pitcher yet. CI
workflows from the scaffold are not committed yet; add them with the first
real code.

## Tech Stack

- **Language**: Go 1.25+
- **HTTP**: stdlib `net/http` (no framework)
- **Queue**: Redis Streams via `homerun-library`
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
