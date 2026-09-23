# RFC-0039 Postgres pooling, credential rotation and resize

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0009 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

PgBouncer pooling in front of a database (`spec.pooler`), `shpyrd pg rotate db` to change
the application password with a config release for attached apps, `shpyrd pg resize db
--size --storage`, and metrics scraping (PodMonitor) shared with RFC-0028.

## Motivation

Serverless-style apps open many connections; passwords must be rotatable; size changes
need a command.

## Proposal

- `spec.pooler: {enabled: true, instances: 1, mode: transaction}` → CNPG `Pooler`
  `<name>-pooler`; the Binder's host becomes `<name>-pooler-rw` while `DATABASE_DIRECT_URL`
  keeps the primary for migrations.
- `shpyrd pg rotate db`: new random password written to the `<name>-app` Secret (CNPG
  applies it); the App controller watches CNPG app Secrets and re-renders bindings → release
  "Rotate db credentials" in every attached app.
- `shpyrd pg resize db --size shared-l --storage 20Gi`; CNPG rolls instances; single
  instance means a short outage, said in the command output.
- `spec.monitoring.enablePodMonitor: true` always on.

## Design Details

- Secret watch mapped to Apps bound to the Postgres (label `cnpg.io/cluster`).

## Implementation History

- 2026-09-22: RFC written.
