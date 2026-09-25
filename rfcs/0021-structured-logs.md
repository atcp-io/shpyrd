# RFC-0021 Structured logs in the viewer and the CLI

**Status:** in progress

**Owner:** Marcelo Paez Sequeira (shpyrd-io/shpyrd rfc-0021-structured-logs)

**Depends on:** none

**Creation date:** 2026-09-22

**Last update:** 2026-09-25

## Summary

Detect JSON log lines and render them readably: level, message and time up front, other
fields collapsed and expandable, filtering by level and field; `shpyrd logs --pretty`
(default on a terminal) does the same in the CLI.

## Motivation

Most frameworks log JSON in production; a wall of `{"level":"info","msg":...}` is hard to
read and the viewer's level highlighting misses it.

### Goals

- JSON lines look like log lines; raw view one click away.
- Level filter works for JSON (`level`, `severity`, `lvl`) and plain lines alike.

### Non-Goals

- Storage or search over time (RFC-0022).

## Proposal

- Parser: a line whose trimmed body starts with `{` and parses as an object is structured;
  well-known keys map to level (`level|severity|lvl|log.level`), message
  (`msg|message|event`), time (`time|ts|timestamp|@timestamp`), error (`error|err`).
- Dashboard: structured lines render `LEVEL message` with a chevron revealing the remaining
  fields as key/value; a "raw" toggle per view; the filter box matches field values too.
- CLI: `--pretty` renders `time level message key=value...`; `--json` passes lines through
  untouched; default: pretty when stdout is a terminal.
- Works on the live stream (no dependency on the pipeline); when RFC-0022 lands the same
  renderer applies to history.

## Design Details

- UI: `parseLogLine` in `ui/src/lib/logs.ts` with tests; CLI: `pkg/logfmt`.
- Multi-line JSON is not reassembled (lines are units).

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-25: implementation started.
