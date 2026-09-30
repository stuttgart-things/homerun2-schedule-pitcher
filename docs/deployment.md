# Deployment

## Container Image

Built with [ko](https://ko.build/) using a distroless base image (`cgr.dev/chainguard/static:latest`):

```bash
# Build locally
ko build .

# Build via Taskfile
task build-scan-image-ko
```

## Flux App Deployment

The recommended way to deploy the full homerun2 stack is via the [homerun2 Flux app](https://github.com/stuttgart-things/flux/tree/main/apps/homerun2). It uses Kustomize Components to deploy Redis Stack + all homerun2 microservices into a shared namespace.

See the [Flux app README](https://github.com/stuttgart-things/flux/tree/main/apps/homerun2) for all substitution variables and a complete cluster example.

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
