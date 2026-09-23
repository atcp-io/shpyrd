# RFC-0002 Extension model

**Status:** implemented (framework); schema-driven forms → RFC-0028

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Keep the shpyrd core small (installer, App controller, server, CLI, dashboard) and add
optional platform capabilities as **extensions**: Go packages compiled into the binaries,
switched on per cluster, each contributing an installer component, resource types with
controllers, server hooks, CLI commands and dashboard panels through a handful of small
interfaces. Authentication, storage, databases, caches and shell access are the first
extensions (RFC-0004 to RFC-0010).

## Motivation

Real workloads need a database, a cache, a disk, a shell and more than one user with
different rights. Putting every capability in the core makes shpyrd a monolith nobody can
review; making every capability a separate deployable makes it a distributed system
nobody wants to operate. Extensions in the same binaries with a strict interface give the
middle ground: one release, one test suite, opt-in per cluster.

### Goals

- One mechanism for optional capabilities, used by shpyrd's own addons first.
- Enable, upgrade and disable per cluster with the runlevel installer's guarantees
  (ordering, readiness waits, install record).
- Resource types that the dashboard and CLI can render generically, with per-type extras.
- Interfaces small enough that an out-of-process adapter could implement them later.

### Non-Goals

- Third-party plugins loaded at runtime, marketplaces, remote registries (revisit when a
  third party asks; the interfaces are designed not to preclude it).
- Extensions modifying the core's behaviour through hooks scattered across the codebase.

## Proposal

### Options considered

| Option | Pros | Cons |
| --- | --- | --- |
| **In-tree extensions** (chosen): Go packages behind interfaces, compiled in, enabled by configuration | one binary, one release cycle, easy testing, shared controller runtime and UI | third parties must upstream; disabled extensions still ship in the binary (dormant) |
| Out-of-process plugins (hashicorp/go-plugin, gRPC) | isolation, independent releases | protocol to version, harder UI integration, more processes to run |
| Heroku add-on provider protocol (HTTP provision/deprovision) | proven, language agnostic | designed for hosted SaaS; for in-cluster resources a controller is the natural provider. Kept as the model for **bindings** (RFC-0003) |

### What an extension contributes

| Contribution | Example | Core mechanism reused |
| --- | --- | --- |
| Installer component | CloudNativePG operator, Dex, an NFS provisioner | `deploy/components/<ext>` appended to the profile's runlevels; installed and waited for like cert-manager |
| Resource type | `Postgres`, `Redis`, `Volume` CRDs and reconcilers | controller-runtime reconcilers in `shpyrd-server`, next to the App controller |
| Binding | `DATABASE_URL`, `REDIS_URL` | the App controller merges the config vars a bound resource exposes into the process environment (RFC-0003) |
| Server hooks | authentication provider, `/api/pg/*` routes, audit sink | Gin route groups, small Go interfaces |
| CLI and UI | `shpyrd pg psql`, a "Databases" panel | cobra subcommands; the dashboard learns enabled extensions from `GET /api/config` and each resource type publishes a JSON schema for its create form |

### Lifecycle

- `shpyrd cluster init --enable postgres,shell` (or `shpyrd extensions enable postgres`)
  records the choice in the install record, adds the component to the runlevels and
  applies it. Enabling is idempotent; upgrading is re-running.
- `shpyrd extensions disable postgres` refuses while resources of its types exist ("3
  Postgres in use"), then removes the component.
- Each extension gets its own ServiceAccount and RBAC scoped to its CRDs and namespaces,
  so the core's permissions do not grow with every addon.
- The dashboard's Cluster page lists extensions with health, like components today.

## Design Details

```go
// pkg/ext
type Extension interface {
    Name() string
    Description() string
    Component() *install.Component          // installer component, nil when none
    Register(mgr ctrl.Manager) error        // reconcilers for resource types
    Routes(api gin.IRouter, deps Deps)      // extra API routes
    CLI(g *cli.Globals) []*cobra.Command    // extra CLI commands
    Types() []ResourceType                  // resource kinds this extension owns
}

type ResourceType struct {
    Kind        string          // "Postgres"
    Plural      string          // "postgres"
    Schema      json.RawMessage // JSON schema for the create form
    Bindable    bool            // can be attached to an App (RFC-0003)
}

type Binding interface {                     // implemented by bindable resource reconcilers
    ConfigVars(ctx context.Context, ref ResourceRef) (map[string]string, error)
    Ready(ctx context.Context, ref ResourceRef) (bool, string, error)
}
```

Registration is a static table (`ext.All()`); enabling is data (the install record), not a
build tag, so one binary serves every cluster. Resource CRDs share a status shape (phase,
message, conditions, endpoint) so the dashboard renders them generically.

### Drawbacks

- Binaries grow with each extension (reconcilers only; operators stay separate images).
- Interfaces in the core must stay stable once extensions depend on them.

## Implementation History

- 2026-09-22: RFC written (split from the earlier extensions draft). Phase C in RFC-0007 introduces `pkg/ext` with the first extension (local authentication).
- 2026-09-22: Implemented `pkg/ext` (`Extension`, `ComponentRef`, `ResourceType`, `Router`,
  `Deps`, `AuthRegistry`/`OIDCProvider`, `Identity`), the static registry `pkg/ext/all`, and
  the lifecycle: the installer appends enabled extensions' components (named directories
  under `deploy/components`) to their runlevel, derives `SHPYRD_EXTENSIONS`,
  `SHPYRD_DASHBOARD_URL` and `SHPYRD_AUTH_URL`, records the set in the install record so
  `cluster init` and `cluster status` keep it; `shpyrd extensions list|enable|disable` and
  `cluster init --enable`; `Engine.Remove` deletes a component (Kustomize objects, Helm
  release, record key). The server mounts extension routes, hands them `Deps`, registers
  their controllers and lists them in `/api/config` and on the Cluster page; the CLI adds
  their commands. Differences from the sketch: `Component()` returns a name/runlevel
  reference instead of a full component (the installer resolves it from the embedded tree),
  extension RBAC ships inside the component manifests (Dex grants the server access to
  `passwords`), and the per-type JSON schema form is not built yet.
