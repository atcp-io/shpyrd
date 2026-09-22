# RFC-0003 Projects and resources

**Status:** implemented (resources list, bindings plumbing); attach/detach CLI arrives with the first bindable kind

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A **project** is what you deploy to: a namespace with a domain, config vars, membership and
a set of **resources**. Today the only resource type is the app (code with process types);
this RFC makes the project a first-class container so databases, caches and volumes
(RFC-0006, RFC-0009, RFC-0010) join it as further resource types, and defines **bindings**:
attaching a resource to an app injects its connection details as config vars, Heroku style.

## Motivation

One codebase per project was enough for the MVP. Real projects have a database and a cache
next to the code, sometimes several services. Users already think in projects (the
dashboard lists them); the platform should too, without renaming the working parts.

### Goals

- Several resources per project, listed and managed on the project page.
- Attach/detach resources to apps; the app sees plain config vars.
- Keep today's one-app project working unchanged (`shpyrd deploy` needs no new flag).
- Everything remains Kubernetes objects in the project namespace, visible to `kubectl`.

### Non-Goals

- Multiple environments per project (staging/production); a project is one environment,
  environments are separate projects for now.
- Cross-project bindings.

## Proposal

### Model

```
Project (namespace app-<name>)          labels: shpyrd.io/project=<name>
├── App        <name>                   code, processes, releases   (exists today)
├── Postgres   db                       RFC-0009
├── Redis      cache                    RFC-0010
└── Volume     data                     RFC-0006
```

The namespace **is** the project; a `Project` object in `shpyrd-system` is added only
when it needs its own spec (owning team, quotas, default size) in RFC-0008. `shpyrd
projects create` keeps creating the namespace and the app resource; `shpyrd projects
info` lists every resource in it.

Resource CRDs (`shpyrd.io/v1alpha1`) share a status shape so the dashboard can render any
of them: `phase` (Pending, Provisioning, Ready, Failed, Deleting), `message`, `conditions`,
and `endpoint` (host:port) where it applies.

### Bindings

```yaml
# App spec
bindings:
  - kind: Postgres
    name: db
    prefix: DATABASE      # optional; default from the resource type
```

The App controller asks each bound resource (through the `Binding` interface of RFC-0002)
for its config vars (`DATABASE_URL`, `PGHOST`, ...) and merges them with the project's own
config vars when rendering the process environment. Bound vars are read-only in the Config
tab (shown with the resource that provides them) and take part in the release
fingerprint, so attaching a database is a `config` release ("Attach db") that can be
rolled back like any other. Detaching removes them.

CLI: `shpyrd attach db` / `shpyrd detach db` (the kind is inferred from the name within the
project); dashboard: "Attach" on the resource, with the resulting var names shown before
confirming.

### Project page

- Header: project name, domain, phase summary.
- **Resources** list: type, name, status, endpoint, attached-to; "Add resource" opens the
  type's form (from its JSON schema) when its extension is enabled.
- The app keeps its tabs (overview, metrics, logs, builds, config).

### Deleting

Destroying a project deletes the namespace and everything in it; the dialog lists the
resources that will go, and resources holding data (Postgres, Volume) require typing the
project name (already the case) and are listed first. Deleting a single resource that is
still bound is refused until detached.

## Design Details

- The `Binding` config vars are produced into a Secret `<app>-bindings` owned by the App,
  mounted with `envFrom` after `<app>-env` (bound vars win on conflicts, and the Config tab
  says so). Its content is part of `configHash`.
- Resource controllers set `status.endpoint` and a `Secret` with credentials in the project
  namespace; the App controller never reads provider CRs directly, only the shpyrd resource
  and its `Binding` implementation.
- `shpyrd projects info` and `GET /api/apps/{ns}/{name}` grow a `resources` list; a
  generic `GET /api/projects/{name}/resources` serves the dashboard.

### Drawbacks

- Two levels (project → resources) add a click where the MVP had one; the app stays the
  default landing so single-app projects feel unchanged.

## Implementation History

- 2026-09-22: RFC written. Volumes (RFC-0006) are the first non-app resource type.
- 2026-09-22: Implemented the model: namespaces carry `shpyrd.io/project`, `GET
  /api/projects/{ns}/resources` and `shpyrd projects info` list every resource in the
  shared shape (kind, phase, message, endpoint, details, attachedTo, data), the dashboard
  project page has a Resources card (volumes created and resized there) and the destroy
  dialog lists what goes, data-holding resources first. Bindings: `spec.bindings` on the
  App, a `Binder` registry per kind in the controller, the Secret `<app>-bindings` (owned by
  the App, `envFrom` after `<app>-env` so bound vars win, provider recorded per variable,
  part of `configHash`), releases record their bindings and rollback restores them, the
  Config tab and `shpyrd secrets list` show bound vars read-only with their provider. No
  kind registers a Binder yet; `shpyrd attach|detach` ship with Postgres (RFC-0009).
