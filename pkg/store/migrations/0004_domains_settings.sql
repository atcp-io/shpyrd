-- Phase 3: workspace settings (the join policy) and domain claims.

ALTER TABLE workspaces ADD COLUMN settings JSONB NOT NULL DEFAULT '{}';

CREATE TABLE domain_claims (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    domain       TEXT NOT NULL,
    token        TEXT NOT NULL,
    connector    TEXT NOT NULL DEFAULT '',
    verified_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, domain)
);
