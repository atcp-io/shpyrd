# RFC-0045 Published binaries, images and CI

**Status:** implementable

**Owner:** unassigned

**Depends on:** none

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Release builds of the CLI (macOS and Linux, arm64 and amd64) through GoReleaser with a
Homebrew tap and a curl installer, a multi-arch `ghcr.io/shpyrd-io/shpyrd-server` image built
from tags, the installer defaulting to the matching server version, and CI running Go
tests, the UI build and an end-to-end run on kind for every pull request.

## Motivation

Getting started requires Go and `make cli`; the server image referenced by the local
profile (`:latest`) does not exist as a published artifact.

### Goals

- `brew install shpyrd-io/tap/shpyrd` or `curl -fsSL get.shpyrd.io | sh`, then
  `shpyrd cluster create` works with no toolchain.
- A tag produces everything; CI blocks regressions.

### Non-Goals

- Windows binaries (WSL documented).

## Proposal

- GitHub Actions: `ci.yml` (go test, vet, UI lint/build, e2e on kind: create cluster,
  deploy `examples/hello` and `examples/hello-docker`, volumes, roles), `release.yml` on
  tags: GoReleaser (CLI archives, checksums, Homebrew formula in `shpyrd-io/homebrew-tap`),
  Docker buildx multi-arch server image tagged with the version and `latest`, release notes
  from Conventional Commits.
- `pkg/version.Version` from the tag; the local profile's `SHPYRD_SERVER_IMAGE` becomes
  `ghcr.io/shpyrd-io/shpyrd-server:${SHPYRD_VERSION}`; dev builds keep `--set`.
- Installer script served from the docs site (`get.shpyrd.io` → GitHub release assets).
- Maintenance image (RFC-0020) and future helper images built in the same workflow.
- Docs: installation page rewritten around the binaries; `make cli` becomes the developer
  path.

## Design Details

- Signing of our own artifacts with cosign keyless (GitHub OIDC); SBOM via syft.

## Implementation History

- 2026-09-22: RFC written.
