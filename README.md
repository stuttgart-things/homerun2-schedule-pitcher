# homerun2-schedule-pitcher

homerun2 pitcher that runs scheduled checks (token & certificate expiry, probes) and user-defined reminders, and pitches the results into homerun2

> **Status: MVP in progress.** The design is tracked in
> [#1 Design: homerun2-schedule-pitcher](https://github.com/stuttgart-things/homerun2-schedule-pitcher/issues/1).
> Implemented so far: the scheduler core with the checks `github-token-expiry`
> and `tls-endpoint`, the state machine, delivery to omni-pitcher (`grafana` and
> `generic` format), state and history in Redis (or in memory without Redis),
> `/health`, `/ready`, `/metrics` and a small JSON API. Still to come (MVP 2 in
> #1): findings from other jobs, office-hours delivery, reminders, multi-cluster
> secret access, the web UI, CI workflows and KCL manifests.

## How it works

The service reads a `SchedulePitcherProfile` (usually a mounted ConfigMap),
runs every check at startup and then on its `schedule`, and pitches to
omni-pitcher when the state of a check changes:

| Situation | Pitch |
|---|---|
| Check enters a band (`warning` / `error` / `critical`) or changes band | immediately, with that severity |
| Check stays in a bad band | again at most once per `remind` cadence (default daily 08:00 Europe/Berlin) |
| Check is ok again (for example a token was rotated) | once, `success` / Grafana `resolved` |
| Check could not complete (network error, 5xx, missing Secret) | separate `warning` "Could not check …", rate-limited like reminders, never reported as an expiry |
| Token without expiry date | `info`, once |

Bands come from the time left until expiry: at or below `thresholds.warning`
is warning, `error` is error, `critical` or expired is critical. Defaults are
`30d`/`7d`/`1d`, and `30d`/`14d`/`3d` for `github-token-expiry`, since tokens
need more lead time to rotate. Precedence per field: check, `spec.defaults`,
type default, global default. A check can also raise the band itself: a token GitHub
rejects (`401`) is critical, a certificate that is not trusted or not valid for
the host name is error.

### Checks

| Type | What it does |
|---|---|
| `github-token-expiry` | `GET /rate_limit` with the token and reads the `github-authentication-token-expiration` header. `401` means expired or revoked. `GET /user` adds the token owner (`owner: false` turns that off). The token is read again on every run, so a rotated Secret is picked up. |
| `tls-endpoint` | TLS dial to `target` (`host[:port]`, port defaults to 443) with SNI, reads the leaf `NotAfter`, or the earliest `NotAfter` of the presented chain with `chain: true`. Trust and host name are verified against the system roots plus `caFile`. An expired certificate is still read and reported. |

### Profile

```yaml
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
metadata:
  name: machinery
spec:
  redis:                       # optional; without it state is kept in memory
    addr: redis-stack.homerun2.svc.cluster.local
    port: "6379"               # password: REDIS_PASSWORD or password / passwordFrom
  pitcher:
    addr: https://omni.platform.sthings-vsphere.labul.sva.de/pitch/grafana
    format: grafana            # grafana (POST /pitch/grafana) | generic (POST /pitch)
    caFile: /etc/ssl/vault-pki-ca/ca.crt
    auth:
      tokenFrom:
        secretKeyRef: { name: omni-pitcher-labul-platform, namespace: crossplane-system, key: token }
  defaults:
    schedule: "0 */6 * * *"    # cron or @every 6h
    timezone: Europe/Berlin
    remind: "0 8 * * *"
    thresholds: { warning: 30d, error: 7d, critical: 1d }
    system: homerun2-schedule-pitcher
    assignee: patrick.hermann
    tags: [expiry]
  checks:
    - id: github-runner-pat
      type: github-token-expiry
      description: Fine-grained PAT used by ARC runners
      tokenFrom:
        secretKeyRef: { name: github-runner-token, namespace: tekton-ci, key: GITHUB_TOKEN }
      thresholds: { warning: 30d, error: 14d, critical: 3d }
      url: https://github.com/settings/personal-access-tokens
    - id: omni-platform-tls
      type: tls-endpoint
      target: omni.platform.sthings-vsphere.labul.sva.de
      caFile: /etc/ssl/vault-pki-ca/ca.crt
      chain: true
```

Secret values (`tokenFrom`, `passwordFrom`) take exactly one of
`secretKeyRef` (read through the Kubernetes API, in-cluster or `KUBECONFIG`;
a missing namespace means the pod's own), `env` or `file`.

Per check, `schedule`, `remind`, `thresholds` (field by field), `assignee` and
`tags` (added to the default tags) override `spec.defaults`. Further fields:
`description`, `url`, `assignee`, `paused`, `timeout` (default `10s`),
`apiURL` (GitHub Enterprise), `serverName` (TLS SNI / name to verify).
Unknown fields are rejected. See [`profiles/`](profiles/) for examples.

## Commands

```bash
# Scheduler + HTTP API (default command)
homerun2-schedule-pitcher serve --profile /etc/homerun2-schedule-pitcher/profile.yaml

# One pass, no state: pitches everything that is not ok, then exits.
# For CI or a ScheduledRun. --dry-run prints the messages instead.
homerun2-schedule-pitcher run --profile profile.yaml [--dry-run] [--check <id>]
```

## API Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/health` | `GET` | None | Liveness (version, commit, date) |
| `/ready` | `GET` | None | `200` once the scheduler runs and the state store answers |
| `/metrics` | `GET` | None | Prometheus metrics |
| `/api/checks` | `GET` | Bearer | All checks with band, expiry, last result, next run |
| `/api/checks/{id}/run` | `POST` | Bearer | Run a check now; returns its new state |
| `/api/checks/{id}/history?limit=N` | `GET` | Bearer | Last runs of a check, newest first (default 20, max 100) |

### State in Redis

With `spec.redis.addr` (or `REDIS_ADDR`) the state survives restarts and is
shared between replicas; without it the service keeps state in memory and
pitches a check that is not ok once more after a restart. Keys (prefix
`homerun2-schedule-pitcher:`, `spec.redis.prefix` changes it):

| Key | Type | Content |
|---|---|---|
| `…:check:<id>:state` | hash | band, expiry, summary, problem, failing, last error, what was pitched when (`redis-cli HGETALL`) |
| `…:check:<id>:history` | stream | last 100 runs, one JSON `entry` each |
| `…:lock:<id>` | string + TTL | run lock with an owner token, so two replicas never run the same check at once |

Metrics: `schedule_pitcher_check_last_run_timestamp_seconds{check}`,
`schedule_pitcher_check_status{check}` (0 ok … 3 critical, -1 unknown),
`schedule_pitcher_check_failing{check}`,
`schedule_pitcher_check_expiry_seconds{check}`,
`schedule_pitcher_pitch_total{result}`.

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `PROFILE_PATH` | Path to the profile (`--profile` overrides) | `/etc/homerun2-schedule-pitcher/profile.yaml` |
| `PORT` | HTTP server port | `8080` |
| `AUTH_TOKEN` | Bearer token for `/api/*` | (required for the API) |
| `PITCH_TARGET` | `http` (omni-pitcher from the profile), `file` or `stdout` | `http` |
| `PITCH_FILE` | File for `PITCH_TARGET=file` (JSON lines) | `pitched.log` |
| `PITCHER_ADDR` | Overrides `spec.pitcher.addr` | |
| `PITCHER_TOKEN` | Overrides `spec.pitcher.auth` | |
| `REDIS_ADDR` | Overrides `spec.redis.addr`; enables the Redis store | |
| `REDIS_PORT` | Overrides `spec.redis.port` | `6379` |
| `REDIS_PASSWORD` | Overrides `spec.redis.password` / `passwordFrom` | |
| `POD_NAMESPACE` | Namespace for `secretKeyRef` without namespace | pod namespace |
| `LOG_FORMAT` | `json` or `text` | `json` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `LOG_HEALTH_CHECKS` | Also log `/health`, `/ready`, `/metrics` requests | `false` |

## Development

```bash
task run-once     # dry run of profiles/local.yaml (uses `gh auth token`)
task run-local    # serve profiles/local.yaml, pitching to stdout

go test ./...     # unit tests, no Redis or cluster needed (miniredis)
task lint
task build-scan-image-ko
```

The image is built with ko (`.ko.yaml`), there is no Dockerfile. The
scaffold's GitHub Actions workflows are not committed yet; they come with
the CI task of the MVP in #1.

## Links

- [Releases](https://github.com/stuttgart-things/homerun2-schedule-pitcher/releases)
- [homerun-library](https://github.com/stuttgart-things/homerun-library)
- [homerun2-omni-pitcher](https://github.com/stuttgart-things/homerun2-omni-pitcher)
