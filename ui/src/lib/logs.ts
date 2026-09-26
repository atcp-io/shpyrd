/** Log line parsing shared by the log viewer and the level filter.
 *
 * Mirrors pkg/logfmt on the server side: both turn one raw line into a
 * level, a message and the remaining fields, so the dashboard and
 * `shpyrd logs --pretty` agree on what a line says. Keep the two in step.
 */

/** Severity buckets used for colouring and filtering. */
export type LogLevel = "error" | "warn" | "info" | "debug";

export type LogField = {
  key: string;
  /** One-line form, shown collapsed and searched by the filter. */
  value: string;
  /** The parsed value when it is a non-empty object or array, so the
   * viewer can open it instead of showing a wall of JSON. */
  json?: object;
};

export type ParsedLine = {
  /** True when the line was a JSON object. */
  structured: boolean;
  /** Severity bucket: from the level field, or guessed from the text. */
  level: LogLevel;
  /** The level as the line spelled it ("" when the line names none). */
  levelText: string;
  message: string;
  /** The line's own timestamp, when it carried one. */
  time?: string;
  /** Everything that was not a well-known key, in source order. */
  fields: LogField[];
};

// Well-known keys, in the spellings the common loggers use.
const levelKeys = ["level", "severity", "lvl", "log.level"];
const messageKeys = ["msg", "message", "event"];
const timeKeys = ["time", "ts", "timestamp", "@timestamp"];
const errorKeys = ["error", "err"];

/** Guesses a level from a line that does not carry one. */
function guessLevel(text: string): LogLevel {
  const m = text.slice(0, 200).toLowerCase();
  if (/\b(error|err|fatal|panic|exception|traceback|failed)\b/.test(m))
    return "error";
  if (/\b(warn|warning)\b/.test(m)) return "warn";
  return "info";
}

/** Renders a JSON value as the single line a field shows. */
function fieldValue(v: unknown): string {
  if (typeof v === "string") return v;
  return JSON.stringify(v) ?? "";
}

/** The value as data when it is worth opening: a non-empty object or
 * array. Anything else reads fine on one line. */
function fieldJSON(v: unknown): object | undefined {
  if (v === null || typeof v !== "object") return undefined;
  return Object.keys(v).length > 0 ? (v as object) : undefined;
}

function field(key: string, v: unknown): LogField {
  const json = fieldJSON(v);
  return json
    ? { key, value: fieldValue(v), json }
    : { key, value: fieldValue(v) };
}

/** The first value of the aliases that is set, as data. */
function firstRaw(obj: Record<string, unknown>, keys: string[]): unknown {
  for (const k of keys) {
    const v = obj[k];
    if (v !== undefined && v !== null && v !== "") return v;
  }
  return undefined;
}

function first(obj: Record<string, unknown>, keys: string[]): string {
  for (const k of keys) {
    const v = obj[k];
    if (v !== undefined && v !== null && v !== "") return fieldValue(v);
  }
  return "";
}

export function parseLogLine(raw: string): ParsedLine {
  const body = raw.trim();
  let obj: Record<string, unknown> | null = null;
  if (body.startsWith("{")) {
    try {
      // The leading brace already rules out arrays and scalars; the
      // typeof check is what narrows the parsed value for TypeScript.
      const v: unknown = JSON.parse(body);
      if (v !== null && typeof v === "object")
        obj = v as Record<string, unknown>;
    } catch {
      obj = null;
    }
  }
  if (!obj) {
    return {
      structured: false,
      level: guessLevel(raw),
      levelText: "",
      message: raw,
      fields: [],
    };
  }

  const levelText = first(obj, levelKeys);
  const message = first(obj, messageKeys);
  const time = first(obj, timeKeys);
  const err = first(obj, errorKeys);
  const errRaw = firstRaw(obj, errorKeys);

  const known = new Set([
    ...levelKeys,
    ...messageKeys,
    ...timeKeys,
    ...errorKeys,
  ]);
  const fields: LogField[] = [];
  // The error reads as part of the message, so it leads the fields.
  if (err) fields.push(field("error", errRaw));
  // Object.keys preserves insertion order for non-numeric keys, so fields
  // stay in the order the application wrote them.
  for (const k of Object.keys(obj)) {
    if (known.has(k)) continue;
    fields.push(field(k, obj[k]));
  }

  // levelText keeps the spelling the line used; the bucket in `level` is
  // what gets displayed, so "warning" and pino's 40 read the same width.
  const level =
    levelText === "" ? guessLevel(message) : normalizeLevel(levelText);

  return {
    structured: true,
    level,
    levelText,
    message,
    time: time || undefined,
    fields,
  };
}

/** Reads pino's scale (10 trace to 60 fatal) and, below 10, the syslog
 * severities (0 emerg to 7 debug). */
function numericLevel(n: number): LogLevel {
  if (n < 10) {
    if (n <= 3) return "error";
    if (n === 4) return "warn";
    return n <= 6 ? "info" : "debug";
  }
  if (n >= 50) return "error";
  if (n >= 40) return "warn";
  return n >= 30 ? "info" : "debug";
}

/** Folds a logger's level into a severity bucket, by name or by number:
 * pino counts in tens up to 60, the syslog severities count down from 0,
 * and both are common enough to read. */
export function normalizeLevel(text: string): LogLevel {
  const t = text.trim();
  if (/^-?\d+$/.test(t)) return numericLevel(Number(t));
  switch (t.toLowerCase()) {
    case "error":
    case "err":
    case "fatal":
    case "crit":
    case "critical":
    case "panic":
    case "alert":
    case "emerg":
    case "emergency":
      return "error";
    case "warn":
    case "warning":
      return "warn";
    case "debug":
    case "trace":
      return "debug";
    default:
      return "info";
  }
}

const rank: Record<LogLevel, number> = { debug: 0, info: 1, warn: 2, error: 3 };

/** True when level is min or more severe; how the level filter selects. */
export function atLeast(level: LogLevel, min: LogLevel): boolean {
  return rank[level] >= rank[min];
}

/** True when the filter text appears anywhere in the line, fields included. */
export function lineMatches(
  line: ParsedLine,
  instance: string,
  filter: string,
): boolean {
  const f = filter.trim().toLowerCase();
  if (!f) return true;
  if (line.message.toLowerCase().includes(f)) return true;
  if (instance.toLowerCase().includes(f)) return true;
  return line.fields.some(
    (x) => x.key.toLowerCase().includes(f) || x.value.toLowerCase().includes(f),
  );
}
