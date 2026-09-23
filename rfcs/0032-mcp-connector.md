# RFC-0032 MCP connector

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0031 (remote transport)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A Model Context Protocol server exposing shpyrd to AI agents (Claude Code, Cursor, Claude
Desktop): first as `shpyrd mcp` over stdio using the kubeconfig, then as a remote endpoint on
the server (streamable HTTP) authenticated with API tokens. Tools cover reading state and the
everyday operations; destroying is excluded.

## Motivation

Agents already run `shpyrd` commands by hand; a typed tool surface is safer and richer
(structured logs, metrics, releases).

### Goals

- `claude mcp add shpyrd -- shpyrd mcp` works; tools return structured JSON.
- Remote mode for hosted agents with per-user tokens and roles enforced.

### Non-Goals

- Autonomous remediation; anything without an explicit tool call.

## Proposal

- Tools: `list_projects`, `project_status`, `releases`, `logs` (window, process, filter),
  `metrics_summary`, `deploy_git`, `rollback`, `scale`, `set_config_vars` (names+values in,
  never out), `run_command` (one-off, returns output), `resources`, `audit`.
  Excluded: destroy project, delete resources, member changes.
- Resources (MCP): project overview documents; prompts: "diagnose failing instances".
- Local: `shpyrd mcp` speaks stdio, uses the kubeconfig like the CLI. Remote: `/mcp` on the
  server (streamable HTTP), `Authorization: Bearer shp_...`, roles enforced by the same
  middleware; every call audited with `via: mcp`.

## Design Details

- Go MCP SDK; tool schemas generated from Go structs; tests with a scripted client.

## Open questions

1. Tool set as above, never destroy? Default: yes.
2. Which clients to test with? Default: Claude Code and Claude Desktop (stdio), Cursor
   (remote).

## Implementation History

- 2026-09-22: RFC written.
