# KCL: homerun2-schedule-pitcher

Renders the central instance (`config.mode: central`, default) or a cluster
agent (`config.mode: agent`). See [docs/deployment.md](../docs/deployment.md).

```bash
dagger call -m github.com/stuttgart-things/dagger/kcl@v0.134.0 run \
  --source kcl --parameters-file tests/kcl-deploy-profile.yaml \
  export --path /tmp/central.yaml

kcl run kcl/main.k -D config.mode=agent -D config.reportToken=changeme \
  -D 'config.discoveryNamespaces=["flux-system"]'
```

| Option | Default | Mode |
|---|---|---|
| `config.mode` | `central` | both |
| `config.name`, `config.namespace` | `homerun2-schedule-pitcher`, `homerun2` | both |
| `config.image` | `ghcr.io/stuttgart-things/homerun2-schedule-pitcher:main` | both |
| `config.profileYaml` | | both: the SchedulePitcherProfile |
| `config.authToken` | | both: Secret `<name>-token` |
| `config.trustBundleConfigMap` | | both |
| `config.extraEnvVars` | `{}` | both |
| `config.redisAddr`, `config.redisPort`, `config.redisPassword` | `redis-stack.homerun2.svc.cluster.local`, `6379` | central |
| `config.pitcherToken` | | central: Secret `<name>-pitcher` |
| `config.httpRouteEnabled`, `...ParentRefName`, `...ParentRefNamespace`, `...Hostname` | off | central |
| `config.reportToken` | | agent: Secret `<name>-report` |
| `config.agentKind` | `cronjob` (`run`) or `deployment` (`serve`) | agent |
| `config.agentSchedule` | `0 */6 * * *` | agent |
| `config.secretAccess` | `[]` (`{namespace, secretNames}`) | agent: Role with `get` |
| `config.discoveryNamespaces` | `[]` | agent: Role with `get`, `list` |
