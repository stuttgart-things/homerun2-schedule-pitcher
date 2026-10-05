# Deployment

## Container image

Built with [ko](https://ko.build/) on `cgr.dev/chainguard/static` (no
Dockerfile): `ko build .` or `task build-scan-image-ko`. Releases push
`ghcr.io/stuttgart-things/homerun2-schedule-pitcher:<tag>`.

## Manifests (KCL)

`kcl/` renders either the central instance or an agent, selected with
`config.mode`. The release publishes the central variant
(`tests/kcl-deploy-profile.yaml`) as the kustomize OCI artifact
`ghcr.io/stuttgart-things/homerun2-schedule-pitcher-kustomize`.

```bash
dagger call -m github.com/stuttgart-things/dagger/kcl@v0.134.0 run \
  --source kcl --parameters-file tests/kcl-agent-profile.yaml \
  export --path /tmp/agent.yaml
```

| Mode | Renders |
|---|---|
| `central` | ServiceAccount, profile ConfigMap, Deployment (`serve`), Service, optional HTTPRoute; env `REDIS_*`, `AUTH_TOKEN`, `PITCHER_TOKEN` |
| `agent` | ServiceAccount, profile ConfigMap, CronJob (`run`, default) or Deployment (`serve`), one `Role` + `RoleBinding` per namespace it reads Secrets in; env `REPORT_TOKEN` |

Secrets are referenced as `<name>-token` (`auth-token`), `<name>-redis`
(`password`), `<name>-pitcher` (`token`) and `<name>-report` (`token`). The
module only emits them when a value is given (`config.authToken`, ...); in
GitOps they come from SOPS or ESO instead.

Agent RBAC: `config.secretAccess` grants `get`, restricted by `secretNames`;
`config.discoveryNamespaces` grants `get` and `list`. `list` returns Secret
data in Kubernetes, so keep discovery namespaces narrow.

`config.trustBundleConfigMap` mounts `trust-bundle.pem` (e.g. the vault PKI
CA) and adds it to the system roots via `SSL_CERT_DIR`.

## Placement

- **Central** on platform, next to homerun2, `redis-stack` and omni-pitcher.
- **Agents** on every watched cluster, reporting to the central
  `https://<central>/findings` with a token of the central `AUTH_TOKEN`.

## Testing

```bash
go test ./...            # unit tests, no Redis or cluster
task build-test-binary   # Dagger: unit tests + integration test with Redis
task lint
task build-scan-image-ko
```
