# RFC-0050 Git push deploys

**Status:** rejected

**Owner:** unassigned

**Depends on:** RFC-0031, RFC-0034

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

`git push shpyrd main` deploys: the server accepts Git pushes over HTTPS (API token as the
password), turns the pushed tree into an archive deploy and streams the build back to the
`git push` output, Heroku style.

## Motivation

Some teams want the Heroku workflow exactly; it needs no CLI on the developer's machine.

### Goals

- `git remote add shpyrd https://shpyrd.<domain>/git/shop.git && git push shpyrd main` builds
  and releases, with build output in the terminal.
- Authentication with API tokens (RFC-0031); roles enforced (`project.deploy`).

### Non-Goals

- Hosting repositories (the receiver keeps no history; the pushed tree is archived and
  discarded).

## Proposal

- Smart HTTP receive-pack endpoint `/git/<slug>.git` in the server backed by an in-memory
  or temp-dir repository per push; on `post-receive` the tree of the pushed branch is
  archived, uploaded to the source store and the App's source is set (same path as
  `shpyrd deploy`), then the build log is streamed as sideband progress.
- `shpyrd git remote add` prints the remote URL; the dashboard shows it on the Source card.
- Branch to deploy: `main` or `master`, or `shpyrd.yaml`'s `deploy.branch`.

## Decision

Rejected (2026-09-22). `shpyrd deploy` and auto-deploy from the repository (RFC-0018, RFC-0054)
cover the workflows in use; a Git receive-pack implementation adds an authentication
surface and a second deploy path for Heroku-style habit only.

## Implementation History

- 2026-09-22: RFC written and rejected.
