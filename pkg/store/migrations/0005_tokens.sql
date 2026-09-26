-- Per-user API tokens (RFC-0031): scoped, expiring, last-seen.
-- The secret value shp_<id>_<random> is shown once; only the SHA-256 hash
-- of the random part is stored here.

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    owner_email  TEXT NOT NULL DEFAULT '',
    hash         TEXT NOT NULL,           -- SHA-256(random) hex
    platform_role TEXT NOT NULL DEFAULT '',
    project_roles JSONB NOT NULL DEFAULT '{}',  -- {slug: role}
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,             -- NULL = no expiry
    last_used_at TIMESTAMPTZ,
    UNIQUE (workspace_id, owner_email, name)
);

CREATE INDEX api_tokens_hash ON api_tokens (hash);
