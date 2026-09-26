-- Sessions and the edge's one-time codes (RFC-0033 phase 3): out of the
-- server's memory and its Secret mirror, so restarts keep people signed in
-- and every replica sees the same sign-outs.

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    identity     JSONB NOT NULL,
    csrf         TEXT NOT NULL,
    id_token     TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_last_seen ON sessions (last_seen_at);

CREATE TABLE edge_codes (
    code       TEXT PRIMARY KEY,
    host       TEXT NOT NULL,
    claims     JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
