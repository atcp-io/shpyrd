# shpyrd

**Opensource Cloud PaaS.** Manage applications and agents stack from one
place, from deploy to monitoring: cluster bootstrap, buildpack and Dockerfile
builds, releases and rollbacks, URLs with TLS, config vars, logs and metrics, from one
CLI and one dashboard on top of Kubernetes.

Status: pre-alpha. The design and roadmap live in
[RFC-0001](rfcs/0001-mvp-local-platform.md). Website: https://shpyrd.io

## Quick start (local)

Requirements: Docker (Docker Desktop with 6-8 GB of memory) and Go 1.27 to
build the CLI until binaries are published.

```sh
make cli
./bin/shpyrd cluster create          # kind cluster + base stack (10-20 min first time)
./bin/shpyrd cluster trust-ca        # trust the development CA (asks for sudo)
./bin/shpyrd cluster status
./bin/shpyrd cluster dashboard       # opens https://shpyrd.127.0.0.1.nip.io signed in
```

`cluster create` runs [kind](https://kind.sigs.k8s.io) through its Go library
and installs the base stack in dependency-ordered runlevels:

| Level | Components |
|-------|------------|
| rc0 | Prometheus Operator CRDs |
| rc1 | cert-manager |
| rc2 | development CA `ClusterIssuer`, trust-manager, ingress-nginx (host ports 80/443), in-cluster registry |
| rc3 | kpack (Paketo buildpacks builder), kube-prometheus-stack + Grafana |
| rc4 | shpyrd server (API, controller, dashboard) |

Everything is reachable under a wildcard domain that resolves to your machine
(`127.0.0.1.nip.io` by default, `--domain` to change it):

- `https://shpyrd.127.0.0.1.nip.io` dashboard
- `https://grafana.127.0.0.1.nip.io` Grafana (admin / shpyrd on the local profile)
- `localhost:30050` registry (in-cluster address `10.96.0.50:5000`)

Useful commands:

```sh
shpyrd cluster create --http-port 8080 --https-port 8443   # when 80/443 are taken
shpyrd cluster init --only shpyrd            # re-apply one component
shpyrd cluster init --skip monitoring        # lighter install
shpyrd cluster export -o ./gitops            # render everything for Flux / Argo CD
shpyrd cluster destroy
```

Docker Desktop must use cgroup v2 (the default). With the deprecated cgroup v1
setting enabled the CLI applies a kubelet override and warns.

## Deploying apps

Try the bundled example first (a Go HTTP server rendering an HTML "Hello world"):

```sh
shpyrd projects create hello-world
cd examples/hello && shpyrd deploy  # shpyrd.yaml names the app; ~1 min for the first build
shpyrd open                         # https://hello-world.127.0.0.1.nip.io
shpyrd secrets set GREETING="Olá mundo"   # the page picks it up
shpyrd scale web=3                        # reload: the pod name changes
```

Then your own code:

```sh
cd my-service                       # any repo a Paketo buildpack understands (Go, Node, Java, Python, Ruby, .NET, static) or with a Dockerfile
shpyrd projects create my-service --save   # namespace app-my-service, App resource, shpyrd.yaml
shpyrd deploy                       # archive HEAD, build in-cluster (buildpacks, or the Dockerfile when there is one), roll out
shpyrd deploy --working-tree        # ...or the directory as is, uncommitted changes included
shpyrd open                         # https://my-service.127.0.0.1.nip.io

shpyrd deploy --git https://github.com/org/repo --ref main --path services/api   # build from Git; new commits rebuild (buildpacks)
shpyrd deploy --git https://github.com/org/repo --dockerfile             # ...or build the repository's Dockerfile with BuildKit
shpyrd secrets set DATABASE_URL=postgres://...   # config vars -> new release, rolling restart (values are never shown again)
shpyrd scale web=2 worker=1         # process types come from the buildpack (Procfile / launch.toml)
shpyrd resize web=shared-l          # sizes: shpyrd sizes list (shared = burstable CPU, dedicated = guaranteed)
shpyrd logs -f --process web
shpyrd shell --instance web.2       # bash in a running instance, with the buildpack environment
shpyrd run rails db:migrate         # one-off instance of the current release; exit code passes through
shpyrd releases && shpyrd rollback 2     # re-releases v2: its build and its config vars
shpyrd volumes create data --size 5Gi    # persistent disk; mount with processes.web.volumes: [{name: data, path: /data}]
```

A project is a namespace with its resources; today that is one app resource
(`kubectl get apps -A`) that the controller in `shpyrd-server` turns into kpack
builds or rootless BuildKit Jobs, Deployments, Services and Ingresses with
certificates. Databases, caches and volumes join as further resource types
(see rfcs/0002).

Dockerfile builds (`build.strategy: dockerfile` in `shpyrd.yaml`, detected
automatically for local deploys) support multi-stage targets, build args
(`build.env`), `.dockerignore` and a registry layer cache between builds. Their
images have one entrypoint, so process types other than `web` declare a
`command`; see `examples/hello-docker`.

`shpyrd projects info` and the project page list every resource of the project
(the app, volumes, later databases and caches) with its status and what uses it.
Attaching a resource to the app injects its connection details as read-only
config vars (`<PREFIX>_URL`, ...), recorded in the release like any config
change; the first attachable kinds are Postgres and Redis (rfcs/0009, 0010).

Volumes are persistent disks of a project (`shpyrd volumes create|list|resize|delete`,
a `Volume` resource backed by a PersistentVolumeClaim it owns). A volume is
single-instance by default: the process mounting it runs one instance with
Recreate rollouts, and scaling it up is refused with an explanation. `--shared`
volumes (ReadWriteMany) can be mounted by many instances but need a provisioner
that offers it. Data survives deploys, scaling and crashes; only `volumes
delete` and `projects destroy` remove it (on kind, `cluster destroy` too).

## Dashboard

`shpyrd cluster dashboard` opens the web UI signed in with the admin token
(`shpyrd cluster token` prints it). Apps are listed as projects. From the UI you can create them, deploy
them from a Git repository, scale processes, roll back (build and config), edit
config vars and destroy apps. Per app it shows: metrics modelled on Heroku/Fly
(throughput by status class, p50/p95/p99 response time, instances, CPU and
memory as a percentage of each process' allocation, network, with release
markers), a live build log while building, the
build history, logs from every instance (`web.1`, `worker.2`...) with level
highlighting, filtering and live tail, and the config var names. Config var
values are write-only: they can be added, replaced or removed but never read
back, in the UI or the CLI. The cluster page shows capacity: CPU and memory used
versus reserved by pod requests, per node and in total, plus the installed
components and the available extensions.

## Extensions and sign-in

Optional capabilities are **extensions** compiled into shpyrd and switched on
per cluster (`shpyrd extensions list|enable|disable`; rfcs/0002). Enabling
installs the extension's component with the same runlevel installer and
restarts the server with it; disabling removes the component and is refused
while resources of the extension exist.

The first extension is `auth-local` (rfcs/0007): a bundled [Dex](https://dexidp.io)
issuer at `https://auth.<domain>` with local email/password accounts. The server
is an OpenID Connect relying party (authorization code with PKCE, session
cookie, CSRF protection), so any OIDC provider can follow as configuration.

```sh
shpyrd extensions enable auth-local
shpyrd users add you@example.com          # prompts for the password; also from the Users page
shpyrd users list | passwd | rm
```

The dashboard then offers "Sign in with email and password" next to the admin
token. The token is a shared platform-admin credential meant for bootstrap and
automation: rotate it with `shpyrd cluster token --rotate`, and once accounts
and a platform-admin team exist switch it off with `shpyrd cluster token
--disable` (the API then refuses it). `shpyrd cluster dashboard` never sends
the token to the browser: it mints a one-time, 60-second login ticket in the
cluster that the browser exchanges for a normal session attributed to you.
Wrong tokens are audited and throttled per client.

## Teams, roles and security

Users belong to **teams**; projects grant **roles** to users or teams
(rfcs/0008): `viewer` sees everything and changes nothing, `developer` deploys,
rolls back, scales, edits config vars and opens shells, `admin` also manages
members and resources and can destroy the project. Two platform roles exist:
`platform-admin` (everything, including the cluster page, extensions, teams and
users) and `platform-viewer` (read-only everywhere). The API refuses what the
role cannot do with a plain explanation, the dashboard hides it beforehand, and
a controller mirrors the grants into Kubernetes RBAC (`RoleBinding`s per
project, `ClusterRoleBinding`s for platform roles) so `kubectl` users
authenticated by the same identity provider see the same.

```sh
shpyrd teams create platform --platform-role platform-admin --member you@example.com
shpyrd teams create web --member ada@example.com --group engineering   # groups: your IdP's groups claim
shpyrd members add shop --team web --role developer
shpyrd members add shop --user guest@example.com --role viewer
shpyrd members list shop
```

Until the first team or member exists every signed-in user is a platform admin,
so a fresh cluster stays usable; the dashboard says so. The admin token is
always a platform admin.

Hardening that needs no extension: a `NetworkPolicy` per project (ingress only
from the project itself, the ingress controller and monitoring; egress to the
project, platform namespaces and the internet, never to other projects), the
`restricted` Pod Security Standard in warn/audit mode with app containers run
non-root without capabilities (Dockerfiles need a numeric `USER`), security
headers and a strict CSP on the dashboard, rate-limited sign-in, and an audit
trail of every mutation from the API and the CLI (`{who, what, target, when,
from}` as Kubernetes Events, shown on the project page).

## Instance sizes

Processes run with a named **instance size** from a cluster-wide catalog
(`shpyrd sizes list`, editable with `shpyrd sizes set|delete|default` or on the
dashboard's Cluster page). Two kinds: `shared` sizes get a guaranteed CPU share
that can burst up to 4x; `dedicated` sizes get whole cores with requests equal
to limits. The default catalog goes from `shared-xs` (0.25 CPU, 32 MiB) to
`dedicated-2xl` (16 CPU, 32 GiB); the default size is `shared-s` (0.5 CPU,
64 MiB). Pick one per process with `size:` in `shpyrd.yaml`, `shpyrd resize
web=shared-m`, or the project page; every change is a release. Metrics show CPU
and memory as a percentage of the size.

The API behind the dashboard (`/api/...`) requires the token; only `/api/healthz`,
`/api/config` and content-addressed source archives are public.

## Layout

```
cmd/shpyrd            CLI
cmd/shpyrd-server     in-cluster server (API + App controller + embedded UI)
api/v1alpha1          App CRD types (kubebuilder layout; `make generate`)
internal/controller   App reconciler
pkg/install           runlevel installer: embedded Kustomize + Helm SDK + server-side apply
pkg/kind              kind cluster provisioning
pkg/localca           development root CA
pkg/configvars        config vars (names + metadata, values are write-only)
pkg/ext               extension interfaces; pkg/ext/all the registry; pkg/ext/authlocal the first extension
pkg/authz             roles and actions (RFC-0008); pkg/audit the audit trail
pkg/api               HTTP API, OIDC relying party and sessions, dashboard serving
deploy/               components and profiles embedded in the binary (incl. extension components such as dex)
ui/                   dashboard (Vite + React 19 + Tailwind 4 + shadcn/ui)
examples/hello        example app (Go, web + worker); examples/hello-docker the Dockerfile variant
rfcs/                 design documents
```

## Developing

```sh
make dev-cluster     # cluster with everything except the shpyrd server
make dev-deploy      # build the server image, load it into kind, apply the shpyrd component
make test vet
```

The UI can be developed against a local server: `go run ./cmd/shpyrd-server`
in one terminal, `cd ui && npm run dev` in another (Vite proxies `/api`).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Commits follow
[Conventional Commits](https://www.conventionalcommits.org) and need a DCO
sign-off (`git commit -s`).

## License

[MPL-2.0](LICENSE)
