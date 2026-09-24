# RFC-0059 In-cluster registry as the default on every profile

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0035 (cloud profiles), RFC-0060 (storage classes per profile)

**Creation date:** 2026-09-24

**Last update:** 2026-09-24 (TLS from the platform CA instead of plain HTTP)

## Summary

The registry that holds built application images runs inside the cluster on every profile,
cloud included, serves TLS with a certificate from the platform CA, and a provider registry
(OCIR, ECR, GHCR, ...) becomes the opt-in alternative. shpyrd owns the whole build-to-run
path: no registry account, token rotation or repository visibility to get right before the
first deploy, no image egress costs, and no plaintext image traffic. What kpack lacks
(trusting a private CA) a small shpyrd admission webhook provides; what cloud nodes lack (the
CA in the runtime's trust store) a per-node setup component provides.

## Motivation

The OKE proof of concept (RFC-0035) used OCIR and it worked, but it was the longest part
of the setup: an auth token created in the console, a user name in the
`<namespace>/<user>` form, repositories created private by default and opened for pulls,
a token that propagates in ninety seconds, a Secret the controller mirrors into every
project. Every provider has an equivalent list. The local profile avoids all of it with an
in-cluster registry, but over plain HTTP on a cluster address, which is acceptable on a
laptop and not on a shared cloud network.

### Goals

- `shpyrd cluster init --profile oci --domain ...` needs no registry flags; the first
  deploy builds and runs from the in-cluster registry over TLS.
- Builds (kpack and BuildKit) and node pulls work without changes to project images and
  without any plaintext fallback.
- Storage grows with `--set`, old images are reclaimed, the cluster page shows usage.
- A provider registry stays one flag away (`--registry-host` plus credentials).

### Non-Goals

- Serving images to consumers outside the cluster (CI pulls, `docker pull`); the registry
  is internal. `shpyrd deploy --local-build` keeps working on the local profile.
- Multi-replica registry; it needs object storage (RFC-0046) and comes with it.
- Image signing and provenance (RFC-0044).

## Proposal

- `SHPYRD_REGISTRY=in-cluster|external`, default `in-cluster` on every profile;
  `--registry-host` (with `--registry-user`/`--registry-token-file`) sets `external`. The
  `oci` profile drops OCIR as its default.
- The `registry` component runs on cloud profiles too: one replica, a fixed ClusterIP,
  **TLS only** (no plaintext listener) with a certificate from the platform CA whose SAN is
  that address, HTTP basic auth with one platform credential, a claim on the profile's
  storage class (`SHPYRD_REGISTRY_SIZE`: 20Gi local, 50Gi on `oci`).
- Trust, in four places, all fed by one trust-manager bundle (platform CA plus the public
  roots) that already exists on the local profile:
  1. **kpack build pods** — a mutating webhook served by shpyrd-server mounts the bundle
     into every container of pods labelled `kpack.io/build` and sets `SSL_CERT_FILE`. Every
     step is a Go binary (build-init, the lifecycle, completion) and Go honours that
     variable; kpack itself has no CA support (issue #207, open since 2019).
  2. **kpack controller** — the same bundle and variable through a kustomize patch (it
     assembles and pushes the builder image).
  3. **BuildKit** (Dockerfile builds) — `buildkitd.toml` `[registry."<addr>"] ca=[...]`
     from the mounted bundle; `registry.insecure` goes away.
  4. **Nodes** — the `registry-nodes` DaemonSet writes the CA where the runtime reads it
     (CRI-O `/etc/containers/certs.d/<addr>/ca.crt`, containerd
     `/etc/containerd/certs.d/<addr>/hosts.toml` with `ca`); both are read at pull time, no
     reload. kind nodes use the same DaemonSet, replacing the HTTP hosts.toml written at
     cluster creation.
- Credential: the `registry-credentials` hook generates `shpyrd:<random>` for the
  in-cluster registry and writes the htpasswd Secret plus the dockerconfigjson Secret the
  controller already mirrors (builder service account, `imagePullSecrets`, BuildKit docker
  config — the RFC-0035 plumbing). Project workloads never see it.
- Retention: when the controller prunes a release it deletes that image's manifest; a
  weekly garbage collection reclaims blobs. `shpyrd cluster registry` prints mode, address,
  images and usage; `shpyrd cluster registry gc` runs a collection now.
- Cluster page: a Registry card (in-cluster or the external host, storage used of size,
  certificate expiry, last collection); an alert past 80% usage.

### Alternatives

- **Plain HTTP on a cluster address** (the local profile today, and this RFC's first
  draft). Zero trust configuration, but images and credentials cross the pod network in
  clear and every runtime must be told the registry is "insecure". Rejected.
- **VMware's cert-injection-webhook.** The mechanism kpack maintainers point to and the
  model for ours; but Carvel-only install, a cluster-wide webhook without selector, the CA
  fixed at deploy time (restart to rotate) and `failurePolicy: Ignore`, which turns a
  webhook outage into silent TLS failures. Ours is thirty lines of mutation behind a narrow
  selector.
- **Rebuilding the stack images with the CA.** Covers analyze, restore and export only;
  `prepare` and `completion` run kpack's own images and would need rebuilding too, and
  Paketo ships stack updates every one to four days. Rejected.
- **A hostname instead of an address** (`registry.shpyrd.internal`). Avoids
  go-containerregistry's RFC 1918 heuristics but needs a `/etc/hosts` entry on nodes and a
  CoreDNS entry for pods, and OKE reconciles CoreDNS on Basic clusters. The address with a
  TLS-only listener achieves the same guarantee with fewer parts.
- **Provider registry by default** (status quo on `oci`). Kept as the opt-in.
- **Zot instead of CNCF Distribution.** Attractive (built-in retention, GC without a
  read-only window); a possible swap behind the same component later.

## Design Details

- Address: the Service keeps a fixed ClusterIP. Local stays `10.96.0.50`; elsewhere
  `cluster init` lets Kubernetes allocate one on the first install, records it as
  `SHPYRD_REGISTRY_IP` and renders it fixed afterwards, so an accidental Service re-creation
  cannot change the address every Deployment and certificate references.
- Certificate: cert-manager `Certificate` from the `shpyrd-ca` ClusterIssuer with
  `ipAddresses: [<addr>]`, 90-day duration, renewed automatically; Distribution's `http.tls`
  points at the Secret and reloads on rotation (pod restart by the controller when the
  Secret changes). Go clients verify IP SANs and send no SNI, so the registry presents that
  single certificate.
- Why TLS-only matters: go-containerregistry treats RFC 1918 addresses as
  insecure-eligible and races an https and a plain http attempt, first success wins. With
  no plaintext listener the http attempt fails and only https can succeed; a mis-trusted
  CA fails loudly instead of degrading to HTTP.
- Webhook: `MutatingWebhookConfiguration` with `objectSelector` `kpack.io/build Exists` and
  `namespaceSelector` `shpyrd.io/project Exists`, `failurePolicy: Fail` (only kpack build
  pods are affected by an outage, and kpack retries), serving certificate from cert-manager
  with `cert-manager.io/inject-ca-from` filling the CA bundle. Mutation: a ConfigMap volume
  (the trust bundle in the project namespace), a mount on every init and regular container
  at `/etc/shpyrd/ca/ca-certificates.crt`, `SSL_CERT_FILE` on each. Rebase pods carry the
  same label and are covered.
- Trust bundle: the trust-manager `Bundle` that distributes the platform CA gains
  `useDefaultCAs: true` (so `SSL_CERT_FILE` does not drop the public roots the builds need
  for Docker Hub) and targets project namespaces, `kpack` and `shpyrd-system`.
- `registry-nodes` DaemonSet: privileged, mounts `/etc/containers` and `/etc/containerd`,
  detects the runtime from the sockets present, writes the CA file idempotently (and the
  hosts.toml for containerd with `config_path` set, which OKE, EKS and GKE images do),
  runs as `system-node-critical` so it lands before workloads; an init container does the
  work and a pause container holds the pod. Removes the `insecure` drop-in a previous HTTP
  install may have left.
- shpyrd-server: talks to the registry (manifest deletes, usage) with the bundle mounted;
  the kpack controller patch and the BuildKit toml live in the kpack and shpyrd
  components.
- Garbage collection: a CronJob (`SHPYRD_REGISTRY_GC`: `0 4 * * 0`) sets the registry
  read-only (`REGISTRY_STORAGE_MAINTENANCE_READONLY_ENABLED`, Recreate rollout), runs
  `registry garbage-collect --delete-untagged` in a Job on the same claim, restores
  read-write. Builds started in the window fail with a clear message and `shpyrd deploy`
  retries; the window is minutes for tens of gigabytes.
- Size: `SHPYRD_REGISTRY_SIZE` grows through claim expansion (RFC-0060); shrink refused.
  Usage from `kubelet_volume_stats_*`, already scraped.
- Isolation: with a network policy engine present (RFC-0035), the registry accepts traffic
  only from build pods, shpyrd-server and node addresses; the credential covers clusters
  without one.
- Local profile: the same design replaces plain HTTP; `docker push localhost:30050/...`
  (`--local-build`) needs the platform CA in Docker's `certs.d`, which `shpyrd cluster
  trust` installs alongside the system trust.
- Disabling: `--registry-host` on a later `cluster init` switches to `external`; running
  deployments keep pulling from the in-cluster registry until their next release; the
  component and its data stay until `--set SHPYRD_REGISTRY_KEEP=false`.
- Failure modes, documented: a registry outage blocks builds and any rollout or scale-up
  on a node without the image cached (running instances are unaffected); RFC-0046 adds the
  object-storage backend that makes the registry stateless and multi-replica.

## Open questions

1. In-cluster by default on cloud, provider registry opt-in? Default: yes.
2. Switch the local profile to TLS in the same release (one code path) rather than keeping
   HTTP there? Default: yes; `SHPYRD_REGISTRY_INSECURE` stays one release as an escape hatch
   and is then removed.
3. Weekly automatic garbage collection with a read-only window? Default: yes, Sunday
   04:00 UTC, `SHPYRD_REGISTRY_GC` to change or disable.
4. Object-storage backend (`s3` driver, OCI's S3-compatible API) with RFC-0046? Default:
   RFC-0046 adds `SHPYRD_REGISTRY_STORAGE=bucket`; the claim stays the default.

## Implementation History

- 2026-09-24: RFC written after the OKE proof of concept ran on OCIR (RFC-0035); first
  draft kept plain HTTP, replaced the same day by TLS from the platform CA once the kpack
  trust path (webhook plus `SSL_CERT_FILE`) and the TLS-only requirement were established.
