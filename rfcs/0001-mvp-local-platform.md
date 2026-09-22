# RFC-0001 MVP: local platform, App CRD and CLI

**Status:** implemented

**Creation date:** 2026-09-21

**Last update:** 2026-09-21

## Summary

Turn the 2023 proofs of concept under `pocs/` (Flux + Kustomize + kpack on kind and
EKS) into a first working product: a single Go CLI (`shpyrd`) that creates a local
kind cluster, installs the shpyrd base stack in dependency-ordered "runlevels", and
deploys applications from a Git checkout Heroku/fly-style; plus a single in-cluster
binary (`shpyrd-server`) that hosts the API, the `App` controller and the dashboard.

Nothing in the MVP depends on Flux, Terraform or a cloud account. Cloud providers are
introduced later as installer *profiles*.

## Motivation

The website and issue #3 state the goal: "manage the entire application stack from
one place, from deploy to monitoring", with zero config for apps. The POCs proved the
individual pieces (ordered bootstrap, kpack builds, image automation, cert-manager,
ingress-nginx, Prometheus/Grafana) but everything was hand-applied or driven by Flux
against company infrastructure. There is no product surface yet: no CLI, no app model,
no dashboard.

### Goals

1. `shpyrd cluster create` produces a ready local cluster (kind) with the base stack in
   one command, without requiring `kubectl`, `helm`, `kustomize` or `flux` binaries.
2. The base stack is described declaratively in the repo (Kustomize bases + Helm values)
   and selected by a profile (`local` first, `aws` later).
3. `shpyrd deploy` from inside a Git repository builds the app in-cluster with
   buildpacks and exposes it on a TLS URL, with process types (`web`, `worker`),
   secrets, scaling, logs and rollback.
4. A dashboard served from inside the cluster shows apps, releases, logs and basic
   metrics (CPU, memory, requests/s, 5xx, latency) sourced from Prometheus, with
   pre-provisioned Grafana dashboards for web and worker processes.
5. The design leaves the door open to GitOps (export manifests), multiple clusters and
   cloud profiles without rewriting the core.

### Non-Goals

- Multi-cluster management, RBAC/multi-tenancy, OIDC login (Okta) - later.
- Emulating AWS/Cloudflare locally (LocalStack etc.). We abstract what they provide
  instead (see Design Details).
- Heroku-style `git push shpyrd main` (needs a git server); fly-style `shpyrd deploy`
  ships source from the CLI. Can be added later on top of the same build path.
- Dockerfile builds (kpack is buildpacks-only): added in phase B as BuildKit Jobs,
  see RFC-0004.
- Add-ons (Postgres, Redis), autoscaling, cost, log aggregation (Loki) - later phases.

## Proposal

### Decisions (2026-09-21)

| # | Question | Decision |
|---|----------|----------|
| 1 | Install engine | **A**: Go CLI with embedded Kustomize bases + Helm SDK, runlevels executed in Go (not Flux, not an umbrella chart) |
| 2 | Default local domain | `127.0.0.1.nip.io`, overridable with `--domain` |
| 3 | UI component kit | **shadcn/ui** (MIT). The vendored Tailwind UI Catalyst components are removed |
| 4 | App isolation | **Namespace per app** (`app-<name>`) |
| 5 | Controller framework | **kubebuilder** (controller-runtime + controller-gen) |

### User Stories

- As a developer, I run `shpyrd cluster create` on my laptop and get
  `https://shpyrd.127.0.0.1.nip.io` with a certificate my browser trusts.
- As a developer, inside my repo I run `shpyrd apps create myapp && shpyrd deploy` and
  get `https://myapp.127.0.0.1.nip.io` without writing a Dockerfile or YAML.
- As a developer, I run `shpyrd secrets set DATABASE_URL=...`, `shpyrd scale web=2
  worker=1`, `shpyrd logs -f`, `shpyrd releases`, `shpyrd rollback 3`.
- As an operator, I open the dashboard and see every app, its processes, releases,
  request rate and error rate; I click through to Grafana for details.
- As an operator, I later run `shpyrd cluster init --profile aws` against an EKS cluster
  and get the same stack with AWS Load Balancer Controller, ExternalDNS and ACM PCA (the
  `pocs/kust/infrastructure/kinit.d` modules).

### Alternatives

Install engine:

- **Flux as engine** (reuse `pocs/kust` `kinit.d`/`rc*.d` verbatim). Fastest demo,
  `dependsOn`/health/drift for free. Rejected for the product core: five extra
  controllers on a laptop, needs a Git/OCI source, ties the UX to Flux reconciliation,
  and the architecture guide already declares Flux temporary. The engine can export
  its manifests for Flux users.
- **Helm umbrella chart**. Familiar, but Helm cannot wait between subcharts; CRD and
  webhook ordering (cert-manager, prometheus-operator) needs hooks and hacks.
- **Shell scripts**. Not a product.

Build engine:

- **kpack** (chosen): in-cluster, buildpacks, git polling, proven in
  `pocs/flux-poc/apps/base/dummy-go`, used by Cloud Foundry Korifi.
- Local `pack`/Docker in the CLI: kept as `--local-build` fallback (Docker is already
  required by kind).
- Kaniko/BuildKit Job: Dockerfile support, later.
- Shipwright: another operator plus Tekton.

Source delivery:

- **Archive upload** (chosen default): `git archive HEAD` -> API -> served on a
  cluster-internal URL -> kpack `source.blob`. Works for private and local repos.
- `--git <url>`: kpack `source.git`; kpack polls and rebuilds on new commits.
- `git push` remote: later.

## Design Details

### Components and repository layout

```
cmd/shpyrd/                 CLI (cobra)
cmd/shpyrd-server/          API (Gin) + App controller (controller-runtime) + embedded UI
api/v1alpha1/               App types (kubebuilder layout), CRDs generated by controller-gen
internal/controller/        App reconciler
pkg/api/                    HTTP handlers
pkg/install/                runlevel engine: embed FS, kustomize (krusty), Helm SDK, SSA, waits
pkg/kube/                   client factory, kubeconfig discovery
pkg/build/                  kpack helpers, source archive store
deploy/                     embedded manifests (see below)
ui/                         Vite + React + Tailwind + shadcn/ui, built into the server binary
contrib/local-cluster/      kind config and scripts for people without the CLI
```

### Base stack (local profile) as runlevels

Each runlevel is applied fully and waited for (CRDs `Established`, Deployments
`Available`, webhooks answering) before the next starts. The idea and the module names
come from `pocs/kust/infrastructure/{kinit.d,prod/rc*.d}`.

| Level | Component | Mechanism | Notes |
|-------|-----------|-----------|-------|
| rc0 | `monitoring-crds` | Helm (prometheus-community) | Prometheus Operator CRDs first, so later charts can create `ServiceMonitor`s |
| rc1 | `cert-manager` | Helm (jetstack) | CRDs on; the webhook is considered ready when a dry-run `Issuer` is accepted |
| rc2 | `ca-issuers` | Kustomize + hook `local-ca` | Secret `shpyrd-root-ca` from the CLI-generated CA, `ClusterIssuer shpyrd-ca` |
| rc2 | `trust-manager` | Helm + Kustomize | `Bundle shpyrd-ca-bundle` (public roots + shpyrd CA) in every namespace |
| rc2 | `ingress-nginx` | Helm | local profile: `hostPort` 80/443 on the control-plane node; metrics + ServiceMonitor on |
| rc2 | `registry` | Kustomize | `registry:3` on a fixed ClusterIP `10.96.0.50:5000` (see below), NodePort 30050 for the host |
| rc3 | `kpack` | Vendored release + Kustomize | `ClusterStack` jammy, one `ClusterBuildpack` per Paketo family, `ClusterBuilder shpyrd` pushed to the registry |
| rc3 | `monitoring` | Helm + Kustomize | kube-prometheus-stack (alertmanager off, short retention), Grafana dashboards as ConfigMaps |
| rc4 | `shpyrd` | Kustomize | `shpyrd-server` Deployment, RBAC, Service, Ingress `shpyrd.<domain>`, Certificate |

Components inside one runlevel are applied in parallel; the engine waits for the
conditions declared in each `component.yaml` (Deployment available, CRD
established, dry-run accepted by a webhook, status condition true with the
current generation observed) before starting the next level. A full re-run on an
installed cluster is idempotent (Helm upgrade + server-side apply) and takes
about 30 seconds.

Manifests live under `deploy/` and are embedded in the binary:

```
deploy/
  components/<name>/           kustomization.yaml + resources, or values.yaml for Helm charts
  profiles/<profile>/          kustomize overlays and values patches per profile
  profiles/<profile>/profile.yaml   ordered list of components per runlevel
```

Kustomize is rendered in-process (`sigs.k8s.io/kustomize/api/krusty`) from an in-memory
file system populated from `embed.FS`; the result is applied with server-side apply
(field manager `shpyrd`). Helm charts are installed with `helm.sh/helm/v3/pkg/action`
with embedded values. kpack has no chart, so its release YAML is vendored as in the POC.

Variables that differ per installation (domain, cluster name, registry host) are
injected as a small set of `${VAR}` substitutions over the rendered manifests, not as
Kustomize vars.

`shpyrd cluster init --export <dir>` writes the rendered manifests instead of applying
them, so the same source of truth can feed Flux or Argo CD.

### Registry addressing

kpack and the buildpacks lifecycle use go-containerregistry, which only speaks
plain HTTP to `localhost`, `*.localhost`, loopback and **RFC1918 addresses**; the
old `*.local` rule the POCs relied on is gone, and kpack has no insecure-registry
option. The registry Service therefore gets a fixed ClusterIP inside kind's
pinned service subnet (`10.96.0.50:5000`) and everything addresses it by IP: kpack
pushes the builder and app images to it, kind nodes get a containerd
`hosts.toml` for `10.96.0.50:5000` so pulls use HTTP, and the host reaches the same
registry at `localhost:30050`. Cloud profiles will use a managed registry instead.

### Architecture (arm64)

kpack resolves multi-arch images to the node architecture. The prebuilt Paketo
`builder-*` images are amd64-only, so they cannot be used as a buildpack source
on Apple Silicon (buildpacks would run under emulation and pick amd64
toolchains). The builder is assembled from the individual Paketo buildpackages
(`paketobuildpacks/go`, `nodejs`, `java`, `python`, `ruby`, `dotnet-core`,
`web-servers`, `procfile`), which ship amd64 and arm64.

### Local replacements for cloud services

| Cloud (POC) | Local |
|-------------|-------|
| NLB via AWS Load Balancer Controller | kind `extraPortMappings` 80/443 + ingress-nginx `hostPort` on the control-plane |
| Route53 / Cloudflare via ExternalDNS | wildcard magic DNS `*.127.0.0.1.nip.io` (override with `--domain`) |
| ACM Private CA / Let's Encrypt | CLI-generated root CA installed in the OS trust store (`shpyrd cluster trust-ca`) and used by `ClusterIssuer shpyrd-ca` |
| ECR | in-cluster `registry:2` at NodePort 30050 |
| S3 | none in MVP; MinIO later for Loki |
| Okta | static admin token in `~/.shpyrd/config.yaml`; OIDC later |

Not resolving `*.localhost` on macOS from CLI tools and non-routable MetalLB addresses
on Docker Desktop are why hostPort + nip.io were chosen.

### kind cluster

`shpyrd cluster create` uses `sigs.k8s.io/kind` as a library (only Docker required). The
default config is one control-plane (labelled `ingress-ready=true`, ports 80/443/30050
mapped) and one worker, a recent `kindest/node`, and the containerd mirror for the
in-cluster registry. `contrib/local-cluster/kind.yml` mirrors that config for manual use.

### App model

```yaml
apiVersion: shpyrd.io/v1alpha1
kind: App
metadata:
  name: myapp
  namespace: app-myapp          # the App lives with its workloads (owner references work)
spec:
  source:                       # one of git / blob; empty until the first deploy
    blob: { url: http://shpyrd-server.shpyrd-system.svc/api/sources/<sha256>.tgz, sha256: ..., ref: <commit> }
    # git: { url: https://github.com/org/repo, revision: main }
    subPath: services/api
  image: ""                     # set by rollback / --image to run a prebuilt image; cleared by the next deploy
  build: { builder: shpyrd, env: [{ name: BP_GO_TARGETS, value: ./cmd/api }] }
  processes:
    web:    { replicas: 2, port: 8080 }
    worker: { replicas: 1 }
  env: [{ name: LOG_LEVEL, value: info }]   # plain vars; secrets live in Secret myapp-env
  domains: [myapp.127.0.0.1.nip.io]
status:
  phase: Running                # Pending | Building | Deploying | Running | Failed
  image: 10.96.0.50:5000/apps/myapp@sha256:...
  url: https://myapp.127.0.0.1.nip.io
  latestBuild: myapp-build-3
  processes: { web: { desired: 2, ready: 2 }, worker: { desired: 1, ready: 1 } }
  releases:
    - { number: 3, image: ..., source: a1b2c3d4e5f6, configHash: ..., description: "Deploy a1b2c3d4e5f6", createdAt: ... }
  conditions: [Ready, Built]
```

The App lives in the app namespace (`app-<name>`, created by `shpyrd apps create`) rather
than in a central namespace: Kubernetes forbids cross-namespace owner references, and
keeping the App with its workloads gives garbage collection, `Owns()` watches and
per-namespace RBAC for free. `kubectl get apps -A` lists them all.

The controller (controller-runtime, kubebuilder layout under `api/` and
`internal/controller/`) reconciles an `App` into: a kpack `Image` (git or blob source,
volume cache), one Deployment per process type (`command: ["/cnb/process/<type>"]` for
non-web processes; `web` uses the image entrypoint so plain images work too), a Service
per process with a port, an Ingress with a cert-manager annotation for `web`, and
status. Web processes get `PORT=<port>` injected, the Heroku/fly convention
buildpack-built apps expect. A release is appended whenever the deployed image or the
config hash (spec.env + Secret `<app>-env`) changes; the hash is also a pod template
annotation so config changes roll out. Releases carry their configuration, as on
Heroku: when a release is recorded the controller snapshots the config vars into
Secret `<app>-release-v<N>` (pruned with the history); `shpyrd rollback N` (or the
dashboard) sets `spec.image` to release N's build plus a one-shot
`shpyrd.io/rollback-to` annotation, and the controller restores the snapshot before
computing the release, so any previous release can be re-released with its build
*and* its config. The next `shpyrd deploy` unpins the build. Release descriptions say what
changed, Heroku style: "Deploy <commit>", "Set GREETING config var" (diffed against the
previous snapshot), "Rollback to v7"; the API classifies them (deploy, config, rollback)
and links each to the kpack build that produced its image. While a release is building
or rolling out, another rollback is refused (HTTP 409; CLI needs `--force`), and the
rollout message counts instances on the new release ("web 1/3 updated") because
previous instances keep serving until the new ones are ready. Every process gets a
default size (requests 100m/128Mi, limits 1 CPU/512Mi; `cpu`/`memory` per process in
`shpyrd.yaml` override it), which is what makes "CPU 42%" meaningful. Status is
patched with optimistic locking so a reconcile working from a stale cache cannot
overwrite a newer status.

kpack details that shaped the implementation: a kpack `Image` keeps polling git branches
(new commits rebuild automatically), rebuilds when the builder or stack changes (so
uploaded archives must stay available: they live on a PVC), and treats the App labels it
inherits as its own (build pods carry `shpyrd.io/app`; workloads are told apart by
`shpyrd.io/process`).

### CLI surface

```
shpyrd cluster   create | init [--profile local --domain ...] | status | destroy | trust-ca | export
shpyrd projects  create <name> [--save] | list | info <name> | destroy <name>
shpyrd deploy    [--project <name>] [--working-tree] [--git <url> --ref <ref>] [--path <dir>] [--dockerfile [path]] [--image <ref>] [--no-wait]
shpyrd secrets   set K=V ... | unset K | list            (names only; values are write-only)
shpyrd scale     web=2 worker=1
shpyrd resize    web=shared-m worker=dedicated-s          (instance sizes; `shpyrd sizes` manages the catalog)
shpyrd logs      [-f] [--process web] [--tail n] [--build]
shpyrd shell     [--process web | --instance web.2] [-- cmd...]   (RFC-0005)
shpyrd run       [--size shared-l] [--detach] <cmd...>            (RFC-0005)
shpyrd releases  | rollback [n]
shpyrd volumes   create <name> --size 5Gi [--shared] | list | resize | delete   (RFC-0006)
shpyrd extensions list | enable <name> | disable <name>                          (RFC-0002)
shpyrd pg        create <name> [--size --storage --version --instances] | list | info | psql | delete   (RFC-0009)
shpyrd redis     create <name> [--engine --persistent --storage] | list | info | cli | delete          (RFC-0010)
shpyrd attach    <resource> [--kind] [--prefix] | detach <resource>              (RFC-0003)
shpyrd users     add <email> | list | passwd <email> | rm <email>               (RFC-0007, auth-local)
shpyrd teams     create <name> [--member e] [--group g] [--platform-role r] | list | add | remove | delete   (RFC-0008)
shpyrd members   add <project> --user e | --team t --role viewer|developer|admin | list [project] | remove   (RFC-0008)
shpyrd open
```

The CLI talks to the Kubernetes API with the user's kubeconfig (App objects, the env
Secret, pod and build logs), like `flux` or `kubectl argo rollouts`; no CLI-to-server
credential exists yet. The only call that reaches `shpyrd-server` is the source upload,
sent through the API server's service proxy
(`POST .../services/shpyrd-server:http/proxy/api/sources`). The server stores archives
by SHA-256 on its PVC and serves them to kpack at a cluster-internal URL.

`shpyrd deploy` from a checkout archives the committed tree of the current directory
(`git archive HEAD`, which scopes to the subdirectory it runs in), so uncommitted
changes are not deployed; outside a repository the directory is tarred. `--git` builds
from a URL and keeps rebuilding on new commits; `--image` runs a prebuilt image. The
CLI then follows the kpack build step by step (prepare, analyze, detect, restore,
build, export) and the rollout, and prints the release and URL.

`shpyrd.yaml` (optional, written by `apps create --save`) is the fly.toml equivalent:
`app`, `processes` (with ports, commands, pinned replicas), `build.env` (e.g.
`BP_GO_TARGETS` to build several commands) and `domains`. `shpyrd deploy` applies it to
the App; `--working-tree` deploys the directory as is (also chosen automatically when
nothing in it is committed).

### Dashboard

`shpyrd-server` embeds the built UI (`//go:embed`, Vite + React 19 + Tailwind 4 +
shadcn/ui). Pages: apps list with cluster counters; app detail with Overview
(source, current release, per-process replicas with scale buttons, release history
with rollback), Metrics (requests/s, 5xx ratio, p95 latency from ingress-nginx
metrics; CPU and memory for the app namespace; 1h/6h/24h/7d), Logs (streamed from all
pods of a process type, follow mode), Build (latest kpack build steps) and Config
(config var names, plain env, domains), plus a destroy dialog; cluster page with
install info, components, nodes and Helm releases. Grafana is linked for deep dives
with the pre-provisioned "shpyrd / Web apps" dashboard.

The read/act API behind it (`/api/apps...`, `/api/cluster`, `/api/config`) is served by
the same binary; metrics come from a fixed set of PromQL `query_range` calls against
the in-cluster Prometheus (`SHPYRD_PROMETHEUS_URL`), so the browser never talks to
Prometheus directly.

Config vars are write-only, as on Fly: the API and CLI list names and last-update
times (kept in an annotation on the Secret) but never return values; the UI lets you
add, replace (blind), remove and bulk-paste `.env` lines. Image references are never
shown; releases and builds are identified by the image digest. Instance names in logs
follow Heroku (`web.1`, `worker.2`), lines carry kubelet timestamps and are streamed
as NDJSON to the UI. The metrics set follows what Heroku, Fly, Render and Railway
show: throughput stacked by status class, p50/p95/p99 response time, instances (step
chart), CPU and memory per process type as a percentage of the process allocation
(kube-state-metrics exposes the `shpyrd.io/*` pod labels and the container limits for
the join; raw cores/bytes when no limits exist), network in/out, with release markers.
The cluster page shows capacity the way operators think about it: *used* (node-exporter,
joined to node names through `node_uname_info`) versus *reserved* by pod requests
(kube-state-metrics), per node and in total, plus utilisation over time. The dashboard
has light/dark/system themes like the website and lists Apps as "Projects" (a project
without a web process is a worker or an agent; no separate type is needed). Releases
record their process types, and the controller inspects pods per process: instances
that cannot start (CrashLoopBackOff, image pull errors, exec failures) put the App in
`Failed` with the container's reason while previous instances keep serving, which is
what happens when rolling back to a build that predates a process type.

Authentication: a random admin token is generated once by the `admin-token` installer
hook (Secret `shpyrd-system/shpyrd-admin-token`) and injected into the server as
`SHPYRD_ADMIN_TOKEN`. Every `/api` route except `/api/healthz`, `/api/config` and the
content-addressed `/api/sources/<sha>.tgz` (fetched by kpack build pods) requires it,
as `Authorization: Bearer` or `X-Shpyrd-Token` (the API server's service proxy strips
`Authorization`, so the CLI upload uses the latter). `shpyrd cluster token` prints it;
`shpyrd cluster dashboard` opens `https://shpyrd.<domain>/#token=...`: the fragment
never reaches the server, the UI stores it in localStorage and removes it from the
address bar. Access via that URL or the login screen.

### Enabling, disabling, detection

The base stack is installed per cluster by `cluster init` and recorded in a ConfigMap
`shpyrd-system/shpyrd-install` (profile, version, components, timestamps). `cluster
status` reads it and checks component health. Components can be skipped with
`--skip <component>`; `cluster destroy` deletes the kind cluster.

### Drawbacks and known limitations

- kpack builders are large images; first build on a fresh cluster is slow.
- Magic DNS needs Internet access for name resolution.
- kube-prometheus-stack is heavy for a laptop; the local profile trims it.
- When host ports 80/443 are taken, `cluster create --http-port/--https-port` maps
  other ports; URLs then carry the port and ingress-nginx's HTTP to HTTPS redirect
  drops it (use the https URLs directly).
- Docker Desktop configured for cgroup v1 (`DeprecatedCgroupv1`) is deprecated by
  Kubernetes; the CLI detects it and sets `failCgroupV1: false` on the kubelet, but
  cgroup v2 is recommended.

## Milestones

| # | Scope | Done when |
|---|-------|-----------|
| 0 | Cleanup: remove startup Helm install and `os.Exit` in handlers, restructure `cmd/` and `pkg/`, `go mod tidy`, vet clean, redact committed secrets, drop Catalyst | `go vet ./...` clean |
| 1 | `shpyrd cluster create/init/status/destroy/trust-ca`, local profile rc0-rc3 | `https://shpyrd.127.0.0.1.nip.io` serves the UI with a trusted certificate on a fresh kind cluster |
| 2 | `App` CRD + controller, `apps`, `deploy`, `secrets`, `scale`, `logs`, `open`, `releases`, `rollback` | a Go and a Node repo deploy to TLS URLs (done: both via `--git` and from a local checkout) |
| 3 | Dashboard with metrics, Grafana dashboards, `cluster dashboard`, admin token | per-app CPU/mem/RPS/5xx visible in shpyrd UI and Grafana (done) |
| 4+ | `aws` profile, Loki, OIDC, Dockerfile builds, add-ons, autoscale, cost, GitOps export, git-push remote | |

## Implementation History

- 2023-07: POCs under `pocs/` (Terraform on kind, Flux on kind, Flux on EKS, Grafana/Okta).
  Removed from the tree with the MVP; the `pocs/` paths referenced in this document
  were purged from the history together with the credentials they contained.
- 2026-09-21: RFC accepted with decisions 1-5. Milestone 0 done (layout, cleanup,
  secret redaction). Milestone 1 done and verified on kind (Apple Silicon): all
  runlevels ready, dashboard and Grafana served over TLS from the local CA, a
  sample Go app built by kpack from Git, served through ingress and visible in
  Prometheus.
- 2026-09-21: Milestone 2 done: `App` CRD (`shpyrd.io/v1alpha1`) and controller,
  source upload, and the app CLI. Verified on kind: a Go sample deployed with `--git`
  and a Node sample deployed from a local checkout, both at TLS URLs; secrets produced
  a config release; scale, logs, releases and rollback work. Deviation from the draft:
  Apps live in `app-<name>` (not a central namespace) and the CLI uses the kubeconfig
  instead of a server token.
- 2026-09-21: Milestone 3 done: shadcn/ui dashboard (apps, app detail with metrics,
  logs, build, config, scale and rollback; cluster page), the API behind it with
  Prometheus-backed metrics, admin token authentication, `shpyrd cluster token` and
  `shpyrd cluster dashboard`. Verified on kind through the ingress.
