# API usage

All `/api/*` endpoints and `POST /findings` need `Authorization: Bearer $AUTH_TOKEN`.

## Report findings (cron job, Ansible, other jobs)

One call per run with the **complete** set of the source. Missing keys are
resolved, an empty list resolves everything of the source.

```bash
curl -sf -X POST https://schedule-pitcher.example/findings \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"source":"disk-dev4","run":"202610060400","findings":[
        {"key":"dev4-vm/disk:/var","title":"/var at 74%","severity":"info",
         "value":74,"threshold":70,"host":"dev4-vm","tags":["disk"]}]}'
```

## Read and acknowledge findings

```bash
curl -s "https://schedule-pitcher.example/api/findings?status=open" -H "Authorization: Bearer $TOKEN"

curl -s -X POST https://schedule-pitcher.example/api/findings/ack -H "Authorization: Bearer $TOKEN" \
  -d '{"source":"disk-dev4","key":"dev4-vm/disk:/var","by":"patrick.hermann","note":"cleanup running"}'
```

## Checks

```bash
curl -s https://schedule-pitcher.example/api/checks -H "Authorization: Bearer $TOKEN"
curl -s -X POST https://schedule-pitcher.example/api/checks/omni-platform-tls/run -H "Authorization: Bearer $TOKEN"
curl -s "https://schedule-pitcher.example/api/checks/omni-platform-tls/history?limit=5" -H "Authorization: Bearer $TOKEN"
```
