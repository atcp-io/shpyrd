-- RFC-0033 phase 6: workspaces answer at a host of their own.
-- address is the host of the workspace's dashboard and sign-in
-- (acme.shpyrd.app); its apps live at <app>.<address>. The implicit
-- workspace keeps '' and uses the platform domain.
-- status lets the operator switch a workspace off (suspended) without
-- deleting anything.

ALTER TABLE workspaces ADD COLUMN address TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN status TEXT NOT NULL DEFAULT 'active';

CREATE UNIQUE INDEX workspaces_address ON workspaces (address) WHERE address <> '';
