# RFC-0004 Dockerfile builds

**Status:** implemented (with gaps) — see Implementation status below

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Build repositories that ship a `Dockerfile` with **BuildKit** running as a rootless
Kubernetes Job, next to the buildpacks path. Same sources (uploaded archive or Git), same
registry, same releases; the build strategy is chosen by `shpyrd.yaml` or detected from the
presence of a `Dockerfile`.

## Motivation

Buildpacks cover the common stacks with zero configuration, but many repositories already
have a Dockerfile, use a stack buildpacks do not know, or need system packages. Cloud
Native Buildpacks cannot build application Dockerfiles: the CNB "extensions" feature only
lets Dockerfile snippets modify the build and run **base images**, and kpack exposes no
Dockerfile path at all.

### Goals

- `shpyrd deploy` of a repository with a `Dockerfile` just works, with streamed build
  output and the same release semantics.
- Multi-stage builds, build args, `.dockerignore`, cache between builds.
- No privileged pods.

### Non-Goals

- Replacing buildpacks as the default. Without a Dockerfile, buildpacks are used.
- Building on the developer's machine (`--image` already covers that).

## Proposal

### Options

| Option | Assessment |
| --- | --- |
| **BuildKit rootless Job** (`moby/buildkit:rootless`, `buildctl-daemonless.sh`) | standard Docker build semantics, maintained, amd64/arm64, unprivileged (user namespaces), pushes straight to the registry |
| kaniko | no longer maintained by Google (a Chainguard fork exists), Dockerfile gaps, slower |
| Shipwright Build | good API for both strategies but pulls in Tekton |
| kpack | no Dockerfile support |

**Decision: BuildKit Jobs orchestrated by the App controller.**

### Flow

1. `shpyrd deploy` detects `Dockerfile` in the deployed directory (or `build.dockerfile`
   in `shpyrd.yaml`) and sets `spec.build.strategy: dockerfile` with the path.
2. The controller creates a `Build` record (a small shpyrd CRD shared by both strategies
   so the dashboard has one build list) and a Job:
   - source: the uploaded archive URL (`--opt context=http://shpyrd-server.../sources/<sha>.tgz`
     - BuildKit accepts tar.gz HTTP contexts) or the Git URL with revision
     (`context=https://github.com/o/r.git#<ref>` plus `--opt build-arg:...`);
   - `--frontend dockerfile.v0 --opt filename=<dockerfile>`;
   - `--output type=image,name=<registry>/apps/<app>:b<n>,push=true,registry.insecure=true`;
   - `--export-cache type=registry,ref=<registry>/apps/<app>:cache --import-cache ...` for
     layer cache between builds;
   - `--metadata-file /out/meta.json` to capture the pushed digest (written to a
     `ConfigMap`/termination message the controller reads).
3. Logs stream from the Job pod like kpack steps (single step "build").
4. The digest becomes the release image; the rest is unchanged (sizes, config, rollout).

### shpyrd.yaml

```yaml
build:
  strategy: dockerfile        # default: auto (dockerfile when a Dockerfile exists, else buildpacks)
  dockerfile: deploy/Dockerfile
  args:
    NODE_ENV: production
  target: runtime             # multi-stage target
```

## Design Details

- Jobs run in the project namespace with the `kpack-builder`-equivalent ServiceAccount, a
  `dedicated`-like size (1 CPU / 2Gi by default, configurable in the catalog as the
  "build" size), `activeDeadlineSeconds` 30m, TTL after finish 1h; the controller keeps the
  last 10 Build records like kpack does.
- The BuildKit daemon runs rootless inside the Job container; user namespaces must be
  allowed by the node (kind and standard clusters are fine).
- Failure reasons are extracted from the Job's last lines and shown in the Activity panel.
- Process types: Dockerfile images have one entrypoint; `processes.<type>.command` sets
  the others (`worker: { command: ["node", "worker.js"] }`), and `web` uses `CMD`.

### Drawbacks

- Two build engines to keep healthy. They share the Build record and log streaming so the
  user does not see two systems.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-22: Implemented. Differences from the proposal above: no `Build` CRD yet, the
  Job itself is the build record (labels `shpyrd.io/build-number`, annotations with the
  key, digest, revision or failure) and the API merges kpack Builds and Jobs into one
  list; the source is fetched by an init container (archive download or `git clone`)
  into an emptyDir and built as a local context, which also gives `--path` (subPath)
  builds and the resolved git commit for the release description; build args come from
  `build.env` instead of a separate `args` map; the pushed digest travels in the
  container's termination message, the failure reason is the log tail
  (`terminationMessagePolicy: FallbackToLogsOnError`). Git sources rebuild when the
  configured revision or build settings change, not on new commits (kpack polls, the Job
  does not); pass a commit or redeploy to rebuild a branch.

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Not implemented:** A "build" size in the catalog (build Jobs use fixed 250m/512Mi requests, no limits) and `ttlSecondsAfterFinished` on build Jobs (they are pruned to the last ten).
- **Superseded:** `build.args`: Dockerfile build args come from `build.env`; the Build record is the Job, not a CRD.
