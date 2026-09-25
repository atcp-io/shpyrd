# RFC-0045 Published binaries, images and CI

**Status:** implemented (with gaps) — see Implementation status below

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** none

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

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
- 2026-09-23: implemented; v0.1.0 is the first release. Notes:
  - GoReleaser 2.x deprecated `brews` for CLIs in favour of `homebrew_casks`; the tap holds
    `Casks/shpyrd.rb`, and `brew install shpyrd-io/tap/shpyrd` resolves it. Casks are macOS
    only, so Linux installs through the script. The cask's quarantine hook uses a stanza
    Homebrew has started to deprecate (`postflight`); it works and GoReleaser will follow.
  - The installer script lives at `https://shpyrd.io/install.sh` (docs site, `public/`)
    instead of a `get.shpyrd.io` host: no DNS to manage. It verifies the SHA-256 from
    `checksums.txt`, installs into `/usr/local/bin` or `~/.local/bin`, honours
    `SHPYRD_VERSION` and `SHPYRD_INSTALL_DIR`.
  - The server image is cross-compiled on the build platform (`--platform=$BUILDPLATFORM`,
    `GOARCH=$TARGETARCH`), so the arm64 image needs no emulation; tags `vX.Y.Z` and
    `latest`. GHCR created the package private; making it public needed the organisation's
    "Package creation: Public" setting and then the package's visibility, both in the UI.
  - `SHPYRD_SERVER_IMAGE` is now derived from the CLI version (`pkg/install.DefaultServerImage`):
    a release tag installs its own image, anything else (`git describe`, `dev`) installs
    `latest`; `--set` overrides as before. The local profile no longer names an image.
  - CI: Go vet and tests, a check that generated CRDs and deepcopy are committed, dashboard
    lint and build, then the kind end-to-end job (cluster create, server from the commit,
    buildpacks and Dockerfile projects answering on their URLs, scale, logs, volumes, teams,
    members). About 10 minutes; green on the first run.
  - Deferred to a follow-up in the same file: cosign keyless signing and SBOMs (the
    pipeline has `id-token` available; add `sbom` and `signs` sections once the first
    releases have settled), the maintenance image of RFC-0020, and Windows binaries.

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Not implemented:** The e2e job deploys `examples/hello` and `examples/api`, not `examples/hello-docker`.
