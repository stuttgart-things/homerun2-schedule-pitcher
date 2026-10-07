# CI/CD

## GitHub Actions Workflows

| Workflow | Trigger | Description |
|----------|---------|-------------|
| `build-test` | Push/PR | Dagger lint, unit tests, integration test against Redis (serve, findings ingest, check finding, ack, `run --dry-run`) |
| `build-scan-image` | Push/PR | ko image to ghcr.io (`pr-<n>` tags on PRs), Trivy scan |
| `lint-repo` | Push/PR | Repository linting (YAML, Markdown, secrets) |
| `release` | After the image build on main | semantic-release, GitHub release, kustomize OCI artifacts of the central instance and of the agent, image pinned to the release |
| `pages` | After a release | TechDocs/MkDocs pages |
| `cleanup-pr-artifacts` | PR closed | Deletes the PR image tags |

PR previews (preview ApplicationSet, preview URL comment) are not set up yet.

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
