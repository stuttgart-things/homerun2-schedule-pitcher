# homerun2-schedule-pitcher

homerun2 pitcher that runs scheduled checks (token & certificate expiry, probes) and user-defined reminders, and pitches the results into homerun2

## Quick Start

```bash
# Run with Redis
export REDIS_ADDR=localhost REDIS_PORT=6379 REDIS_STREAM=messages AUTH_TOKEN=mysecret
go run .

# Dev mode (no Redis)
PITCHER_MODE=file AUTH_TOKEN=test go run .
```

## API Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/health` | `GET` | None | Health check |
| `/pitch` | `POST` | Bearer token | Submit a message to Redis Streams |

## Architecture

```
HTTP POST /pitch → homerun2-schedule-pitcher → Redis Stream (messages)
```
