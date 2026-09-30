# CI/CD

## GitHub Actions Workflows

| Workflow | Trigger | Description |
|----------|---------|-------------|
| `build-test` | Push/PR | Lint + unit tests via Dagger |
| `build-scan-image` | Push to main | Build container image with ko, push to ghcr.io, scan with Trivy |
| `release` | After image build | semantic-release: changelog, GitHub release, kustomize OCI push |

## Release Process

Releases are fully automated via [semantic-release](https://semantic-release.gitbook.io/):

- `fix:` commits trigger a **patch** bump
- `feat:` commits trigger a **minor** bump
- Each release publishes the container image and kustomize OCI artifact to `ghcr.io`

## Taskfile Commands

```bash
task lint                  # Run Go linter
task build-test-binary     # Build + test with Redis via Dagger
task build-scan-image-ko   # Build, push, scan container image
task build-output-binary   # Build Go binary
```
