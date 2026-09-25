import { describe, expect, it } from "vitest";
import { atLeast, lineMatches, parseLogLine } from "@/lib/logs";

describe("parseLogLine on plain lines", () => {
  it("keeps the line as the message", () => {
    const l = parseLogLine("Listening on :8080");
    expect(l.structured).toBe(false);
    expect(l.message).toBe("Listening on :8080");
    expect(l.fields).toEqual([]);
  });

  it("reads the level from the words in the line", () => {
    expect(parseLogLine("Listening on :8080").level).toBe("info");
    expect(parseLogLine("WARN disk almost full").level).toBe("warn");
    expect(parseLogLine("panic: runtime error").level).toBe("error");
  });

  it("leaves the level label empty when the line does not name one", () => {
    expect(parseLogLine("Listening on :8080").levelText).toBe("");
  });
});

describe("parseLogLine on JSON objects", () => {
  it("pulls level, message and time out of the object", () => {
    const l = parseLogLine(
      '{"level":"info","msg":"request served","time":"2026-09-25T10:00:00Z"}',
    );
    expect(l.structured).toBe(true);
    expect(l.level).toBe("info");
    expect(l.levelText).toBe("info");
    expect(l.message).toBe("request served");
    expect(l.time).toBe("2026-09-25T10:00:00Z");
    expect(l.fields).toEqual([]);
  });

  it("keeps the remaining fields in the order the line wrote them", () => {
    const l = parseLogLine(
      '{"msg":"hi","zebra":1,"alpha":"two","level":"warn"}',
    );
    expect(l.fields).toEqual([
      { key: "zebra", value: "1" },
      { key: "alpha", value: "two" },
    ]);
  });

  it("puts the error field first, whatever its position", () => {
    const l = parseLogLine('{"msg":"boom","a":"1","err":"connection reset"}');
    expect(l.fields).toEqual([
      { key: "error", value: "connection reset" },
      { key: "a", value: "1" },
    ]);
  });

  it("renders nested values as compact JSON", () => {
    const l = parseLogLine(
      '{"msg":"hi","req":{"path":"/","n":2},"tags":["a","b"],"ok":true,"none":null}',
    );
    expect(l.fields).toEqual([
      { key: "req", value: '{"path":"/","n":2}' },
      { key: "tags", value: '["a","b"]' },
      { key: "ok", value: "true" },
      { key: "none", value: "null" },
    ]);
  });
});

describe("parseLogLine key aliases", () => {
  it("accepts the common spellings of each well-known key", () => {
    const l = parseLogLine(
      '{"severity":"ERROR","message":"nope","@timestamp":"2026-09-25T10:00:00Z","error":"eof"}',
    );
    expect(l.levelText).toBe("ERROR");
    expect(l.level).toBe("error");
    expect(l.message).toBe("nope");
    expect(l.time).toBe("2026-09-25T10:00:00Z");
    expect(l.fields).toEqual([{ key: "error", value: "eof" }]);
  });

  it("accepts lvl, event and ts", () => {
    const l = parseLogLine('{"lvl":"debug","event":"tick","ts":1758790000}');
    expect(l.level).toBe("debug");
    expect(l.message).toBe("tick");
    expect(l.time).toBe("1758790000");
  });

  it("folds level names onto severity buckets", () => {
    expect(parseLogLine('{"level":"fatal","msg":"x"}').level).toBe("error");
    expect(parseLogLine('{"level":"WARNING","msg":"x"}').level).toBe("warn");
    expect(parseLogLine('{"level":"trace","msg":"x"}').level).toBe("debug");
    expect(parseLogLine('{"level":"notice","msg":"x"}').level).toBe("info");
  });

  it("guesses the level from the message when the object names none", () => {
    expect(parseLogLine('{"msg":"failed to connect"}').level).toBe("error");
  });
});

describe("parseLogLine on lines that only look structured", () => {
  it("treats a JSON array as plain text", () => {
    const l = parseLogLine("[1,2,3]");
    expect(l.structured).toBe(false);
    expect(l.message).toBe("[1,2,3]");
  });

  it("treats an unterminated object as plain text", () => {
    const l = parseLogLine('{"msg":"cut off"');
    expect(l.structured).toBe(false);
    expect(l.message).toBe('{"msg":"cut off"');
  });

  it("treats a bare JSON string as plain text", () => {
    expect(parseLogLine('"hello"').structured).toBe(false);
  });

  it("parses an object that is indented", () => {
    expect(parseLogLine('   {"msg":"hi"}  ').structured).toBe(true);
  });

  it("has an empty message when the object carries none", () => {
    const l = parseLogLine('{"level":"info","user":"ana"}');
    expect(l.message).toBe("");
    expect(l.fields).toEqual([{ key: "user", value: "ana" }]);
  });

  it("leaves ANSI escapes in the message for the renderer to strip", () => {
    const esc = String.fromCharCode(27);
    const l = parseLogLine(
      JSON.stringify({ msg: esc + "[31mred" + esc + "[0m" }),
    );
    expect(l.message).toBe(esc + "[31mred" + esc + "[0m");
  });
});

describe("atLeast", () => {
  it("keeps the level asked for and everything more severe", () => {
    expect(atLeast("error", "warn")).toBe(true);
    expect(atLeast("warn", "warn")).toBe(true);
    expect(atLeast("info", "warn")).toBe(false);
    expect(atLeast("debug", "info")).toBe(false);
  });
});

describe("lineMatches", () => {
  const line = parseLogLine('{"msg":"request served","path":"/checkout"}');

  it("matches the message", () => {
    expect(lineMatches(line, "web.1", "served")).toBe(true);
  });

  it("matches the instance", () => {
    expect(lineMatches(line, "web.1", "web.")).toBe(true);
  });

  it("matches a field value", () => {
    expect(lineMatches(line, "web.1", "checkout")).toBe(true);
  });

  it("matches a field key", () => {
    expect(lineMatches(line, "web.1", "path")).toBe(true);
  });

  it("ignores case and surrounding blanks", () => {
    expect(lineMatches(line, "web.1", "  CHECKOUT ")).toBe(true);
  });

  it("does not match text that is nowhere in the line", () => {
    expect(lineMatches(line, "web.1", "worker")).toBe(false);
  });

  it("matches everything when the filter is empty", () => {
    expect(lineMatches(line, "web.1", "   ")).toBe(true);
  });
});

// Mirrors TestParseDuplicateWellKnownKeys in pkg/logfmt.
describe("parseLogLine with two spellings of one well-known key", () => {
  it("consumes both, keeps the first non-empty value", () => {
    const l = parseLogLine(
      '{"msg":"a","message":"b","level":"","severity":"warn","keep":"1"}',
    );
    expect(l.message).toBe("a");
    expect(l.levelText).toBe("warn");
    expect(l.level).toBe("warn");
    expect(l.fields).toEqual([{ key: "keep", value: "1" }]);
  });
});

// Mirrors TestParseNumericLevels in pkg/logfmt.
describe("parseLogLine with a numeric level", () => {
  it("names pino's scale by its bucket", () => {
    for (const [level, want, text] of [
      [10, "debug", "debug"],
      [30, "info", "info"],
      [40, "warn", "warn"],
      [50, "error", "error"],
      [60, "error", "error"],
    ] as const) {
      const l = parseLogLine(`{"level":${level},"msg":"x"}`);
      expect([l.level, l.levelText]).toEqual([want, text]);
    }
  });

  it("names the syslog severities by their bucket", () => {
    for (const [level, want] of [
      [3, "error"],
      [4, "warn"],
      [6, "info"],
      [7, "debug"],
    ] as const) {
      expect(parseLogLine(`{"level":${level},"msg":"x"}`).level).toBe(want);
    }
  });
});
