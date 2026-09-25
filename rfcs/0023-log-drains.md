# RFC-0023 Log drains

**Status:** implemented (with gaps) — see Implementation status below

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** RFC-0022a (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

## Summary

Forward logs to any compatible provider: HTTPS (JSON batches with custom headers) and
syslog over TCP/TLS (RFC 5424). Two scopes: **project drains** owned by a project, and
**cluster drains** set by a platform admin that receive every project's lines labelled
with the project. Configured with `shpyrd drains add`, the project's Drains card and
a Cluster page card; delivery status visible.

## Motivation

Teams keep their logs in Datadog, Better Stack, Axiom, Papertrail or their own ELK; the
platform should stream to them without an agent in the app. Platform teams often run one
central sink (a Loki, a SIEM) for every project — that is the cluster scope, and it is
also how log storage arrives later (RFC-0022b: a cluster drain to Loki plus a query API).

### Goals

- `shpyrd drains add https://in.logs.example.com/ingest --header "Authorization: Bearer ..."
  --project shop` and lines arrive within seconds; several drains per project.
- `shpyrd drains add --cluster syslog+tls://logs.example.com:6514` for every project.
- Both scopes composable: a cluster drain and project drains work together.
- The dashboard configures both: a Drains card on the project page (project members with
  the `resource` role) and a Log drains card on the Cluster page (platform admins).
- Delivery status visible (last success, errors) on the object and in the cards.

### Non-Goals

- Provider-specific dashboards or metrics forwarding (RFC-0029 covers OTLP).
- Filtering by log level or field at the drain (Vector VRL filters are a follow-on).

## Proposal

### The object

`LogDrain` (`shpyrd.io/v1alpha1`):

```yaml
apiVersion: shpyrd.io/v1alpha1
kind: LogDrain
metadata:
  name: datadog
  namespace: app-shop          # project drain; shpyrd-system for a cluster drain
spec:
  url: https://http-intake.logs.datadoghq.com/api/v2/logs
  format: json                 # json (HTTPS) | syslog (TCP, TLS when the URL is syslog+tls://)
  headersFrom:                 # optional: Secret with header values (API keys)
    name: datadog-headers      # keys become header names: DD-API-KEY: <value>
  processes: [web]             # optional: only these process types
status:
  phase: Active | Failing | Pending
  message: "last delivery 3s ago" | "connection refused"
  lastDeliveryAt: <time>
```

- **Scope by namespace**: a drain in `app-<slug>` receives that project's lines; a drain in
  `shpyrd-system` receives every project's lines. No spec field decides scope.
- **Headers** live in a Secret in the same namespace (write-only like config vars); the
  drain object only references it. The CLI writes the Secret from `--header` flags.
- **Format**: `json` sends one JSON object per line (`{time, project, process, instance,
  stream, level, msg, fields}`) in batches over HTTPS. `syslog` sends RFC 5424 with the
  project as APP-NAME, the instance as PROCID and the message as MSG, over TCP; `syslog+tls://`
  wraps it in TLS.

### Rendering into Vector

A `LogDrainReconciler` watches `LogDrain` objects (all namespaces) and their Secrets and
renders a second Vector config file, `drains.yaml`, into ConfigMap `vector-drains` in
`logs-system`:

```yaml
transforms:
  drain_app-shop_datadog:
    type: filter
    inputs: [shpyrd_enrich]
    condition: .project == "shop" && includes(["web"], .process)
sinks:
  drain_app-shop_datadog_sink:
    type: http
    inputs: [drain_app-shop_datadog]
    uri: https://http-intake.logs.datadoghq.com/api/v2/logs
    encoding: { codec: json }
    framing: { method: newline_delimited }
    request:
      headers: { DD-API-KEY: "${DRAIN_APP_SHOP_DATADOG_DD_API_KEY}" }
```

Header values are injected as environment variables into the Vector DaemonSet from the
referenced Secrets (Vector expands `${VAR}` in its config), so the rendered ConfigMap
never contains a secret. The DaemonSet gets one `envFrom` per header Secret; adding a
drain with a new Secret rolls the DaemonSet once, editing the URL only reloads.

Vector runs with `--config vector.yaml --config drains.yaml --watch-config`, so a URL or
filter change is a hot reload with no pod restart. Cluster drains use `inputs:
[shpyrd_enrich]` without a project condition.

### Status

Vector exposes per-component metrics (`component_sent_events_total`, `component_errors_total`)
on its API. The reconciler polls them every 30 s per drain and writes `status.phase`
(`Active` when events flowed since the last poll, `Failing` when errors grew and nothing
was sent, `Pending` before the first event), `status.message` and `status.lastDeliveryAt`.
Ten consecutive failing polls record an audit event `drain.failing`.

### CLI

```
shpyrd drains add <url> [--name datadog] [--header "Key: value"]... [--processes web,worker] [--project shop | --cluster]
shpyrd drains list [--project shop | --cluster]
shpyrd drains remove <name> [--project shop | --cluster]
```

The name defaults to the URL's host. `--cluster` needs the platform-admin role (the CLI
uses the kubeconfig; the API enforces `cluster.admin`).

### API

- Project: `GET/POST /api/projects/{slug}/drains`, `DELETE /api/projects/{slug}/drains/{name}`
  (`project.resource`).
- Cluster: `GET/POST /api/drains`, `DELETE /api/drains/{name}` (`cluster.admin`).
- The drain view carries `name, url, format, processes, headers (names only), status`.

### Dashboard

- Project page: a **Log drains** card (below Resources) listing drains with status dots and a
  form: URL, format, headers (name + value, value write-only), process filter.
- Cluster page: a **Log drains** card for cluster-wide drains, same form, with the note
  that every project's lines go there.
- Requires the `logs-agent` extension: the cards say so and link to the docs when it is off.

## Design Details

- `LogDrainReconciler` in `internal/controller/logdrain_controller.go`: `For(&LogDrain{})`,
  watches Secrets referenced by drains, single rendering function `renderDrains(drains) string`
  covered by unit tests against the Vector schema.
- The reconciler owns ConfigMap `vector-drains` and patches the DaemonSet's `envFrom` list;
  both live in `logs-system`, so the reconciler needs `configmaps` write and `daemonsets`
  patch there (RBAC added to the shpyrd-server ClusterRole).
- Vector env var names: `DRAIN_<NAMESPACE>_<NAME>_<HEADER>` with dashes and dots mapped to
  underscores, uppercased.
- URL validation: `https://` or `http://` (json), `syslog://` or `syslog+tls://` with a port
  (syslog). Private/loopback addresses are allowed (an in-cluster Loki is a valid target).
- Network policy: `logs-system` egress is open to the internet and to platform namespaces
  (the drains need to reach their targets); project pods still have no log egress.

## Settled questions

1. Scope: both project and cluster drains, decided by the object's namespace.
2. Providers verified: an HTTPS JSON receiver (a `httpbin`-style echo in the cluster) and a
   syslog receiver (a `rsyslog`/`nc` pod) in the e2e; real providers (Datadog, Better Stack)
   are configuration and documented.
3. Header secrets never enter the rendered config: env var indirection.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-23: rewritten for both scopes and the dashboard; moved to implementable.
- 2026-09-23: implemented in shpyrd-io/shpyrd and verified against an HTTP echo and a syslog
  receiver in the cluster: project drain filtered to shop/web with the header delivered,
  cluster drain receiving every project as RFC 5424, an unreachable receiver reported as
  Failing with a DrainFailing event, removal pruning config and header mirrors. Notes:
  - `envFrom` only references Secrets of the pod's namespace, so the controller mirrors
    each header Secret into `logs-system` (`drain-<ns>-<name>-headers`, label
    `shpyrd.io/drain-headers`) with header names sanitized into valid env var names
    (`DD-API-KEY` → `DD_API_KEY`); mirrors of removed drains are pruned.
  - Vector 0.58 gates env interpolation behind `--dangerously-allow-env-var-interpolation`;
    the agent runs with it. The rendered ConfigMap carries `${ENV}` references only.
  - Delivery status comes from Vector's `prometheus_exporter` (port 9598, Service `vector`),
    polled every 30 s; counts are per node (one pod answers) and approximate.
  - Build pods and the pods of attached resources have no `shpyrd.io/process` label and are
    filtered out before the drains (`shpyrd_enrich` is now a filter over `shpyrd_enrich_raw`).
  - Syslog uses Vector's `socket` sink with a VRL remap composing the RFC 5424 line; TLS when
    the URL is `syslog+tls://`.

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Not implemented:** The `drain.failing` audit event after ten failing polls (a Kubernetes warning event is emitted on every failure); a docs link on the disabled-agent notice.
