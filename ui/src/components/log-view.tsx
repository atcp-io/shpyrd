import { useEffect, useMemo, useRef, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { cn } from "@/lib/utils";
import { apiStream, type LogLine } from "@/lib/api";
import { localTime } from "@/lib/format";
import {
  atLeast,
  lineMatches,
  parseLogLine,
  type LogField,
  type LogLevel,
  type ParsedLine,
} from "@/lib/logs";

// Strips ANSI colour codes emitted by buildpacks and apps.
// eslint-disable-next-line no-control-regex
const ansi = /\x1b\[[0-9;]*m/g;

const instancePalette = [
  "text-orange-300",
  "text-sky-300",
  "text-emerald-300",
  "text-violet-300",
  "text-amber-300",
  "text-cyan-300",
  "text-pink-300",
  "text-lime-300",
];

function instanceColor(name: string): string {
  let h = 0;
  for (const c of name) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return instancePalette[h % instancePalette.length];
}

const levelColor: Record<LogLevel, string> = {
  error: "text-red-300",
  warn: "text-amber-300",
  info: "text-zinc-400",
  debug: "text-zinc-600",
};

/** Whether a line shows a level. A line that declared one always does; a
 * plain line only when the text reads as a problem, since labelling every
 * line of ordinary output INFO is noise, not information. */
function labelled(p: ParsedLine): boolean {
  return p.levelText !== "" || p.level === "error" || p.level === "warn";
}

/** Identifies a line across the trimming the stream does as it grows. */
function lineKey(l: LogLine): string {
  return (l.t ?? "") + "|" + l.i + "|" + l.m;
}

/** One key/value pair of a structured line. An object or an array gets a
 * control of its own: closed it reads as the one line it came in on, open
 * it becomes rows, as deep as the line goes. */
function Field({
  field,
  path,
  open,
  onToggle,
}: {
  field: LogField;
  path: string;
  open: Set<string>;
  onToggle: (path: string) => void;
}) {
  const nested = field.json;
  const expanded = open.has(path);
  return (
    <>
      <dt className="flex gap-1 text-zinc-500">
        {nested ? (
          <button
            type="button"
            onClick={() => onToggle(path)}
            aria-expanded={expanded}
            aria-label={(expanded ? "Hide " : "Show ") + field.key}
            className="mt-0.5 size-3 shrink-0 self-start text-zinc-600 hover:text-zinc-200 focus-visible:ring-1 focus-visible:ring-zinc-400 focus-visible:outline-none"
          >
            {expanded ? (
              <ChevronDown className="size-3" />
            ) : (
              <ChevronRight className="size-3" />
            )}
          </button>
        ) : (
          <span className="size-3 shrink-0" aria-hidden="true" />
        )}
        {field.key}
      </dt>
      <dd className="break-all whitespace-pre-wrap text-zinc-200">
        {nested && expanded ? (
          <LogFields
            fields={entries(nested)}
            path={path}
            open={open}
            onToggle={onToggle}
            nested
          />
        ) : (
          field.value.replace(ansi, "")
        )}
      </dd>
    </>
  );
}

/** The key/value rows under an expanded line, and under every object
 * opened within it. */
export function LogFields({
  fields,
  path,
  open,
  onToggle,
  nested = false,
}: {
  fields: LogField[];
  /** What each field's path is built on, so one expansion state can hold
   * every line's rows: the caller passes the line's own key. */
  path: string;
  open: Set<string>;
  onToggle: (path: string) => void;
  nested?: boolean;
}) {
  return (
    <dl
      className={cn(
        "grid grid-cols-[auto_1fr] gap-x-3 text-zinc-400",
        // Nested rows sit under their key, with a rule to follow back up.
        nested && "mt-0.5 border-l border-zinc-800 pl-2",
      )}
    >
      {fields.map((f) => (
        <div key={f.key} className="col-span-2 grid grid-cols-subgrid">
          <Field
            field={f}
            path={path === "" ? f.key : path + "." + f.key}
            open={open}
            onToggle={onToggle}
          />
        </div>
      ))}
    </dl>
  );
}

/** The rows of an object or an array, as fields. */
function entries(value: object): LogField[] {
  const pairs = Array.isArray(value)
    ? value.map((v, i) => ["[" + i + "]", v] as const)
    : Object.entries(value);
  return pairs.map(([key, v]) => {
    const json =
      v !== null && typeof v === "object" && Object.keys(v).length > 0
        ? (v as object)
        : undefined;
    const value = typeof v === "string" ? v : (JSON.stringify(v) ?? "");
    return json ? { key, value, json } : { key, value };
  });
}

/** Structured app log viewer: time, instance and message columns, JSON
 * lines rendered as level, message and expandable fields, level and text
 * filters, a raw view and follow (auto-scroll) mode. */
export function AppLogView({
  lines,
  follow,
  filter,
  level = "debug",
  raw = false,
  className,
  empty = "Waiting for log lines...",
}: {
  lines: LogLine[];
  follow: boolean;
  filter: string;
  /** Lowest level to show; "debug" shows everything. */
  level?: LogLevel;
  /** Show each line as the application wrote it. */
  raw?: boolean;
  className?: string;
  empty?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  // Lines arrive in batches and are re-rendered on every batch, so each one
  // is parsed once and remembered against the object itself.
  const cache = useRef(new WeakMap<LogLine, ParsedLine>());
  const parse = (l: LogLine): ParsedLine => {
    let p = cache.current.get(l);
    if (!p) {
      p = parseLogLine(l.m);
      cache.current.set(l, p);
    }
    return p;
  };

  const visible = useMemo(
    () =>
      lines.filter((l) => {
        const p = parse(l);
        return atLeast(p.level, level) && lineMatches(p, l.i, filter);
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [lines, filter, level],
  );

  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [visible, follow]);

  const toggle = (key: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });

  return (
    <div
      ref={ref}
      className={cn(
        "h-[30rem] overflow-auto rounded-lg border bg-zinc-950 p-2 font-mono text-xs leading-5 text-zinc-100",
        className,
      )}
    >
      {visible.length === 0 ? (
        <div className="p-2 text-zinc-500">
          {lines.length === 0 ? empty : "No lines match the filter"}
        </div>
      ) : (
        visible.map((l, idx) => {
          const p = parse(l);
          const key = lineKey(l);
          const fields = raw ? [] : p.fields;
          const open = expanded.has(key);
          return (
            <div
              key={idx}
              className={cn(
                "grid grid-cols-[5.5rem_6.5rem_1fr] gap-2 px-1 hover:bg-zinc-900",
                p.level === "error" && "bg-red-950/40",
                p.level === "warn" && "bg-amber-950/30",
              )}
            >
              <span className="select-none text-zinc-500">
                {localTime(l.t)}
              </span>
              <span className={cn("truncate", instanceColor(l.i))} title={l.i}>
                {l.i}
              </span>
              <div className="min-w-0">
                <div className="flex gap-1.5">
                  {fields.length > 0 ? (
                    <button
                      type="button"
                      onClick={() => toggle(key)}
                      aria-expanded={open}
                      aria-label={open ? "Hide fields" : "Show fields"}
                      className="mt-0.5 size-3 shrink-0 self-start text-zinc-500 hover:text-zinc-200 focus-visible:ring-1 focus-visible:ring-zinc-400 focus-visible:outline-none"
                    >
                      {open ? (
                        <ChevronDown className="size-3" />
                      ) : (
                        <ChevronRight className="size-3" />
                      )}
                    </button>
                  ) : (
                    <span className="size-3 shrink-0" aria-hidden="true" />
                  )}
                  {!raw && labelled(p) && (
                    // The bucket, so the column keeps one width whatever
                    // the application spelled; the spelling is in the title.
                    // A level read off a plain line is dimmed, because
                    // nothing declared it.
                    <span
                      title={
                        p.levelText === ""
                          ? "read from the text of the line"
                          : p.levelText
                      }
                      className={cn(
                        "shrink-0 uppercase",
                        levelColor[p.level],
                        p.levelText === "" && "opacity-60",
                      )}
                    >
                      {p.level}
                    </span>
                  )}
                  <span
                    className={cn(
                      "min-w-0 flex-1 break-all whitespace-pre-wrap",
                      !raw && p.level === "error" && "text-red-200",
                      !raw && p.level === "warn" && "text-amber-100",
                    )}
                  >
                    {(raw ? l.m : p.message).replace(ansi, "")}
                  </span>
                </div>
                {open && fields.length > 0 && (
                  <div className="mb-1 ml-4.5">
                    <LogFields
                      fields={fields}
                      path={key}
                      open={expanded}
                      onToggle={toggle}
                    />
                  </div>
                )}
              </div>
            </div>
          );
        })
      )}
    </div>
  );
}

/** Plain text log viewer for build output. */
export function TextLogView({
  lines,
  follow = true,
  className,
  empty = "No output",
}: {
  lines: string[];
  follow?: boolean;
  className?: string;
  empty?: string;
}) {
  const ref = useRef<HTMLPreElement>(null);
  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [lines, follow]);
  return (
    <pre
      ref={ref}
      className={cn(
        "h-[30rem] overflow-auto rounded-lg border bg-zinc-950 p-3 font-mono text-xs leading-5 text-zinc-100",
        className,
      )}
    >
      {lines.length === 0 ? (
        <span className="text-zinc-500">{empty}</span>
      ) : (
        lines.map((l, i) => {
          const clean = l.replace(ansi, "");
          const step = clean.startsWith("===> ");
          return (
            <div
              key={i}
              className={cn(
                step && "mt-2 font-semibold text-orange-300",
                /error|failed/i.test(clean) && !step && "text-red-300",
              )}
            >
              {clean}
            </div>
          );
        })
      )}
    </pre>
  );
}

/** Hook: streams NDJSON log lines from a path while enabled. */
export function useLogStream(path: string | null, deps: unknown[]) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [reset, setReset] = useState(0);
  useEffect(() => {
    if (!path) return;
    const ac = new AbortController();
    let first = true;
    // Batch incoming lines and flush at most every 100 ms: re-rendering the
    // list per line is what makes log tails expensive in the browser.
    let pending: LogLine[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;
    const flush = () => {
      timer = null;
      if (pending.length === 0) return;
      const batch = pending;
      pending = [];
      setLines((prev) => {
        const base = first ? [] : prev;
        first = false;
        const next = base.concat(batch);
        return next.length > 5000 ? next.slice(-4000) : next;
      });
    };
    apiStream(path, ac.signal, (raw) => {
      let l: LogLine;
      try {
        l = JSON.parse(raw) as LogLine;
      } catch {
        l = { i: "", p: "", m: raw };
      }
      pending.push(l);
      if (!timer) timer = setTimeout(flush, 100);
    })
      .then(flush)
      .catch((e: Error) => {
        if (e.name !== "AbortError") setError(e.message);
      });
    return () => {
      ac.abort();
      if (timer) clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, reset, ...deps]);
  return {
    lines,
    error,
    restart: () => {
      setLines([]);
      setError(null);
      setReset((n) => n + 1);
    },
  };
}
