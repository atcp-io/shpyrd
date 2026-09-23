# RFC-0018 Repository monitoring and auto-deploy

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0017

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Deploy when the repository changes: push webhooks from GitHub/GitLab for both build
strategies, a polling fallback for Dockerfile builds (buildpack builds already poll through
kpack), a branch filter and a per-project auto-deploy switch.

## Motivation

Only buildpack builds from Git rebuild on new commits today; Dockerfile builds need a
redeploy. Webhooks are faster than polling and the standard integration.

### Goals

- Push to the configured branch → build → release, within seconds with a webhook.
- Works without inbound webhooks (polling) for clusters that are not reachable.

### Non-Goals

- Pull request preview environments (a later RFC).

## Proposal

- `POST /api/hooks/git/<project>/<token>`: a per-project random token (`shpyrd git-hooks
  show` prints the URL and the secret); verifies GitHub's HMAC signature or GitLab's token,
  ignores pushes to other branches, resolves the commit and sets `spec.source.git.revision`
  to it (a deploy with note "Push <sha> by <author>").
- Polling: the controller runs `git ls-remote` every `SHPYRD_GIT_POLL_INTERVAL` (default 2m)
  for Dockerfile-strategy apps whose revision is a branch, using the project's credentials.
- `spec.source.git.autoDeploy: true|false` (default true for git sources); `shpyrd deploy
  --git ... --no-auto-deploy`; dashboard toggle on the Source card.
- Optional "build only, promote manually" mode: builds run, the release waits for
  `shpyrd promote` (see questions).

## Design Details

- Webhook handler is public but rate limited and constant-time compares the token; it
  records an audit entry `git.push` with the actor from the payload.
- Kpack's own polling stays for buildpack builds; the webhook path pins the commit for both.

## Open questions

1. Deploy on push immediately (default) or also offer "build, then promote manually"?
   Default: immediate only in this RFC; promotion as a follow-up.

## Implementation History

- 2026-09-22: RFC written.
