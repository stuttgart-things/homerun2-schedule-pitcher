# homerun2-schedule-pitcher

Runs scheduled checks (GitHub token and TLS certificate expiry) and collects
findings from other jobs, keeps their state, and delivers them into homerun2
in office hours. The design is tracked in
[#1](https://github.com/stuttgart-things/homerun2-schedule-pitcher/issues/1).

```
 cluster A                cluster B                 VMs, other jobs
 agent (run/serve) ──┐    agent ──┐                 cron + curl ──┐
                     └──────────────┴── POST /findings ────────────┘
                                         │
                          central instance on platform
                     (state in redis-stack, delivery, UI) ──▶ omni-pitcher ──▶ Teams
```

- **Central instance** (`serve`): state in Redis, findings, office-hours
  delivery to omni-pitcher, API. Also runs its own checks.
- **Agent** (same binary with `spec.report`): runs the checks of its cluster
  next to the secrets, discovers labelled token Secrets, and sends only the
  results to the central `POST /findings`.

## Quick start

```bash
# Dry run of the local example profile (uses `gh auth token`)
task run-once

# Serve locally, pitching to stdout, state in memory
task run-local
```

See the [README](https://github.com/stuttgart-things/homerun2-schedule-pitcher#readme)
for the profile format, checks, discovery, findings and the API, and
[API usage](api-usage.md) for `curl` examples.
