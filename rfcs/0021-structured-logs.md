# RFC-0021 Structured logs in the viewer and the CLI

**Status:** implemented

**Owner:** Marcelo Paez Sequeira (shpyrd-io/shpyrd rfc-0021-structured-logs)

**Depends on:** none

**Creation date:** 2026-09-22

**Last update:** 2026-09-26

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
- 2026-09-25: implemented.
  - `pkg/logfmt`: `Parse` reads one line into a level, a message, a timestamp and the
    remaining fields; `Entry.Pretty` renders "LEVEL message key=value...". A line counts as
    structured only when the whole line is a JSON object, so an array, a scalar, a
    truncated object or trailing content stays plain text.
  - `ui/src/lib/logs.ts`: the same contract in the browser, plus `atLeast` for the level
    filter and `lineMatches` for filtering over fields. The plain-line level guess moved
    here out of `log-view.tsx`, so structured and plain lines share one definition.
  - Both parsers consume every spelling of a well-known key (a line writing `msg` and
    `message` shows neither as a field) and keep the remaining fields in the order the line
    wrote them, with the error field first; nested values render as compact JSON, unescaped.
  - A numeric level is read and named by its bucket, so pino's `"level":30` reads as INFO
    rather than 30: tens up to 60 are pino's scale, 0 to 7 the syslog severities.
  - Viewer: a JSON line reads as its level and message with the fields behind a chevron;
    the level select picks a floor ("All levels" through "Errors only") and applies to plain
    lines too; Raw shows each line as the application wrote it. The text filter matches
    field keys and values. Each line is parsed once and kept against the line object in a
    WeakMap, because the stream re-renders on every 100 ms batch.
  - CLI: `shpyrd logs` renders JSON lines when stdout is a terminal, `--pretty` and `--json`
    force either and refuse to be combined. The time and instance columns stay the
    container's, so a line's own time field is not printed twice.
  - Tests: vitest joins the dashboard (`npm run test`, run by `make test` and the Dashboard
    CI job) with 38 tests, including react-dom/server smoke tests of the viewer that need no
    DOM; `pkg/logfmt` carries 18 and the CLI helpers 10, over the same case table, so the two
    parsers cannot drift. Rendering real zap, logrus, pino, bunyan and structlog lines is
    what turned up the numeric levels and the escaped nested values.
  - Verified on kind against `examples/blog` (which logs JSON): rendered on a terminal and
    untouched when piped, `--pretty` and `--json` forcing either, and `--json` byte-identical
    to the container's own lines. Lines written straight into the container's stdout covered
    the rest: a plain line and a `panic:` line pass through, pino's `"level":50` reads as
    ERROR with its `time` not printed twice, logrus's `"warning"` keeps its spelling in the
    warn bucket, a nested object and an array print unescaped, and a truncated object stays
    plain text.
  - Not built, and not promised by this RFC: a `--level` filter for the CLI, and `key=value`
    queries in the filter box (it matches field keys and values as substrings).
- 2026-09-26: the viewer, after reading real output on a cluster.
  - A field holding an object or an array opens too, as deep as the line goes, each level
    indented behind a rule; arrays number their entries and an empty object stays a leaf.
    The parser keeps the parsed value beside the one-line form, so the filter and
    `pkg/logfmt` are untouched.
  - The level column shows the bucket rather than the spelling, because logrus writes
    "warning" and pino a number and both pushed the message out of line; the spelling is on
    the element's title, and `--pretty` does the same.
  - A plain line that reads as an error or a warning is labelled too, dimmed, with a title
    saying it was read from the text. Ordinary output stays unlabelled: an INFO against
    every line of buildpack chatter is noise, and the blank column is what makes a labelled
    line worth looking at.
  - Fixed: the nested chevrons did nothing. The expansion was stored under
    `<line>#<field path>` and read back as the field path alone, so the write and the read
    never met.
  - jsdom and testing-library join the dashboard's tests. Both UI defects so far -- an
    unsized icon and that one -- were invisible to tests that can render but never click.
  - Considered and dropped: ANSI colour in `shpyrd logs`. It was built and reverted; the
    owner did not want it. A large record is read with `--json | jq`, and the terminal
    keeps one line per log line.
