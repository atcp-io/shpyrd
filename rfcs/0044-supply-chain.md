# RFC-0044 Supply chain

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0045

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Components pinned by digest, built images signed with cosign and verified at deploy,
SBOMs retrievable, known CVEs reported on the Cluster page through a Trivy extension.

## Motivation

Production platforms must answer "what exactly is running and is it known-vulnerable".

### Goals

- `shpyrd cluster status` shows image digests and drift from the pinned set.
- Every image built by shpyrd carries a signature and an SBOM; unsigned images are
  refused when the cluster asks for it.

### Non-Goals

- Policy languages (OPA/Kyverno) beyond the signature check.

## Proposal

- Pin: component charts keep versions; images pinned by digest through values where charts
  allow, otherwise documented exceptions; a `make pins` target refreshes digests.
- Sign: kpack signs with cosign (native support, key in a Secret); the BuildKit Job adds a
  `cosign sign` step with the same key; `shpyrd releases` shows "signed".
- Verify: `SHPYRD_REQUIRE_SIGNED_IMAGES=true` makes the App controller verify the release
  digest's signature before rolling out (`cosign verify` in-process); prebuilt `--image`
  deploys need a signature too when enabled.
- SBOM: kpack's SBOM layers exported (`shpyrd releases sbom v12`); BuildKit attests with
  `--attest type=sbom`.
- CVEs: `trivy` extension (Trivy Operator) scanning project images; findings summarised
  per project (critical/high counts) and on the Cluster page.

## Design Details

- Keyless (Fulcio) signing later; a cluster key first.

## Implementation History

- 2026-09-22: RFC written.
