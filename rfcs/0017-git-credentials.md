# RFC-0017 Git credentials for private repositories

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0004 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Deploy from private Git repositories: a token or an SSH deploy key stored per project (or
cluster-wide), used by kpack (buildpacks) and by the BuildKit source fetch (Dockerfiles).

## Motivation

`shpyrd deploy --git` only works for public repositories.

### Goals

- `shpyrd git-auth set --host github.com --token ghp_...` or `--ssh-key ~/.ssh/deploy_key`
  and private repositories build.
- Credentials are write-only and per project by default; a cluster-wide default is optional.

### Non-Goals

- A GitHub App with an installation flow (a follow-up RFC: "Connect GitHub", repository
  picker, short-lived installation tokens).

## Proposal

- Secret `<app>-git-<host>` in the project namespace, `kubernetes.io/basic-auth` (token as
  password, username `x-access-token` for GitHub) or `kubernetes.io/ssh-auth`, annotated
  `kpack.io/git: https://github.com` so kpack attaches it (the kpack Image gets a
  ServiceAccount listing the Secret).
- BuildKit fetch container: for basic auth, a `.netrc`/`GIT_ASKPASS` fed from the Secret;
  for SSH, the key mounted and `GIT_SSH_COMMAND` with known hosts pinned.
- Cluster-wide default: the same Secret in `shpyrd-system` labelled `shpyrd.io/git-default`,
  copied into projects that have none for that host.
- CLI: `shpyrd git-auth set|list|unset` (names and hosts only), dashboard: Deploy dialog
  shows "private repository: credentials for github.com configured" or a link to add them.

## Design Details

- The App controller renders a per-project ServiceAccount `shpyrd-build` with the Secrets
  and points the kpack Image and the BuildKit Job at it.
- Errors from a failed clone ("authentication failed for host") surface in the build log
  and status message.

## Open questions

1. Start with tokens and deploy keys, GitHub App later? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
