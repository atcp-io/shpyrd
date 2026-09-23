# RFC-0043 Builds namespace and enforce-mode Pod Security

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0004 (implemented), RFC-0008 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Move build Jobs (BuildKit, and kpack builds where possible) into a dedicated
`shpyrd-builds` namespace with the policy they need, then switch project namespaces to
`enforce=restricted`: the API server refuses any non-compliant pod, whoever creates it.

## Motivation

Project namespaces warn instead of refusing. A developer with `pods/create` (needed for
one-off runs) could create a privileged pod. The only pods failing `restricted` today are
the rootless BuildKit builds (seccomp and AppArmor `Unconfined`).

### Goals

- `pod-security.kubernetes.io/enforce: restricted` on every project namespace.
- Builds unchanged for users: same logs, same status.

### Non-Goals

- Rootful builds; changing kpack.

## Proposal

- Namespace `shpyrd-builds` (created by the shpyrd component) labelled `baseline` enforce
  with an exemption for the build ServiceAccount, or `privileged` if BuildKit's user
  namespaces require it (verify).
- The App controller creates BuildKit Jobs there, labelled `shpyrd.io/project`,
  `shpyrd.io/app`, `shpyrd.io/build-number`; ownership becomes label-based (no
  cross-namespace owner references) with GC by the controller; the source fetch, push and
  digest reporting are unchanged; log streaming and `shpyrd logs --build` read from the
  builds namespace; the builds namespace gets its own network policy (egress to the
  registry, the server and the internet).
- kpack build pods pass `restricted` already and stay in place.
- Project namespaces: `enforce: restricted` (keep `warn`/`audit`); `shpyrd run` and app pods
  already comply; the friendly non-root/non-numeric-user messages stay.

## Design Details

- RBAC: the builds namespace's Role for the server; developers get no rights there.
- Migration: existing build Jobs in project namespaces are left to prune.

## Implementation History

- 2026-09-22: RFC written.
