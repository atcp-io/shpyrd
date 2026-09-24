# RFC-0059 In-cluster registry as the default on every profile

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0035 (cloud profiles), RFC-0060 (storage classes per profile)

**Creation date:** 2026-09-24

**Last update:** 2026-09-24

## Summary

The registry that holds built application images runs inside the cluster on every profile,
cloud included, and a provider registry (OCIR, ECR, GHCR, ...) becomes the opt-in
alternative. shpyrd owns the whole build-to-run path again: no registry account, token
rotation or repository visibility settings to get right before the first deploy, and no
image egress costs. The registry keeps the plain-HTTP-on-a-cluster-address design that
makes the local profile work without configuration; what cloud nodes lack, a small
per-node setup component provides.

## Motivation

The OKE proof of concept (RFC-0035) used OCIR and it worked, but it was the longest part
of the setup: an auth token created in the console, a user name in the
`<namespace>/<user>` form, repositories that are created private by default and have to be
opened for anonymous pulls, a token that propagates in ninety seconds, and a Secret the
controller mirrors into every project. Every provider has an equivalent list. A PaaS whose
promise is "deploy from one place" should not start with it.

### Goals

- `shpyrd cluster init --profile oci --domain ...` needs no registry flags; the first
  deploy builds and runs from the in-cluster registry.
- Builds (kpack and BuildKit) and node pulls work without changes to project images.
- Storage grows with `--set`, old images are reclaimed, the cluster page shows usage.
- A provider registry stays one flag away (`--registry-host` plus credentials) for teams
  that must keep images in their own registry.

### Non-Goals

- TLS between nodes and the registry (blocked upstream; see Alternatives).
- Serving images to consumers outside the cluster (CI pulls, `docker pull`); the registry
  is internal. Pushing a locally built image (`shpyrd deploy --local-build`) keeps working
  on the local profile only.
- Multi-replica registry; it needs object storage (RFC-0046) and comes with it.

## Proposal

- `SHPYRD_REGISTRY=in-cluster|external`, default `in-cluster` on every profile. `--registry-host
  <host>` (with `--registry-user`/`--registry-token-file` as today) sets `external`; the
  `oci` profile drops OCIR as its default.
- The `registry` component (already used locally) runs on cloud profiles too: one replica,
  a fixed ClusterIP on port 5000, plain HTTP, a claim on the profile's storage class
  (`SHPYRD_REGISTRY_SIZE`: 20Gi local, 50Gi on `oci` where block volumes start there).
- A new `registry-nodes` component (cloud profiles) runs a DaemonSet that tells each node's
  container runtime to pull from that address over HTTP: CRI-O gets a
  `registries.conf.d` drop-in (`insecure = true` for the registry host) and a reload,
  containerd a `certs.d/<host>/hosts.toml`. kind keeps configuring nodes at cluster
  creation, as today. Nodes added later by autoscaling get the DaemonSet pod before any
  project pod is scheduled (system priority class).
- Builds keep working unchanged: go-containerregistry (kpack, the buildpacks lifecycle)
  uses HTTP for RFC 1918 addresses automatically and BuildKit is already told
  `registry.insecure=true` when `SHPYRD_REGISTRY_INSECURE` is set.
- The registry gets one platform credential (basic auth) generated at install by a hook,
  stored as the existing `shpyrd-registry` Secret and delivered by the plumbing that
  RFC-0035 built for OCIR: builder service account, `imagePullSecrets`, BuildKit docker
  config. Project workloads cannot push (they never see the credential) and images stay
  digest-pinned as today.
- Retention: when the controller prunes a release it deletes that image's manifest; a
  weekly garbage collection reclaims the blobs. `shpyrd cluster registry` prints mode,
  address, images and usage; `shpyrd cluster registry gc` runs a collection now.
- Cluster page: a Registry card (in-cluster or the external host, storage used of size,
  last collection); an alert when usage passes 80%.

### Alternatives

- **TLS from the platform CA with a private hostname** (the first idea). kpack has no
  supported way to trust a private CA (issue #207, open since 2019): the controller can be
  patched with `SSL_CERT_DIR`, but build pods need the CA baked into the stack images or
  injected by VMware's cert-injection-webhook. That is a second moving part for every
  build and the runtime still needs per-node configuration. Revisit when kpack ships CA
  support; the node setup component is the same either way, only its drop-in changes.
- **Provider registry by default** (status quo on `oci`). Works, but front-loads the setup
  and ties the platform to each provider's credential model; kept as the opt-in.
- **Harbor or Zot.** Harbor is a platform of its own; Zot is attractive (small, OCI-native,
  built-in GC and retention) and may replace CNCF Distribution later behind the same
  component; nothing in this RFC depends on the implementation.

## Design Details

- Address: the Service keeps a fixed ClusterIP. The local profile stays on `10.96.0.50`;
  elsewhere `cluster init` lets Kubernetes allocate one on the first install, records it as
  `SHPYRD_REGISTRY_IP` and renders it fixed from then on, so an accidental Service
  re-creation cannot change the address every Deployment references.
- `registry-nodes` DaemonSet: privileged, `hostPID`, mounts `/etc/containers` and
  `/etc/containerd`; a shell script detects the runtime from the sockets present, writes
  the drop-in idempotently and reloads (CRI-O re-reads `registries.conf` on SIGHUP;
  containerd reads `certs.d` on the fly when `config_path` is set, which OKE, EKS and GKE
  images do). Runs as a system-node-critical pod so it lands before workloads; runs as an
  init container with a pause main container so it does not hold resources. Verified on
  OKE (CRI-O 1.36, Oracle Linux 8) during implementation; EKS with RFC-0035's AWS work.
- Credential: hook `registry-credentials` extends to generate `shpyrd:<random>` when the
  registry is in-cluster, writes the registry's `htpasswd` Secret and the dockerconfigjson
  Secret the controller already mirrors. Rotation: `shpyrd cluster registry rotate` writes
  a new pair and rolls the registry; deployments re-pull with the mirrored Secret.
- Garbage collection: a CronJob (`SHPYRD_REGISTRY_GC`: `0 4 * * 0`) sets the registry
  read-only (env `REGISTRY_STORAGE_MAINTENANCE_READONLY_ENABLED`, Recreate rollout),
  runs `registry garbage-collect --delete-untagged` in a Job on the same claim, then
  restores read-write. Builds started in the window fail with a clear message and are
  retried by `shpyrd deploy`; the window is minutes for tens of gigabytes.
- Size: `SHPYRD_REGISTRY_SIZE` grows through claim expansion (RFC-0060 says which classes
  allow it); shrink refused. Usage comes from the pod's filesystem metrics
  (`kubelet_volume_stats_*`) already scraped by monitoring.
- Isolation: when the cluster enforces NetworkPolicy (RFC-0035 checks), the registry only
  accepts traffic from build pods, the shpyrd server and node addresses; the credential
  covers clusters that do not.
- Disabling: `--registry-host` on a later `cluster init` switches to `external`; running
  deployments keep pulling from the in-cluster registry until their next release, and the
  component stays until `--set SHPYRD_REGISTRY_KEEP=false` removes it with its data.
- Failure modes, documented: a registry outage blocks builds and any rollout or scale-up
  on a node that does not have the image cached (running instances are unaffected);
  RFC-0046 adds the object-storage backend that makes the registry stateless and
  multi-replica.

## Open questions

1. In-cluster by default on cloud, provider registry opt-in? Default: yes (this RFC).
2. Basic auth for the in-cluster registry (one platform credential) or rely on
   NetworkPolicy only? Default: basic auth; policies as a second layer where enforced.
3. Weekly automatic garbage collection with a read-only window? Default: yes, Sunday
   04:00 UTC, `SHPYRD_REGISTRY_GC` to change or disable.
4. Object-storage backend (`s3` driver, OCI's S3-compatible API) as part of RFC-0046 or a
   later RFC? Default: RFC-0046 adds `SHPYRD_REGISTRY_STORAGE=bucket` when its extension
   is enabled; the claim stays the default.

## Implementation History

- 2026-09-24: RFC written after the OKE proof of concept ran on OCIR (RFC-0035).
