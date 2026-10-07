# Deployment

## Container image

Built with [ko](https://ko.build/) on `cgr.dev/chainguard/static` (no
Dockerfile): `ko build .` or `task build-scan-image-ko`. Releases push
`ghcr.io/stuttgart-things/homerun2-schedule-pitcher:<tag>`.

## Manifests (KCL)

`kcl/` renders either the central instance or an agent, selected with
`config.mode`. Every release publishes both as kustomize OCI artifacts, with
the image pinned to the release version:

| Artifact | From | Contents |
|---|---|---|
| `ghcr.io/stuttgart-things/homerun2-schedule-pitcher-kustomize:<tag>` | `tests/kcl-deploy-profile.yaml` | central instance |
| `ghcr.io/stuttgart-things/homerun2-schedule-pitcher-agent-kustomize:<tag>` | `tests/kcl-agent-artifact.yaml` | agent: CronJob `homerun2-schedule-pitcher-agent`, ServiceAccount, placeholder profile ConfigMap `homerun2-schedule-pitcher-agent-profile` (key `profile.yaml`) and placeholder Secret `homerun2-schedule-pitcher-agent-report` (key `token`); **no Roles** |

Consumers of the agent artifact replace the placeholders and add one `Role` +
`RoleBinding` per namespace the agent reads Secrets in (subject: ServiceAccount
`homerun2-schedule-pitcher-agent`), since those depend on the cluster.

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
