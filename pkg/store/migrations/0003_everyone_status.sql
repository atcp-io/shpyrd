-- Phase 3: the built-in "everyone" team (every person who signed in), and
-- a status on people so one can be suspended at once.

ALTER TABLE teams ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE identities ADD COLUMN status TEXT NOT NULL DEFAULT 'active';
