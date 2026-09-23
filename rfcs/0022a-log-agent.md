# RFC-0022a Log agent (Vector, container log limits)

**Status:** implemented

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** RFC-0001 (implemented)

**Creation date:** 2026-09-23

**Last update:** 2026-09-23

## Summary

A Vector DaemonSet that reads container logs from every node, attaches
`project`, `process`, `instance` labels from pod annotations (no per-line
API calls), enforces per-container log rotation through the kubelet config
so a high-throughput app cannot fill the node disk, and feeds log drains
(RFC-0023) and future storage (RFC-0022b). The current live stream from
the Kubernetes API keeps working unchanged; the agent is an additional
data path, not a replacement.

## Motivation

- A pod with no drain today can fill the node's ephemeral storage: containerd
  has no `max_container_log_size` in the default kind profile.
- Log lines are lost on pod restart or rollout; `shpyrd logs` can only tail
  running pods.
- RFC-0023 (drains to Datadog, Betterstack, syslog) needs an agent that has
  already parsed and labelled the lines.

### Goals

- `max_container_log_size: 10Mi` (2 files) applied to all project pods;
  the live stream is unaffected (the file is still there while the container runs).
- Vector reads `/var/log/pods/<namespace>/<pod>/<container>/*.log`, filters to
  `app-*` namespaces, enriches with `shpyrd.io/*` pod annotations and labels,
  and emits structured lines `{time, project, process, instance, stream, message}`.
- Enabling the extension: `shpyrd extensions enable logs-agent`.
- RFC-0023 drains are sinks in the Vector config, rendered by the controller
  from `LogDrain` objects.

### Non-Goals

- Replacing the live Kubernetes-API log stream (stays as the primary path).
- Log storage / `--since` (RFC-0022b, depends on RFC-0046).
- Log drains themselves (RFC-0023, depends on this RFC).
- Build logs and run logs (same agent reads them; routing to drains: RFC-0023).

## Proposal

### Container log rotation (applied immediately on `cluster init`)

The kubelet `containerLogMaxSize` and `containerLogMaxFiles` settings bound
the on-disk log per container. Applied via a `KubeletConfiguration` patch in
the kind cluster profile (and documented for non-kind clusters as a kubelet
flag or a `KubeletConfiguration` manifest):

```yaml
containerLogMaxSize: "10Mi"
containerLogMaxFiles: 2
```

This is 20 MiB per container at most. High-throughput apps should use drains.
The setting does not affect the running `docker logs` / `kubectl logs` stream.

### Instance annotation

The App controller annotates each pod it creates with `shpyrd.io/instance:
web.1` (the stable name from `InstanceNames`) so Vector can label log lines
without an API call per line. The annotation is set in `desired.go`'s
`mutateDeployment` and `runpods.go`.

### Vector DaemonSet

- Image: `timberio/vector:0.58.0-alpine`
- Source: `kubernetes_logs` (reads from the node's log dir, uses the Kubernetes
  API to enrich pods with labels/annotations)
- Remap: parse the `shpyrd.io/*` annotations, derive `project`/`process`/`instance`;
  drop lines from non-`app-*` namespaces (builds and platform pods stay on the
  Kubernetes API stream for now); parse the timestamp; detect JSON log lines and
  promote their `level`/`msg` fields.
- Sink (initial): `console` + `blackhole` (until RFC-0023 adds drain sinks).
  The console sink lets `kubectl logs -n logs-system deploy/vector` verify the
  pipeline during development; it is turned off in production configs.
- RBAC: `get`, `list`, `watch` on pods and namespaces (same as most log agents).
- Namespace: `logs-system`.
- Network policy: Vector only egresses to drain URLs (empty list until RFC-0023)
  and to the Kubernetes API server; project pods still have no log egress.
- Resource requests: 50m CPU, 128Mi memory.

### Extension

`logs-agent` extension: no API routes, no CLI commands beyond the standard
`shpyrd extensions enable/disable`. The Vector ConfigMap is rendered from the
install vars (domain, system namespace) and re-applied on `cluster init`.

## Design Details

- The `shpyrd.io/instance` annotation is written alongside the existing
  `shpyrd.io/config-hash` annotation on the pod template; it does not change the
  config hash (not an env input).
- Vector's `kubernetes_logs` source already uses the node's local file, not the
  API, for the actual log bytes; it only calls the API for pod metadata
  (annotations, labels). That metadata is cached, so there is no per-line
  API call.
- Log rotation: `containerLogMaxSize`/`containerLogMaxFiles` are kubelet
  settings. On kind, they go into the `KubeletConfiguration` patch in
  `deploy/profiles/local/kind-config.yaml` (rendered by `pkg/kind`). On an
  existing cluster the operator sets them via the kubelet config or a node
  configuration operator; the docs note this.
- The Vector config is a single ConfigMap in `logs-system`; drain sinks (RFC-0023)
  are appended by the controller rendering `LogDrain` objects into additional
  `transforms` and `sinks` stanzas.

## Implementation History

- 2026-09-23: RFC written as a split of RFC-0022 and implemented in shpyrd-io/shpyrd. Notes:
  - kubelet containerLogMaxSize/containerLogMaxFiles (10Mi/2) added to all kind nodes.
  - labelRunningInstances reconciler patches running pods with shpyrd.io/instance annotation after each reconcile; InstanceNames moved to pkg/logs to avoid import cycle.
  - logs-agent extension: Vector 0.58.0-alpine DaemonSet in logs-system; kubernetes_logs source with extra_namespace_label_selector:shpyrd.io/project; VRL remap attaches project/process/instance, parses JSON log lines; console sink; tolerates control-plane to reach build pods.
  - Verified live: shop web.1/web.2/worker.1 correctly labelled; blog JSON lines promoted to level+msg; api JSON parsed; no non-project pods in the stream.
