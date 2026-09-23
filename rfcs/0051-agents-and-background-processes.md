# RFC-0051 Agents and background processes

**Status:** rejected

**Owner:** unassigned

**Depends on:** RFC-0024, RFC-0016, RFC-0029

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

First-class support for long-running, non-HTTP processes — queue workers, schedulers, LLM
agents: a process kind without a URL that shpyrd knows how to health-check, run as a
singleton, wake on events, hand credentials to and observe, with example projects and a
"deploy an agent" guide.

## Motivation

Shpyrd's tagline includes agents; today an agent is a `worker` process with nothing
specific to it.

### Goals

- `kind: agent` processes with singleton semantics, liveness through a heartbeat file or
  endpoint, graceful stop, restart policy and a "last activity" indicator.
- Event triggers: an inbound webhook URL per agent that starts a run or enqueues work.
- Agent-oriented config: model provider keys as global vars (RFC-0016), OTel traces of tool
  calls (RFC-0029), scheduled runs (RFC-0024).

### Non-Goals

- An agent framework or SDK; shpyrd runs what you build.

## Proposal

- `processes.<name>.kind: agent` (default `web` for HTTP, `worker` otherwise): implies one
  instance unless declared, `Recreate` rollout (a singleton never runs twice), no port, a
  heartbeat health check (`healthcheck: {file: /tmp/heartbeat, maxAge: 60s}` or an HTTP
  path on an internal port), longer `shutdownDelay` defaults.
- Triggers: `triggers: [{webhook: true}]` gives `https://shpyrd.<domain>/api/hooks/agents/
  <project>/<name>/<token>` that creates a Run (RFC-0024) with the payload as input or
  pushes to an attached Redis list.
- Dashboard: an Agents view listing agent processes with last heartbeat, runs, triggers.
- Examples: a Python LangGraph agent and a Node worker; a guide.

## Decision

Rejected (2026-09-22). An agent is an application: a `worker` process type, a one-off run or
a scheduled task (RFC-0024). Everything the platform offers apps (config vars, attached
resources, global vars for model provider keys, logs, metrics, OpenTelemetry) applies
without a new kind. Documentation gets a guide on deploying agents as apps.

## Implementation History

- 2026-09-22: RFC written and rejected.
