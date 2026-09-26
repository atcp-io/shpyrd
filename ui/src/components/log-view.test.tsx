import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AppLogView, LogFields } from "@/components/log-view";
import { parseLogLine } from "@/lib/logs";
import type { LogLine } from "@/lib/api";

const lines: LogLine[] = [
  { t: "2026-09-25T10:00:00Z", i: "web.1", p: "web", m: "plain startup line" },
  {
    t: "2026-09-25T10:00:01Z",
    i: "web.1",
    p: "web",
    m: '{"level":"info","msg":"request served","path":"/checkout"}',
  },
  {
    t: "2026-09-25T10:00:02Z",
    i: "worker.1",
    p: "worker",
    m: '{"level":"error","msg":"job lost","err":"eof"}',
  },
];

// Static markup escapes the quotes a JSON line is full of, so assertions
// read the decoded text instead.
function decode(html: string): string {
  return html
    .replace(/&quot;/g, '"')
    .replace(/&#x27;/g, "'")
    .replace(/&amp;/g, "&");
}

function render(props: Partial<Parameters<typeof AppLogView>[0]> = {}) {
  return decode(
    renderToStaticMarkup(
      <AppLogView lines={lines} follow={false} filter="" {...props} />,
    ),
  );
}

describe("AppLogView level labels", () => {
  const spelled: LogLine[] = [
    { t: "2026-09-25T10:00:00Z", i: "web.1", p: "web", m: '{"level":"warning","msg":"slow query"}' },
  ];

  it("shows the bucket, not the spelling, so the column keeps one width", () => {
    const html = decode(
      renderToStaticMarkup(
        <AppLogView lines={spelled} follow={false} filter="" />,
      ),
    );
    // Uppercased by CSS, so the text node itself is the bucket.
    expect(html).toContain(">warn<");
    expect(html).not.toContain(">warning<");
  });

  it("keeps what the line wrote within reach", () => {
    const html = decode(
      renderToStaticMarkup(
        <AppLogView lines={spelled} follow={false} filter="" />,
      ),
    );
    expect(html).toContain('title="warning"');
  });
});

describe("LogFields", () => {
  const fields = parseLogLine(
    '{"msg":"job lost","job":{"id":"j-12","queue":"mail"},"tags":["a","b"],"n":2}',
  ).fields;

  const show = (open: string[] = []) =>
    decode(
      renderToStaticMarkup(
        <LogFields
          fields={fields}
          path=""
          open={new Set(open)}
          onToggle={() => {}}
        />,
      ),
    );

  it("previews an object on one line while it is closed", () => {
    expect(show()).toContain('{"id":"j-12","queue":"mail"}');
  });

  it("gives an object its own control, named for the field", () => {
    expect(show()).toContain("Show job");
  });

  it("gives a primitive no control", () => {
    const html = show();
    expect(html).toContain("Show tags");
    expect(html).not.toContain("Show n");
  });

  it("opens an object into its own keys and values", () => {
    const html = show(["job"]);
    expect(html).toContain("j-12");
    expect(html).toContain("queue");
    expect(html).toContain("Hide job");
    // The one-line preview gives way to the opened rows.
    expect(html).not.toContain('{"id":"j-12","queue":"mail"}');
  });

  it("numbers the entries of an array", () => {
    const html = show(["tags"]);
    expect(html).toContain("[0]");
    expect(html).toContain("[1]");
  });

  it("opens objects nested inside objects", () => {
    const deep = parseLogLine('{"msg":"x","a":{"b":{"c":"deep"}}}').fields;
    const html = decode(
      renderToStaticMarkup(
        <LogFields
          fields={deep}
          path=""
          open={new Set(["a", "a.b"])}
          onToggle={() => {}}
        />,
      ),
    );
    expect(html).toContain("deep");
  });
});

describe("AppLogView", () => {
  it("shows the level and message of a JSON line, not its braces", () => {
    const html = render();
    expect(html).toContain("request served");
    expect(html).not.toContain('"msg"');
  });

  it("keeps a plain line as it was", () => {
    expect(render()).toContain("plain startup line");
  });

  it("hides the fields until the row is expanded", () => {
    const html = render();
    expect(html).not.toContain("/checkout");
    expect(html).toContain("Show fields");
  });

  // The chevron sits in a bare button, so nothing sizes the icon for it:
  // an unsized lucide SVG renders at 24px and breaks the row.
  it("sizes the chevron icon", () => {
    const svg = /<svg[^>]*>/.exec(render())?.[0] ?? "";
    expect(svg).toMatch(/class="[^"]*\bsize-3\b/);
  });

  it("offers no chevron on a line without fields", () => {
    const html = decode(
      renderToStaticMarkup(
        <AppLogView lines={[lines[0]]} follow={false} filter="" />,
      ),
    );
    expect(html).not.toContain("Show fields");
  });

  it("shows the untouched line in raw mode", () => {
    const html = render({ raw: true });
    expect(html).toContain('"msg"');
  });

  it("offers nothing to expand in raw mode: the line is already whole", () => {
    expect(render({ raw: true })).not.toContain("Show fields");
  });

  it("drops lines below the chosen level", () => {
    const html = render({ level: "error" });
    expect(html).toContain("job lost");
    expect(html).not.toContain("request served");
    expect(html).not.toContain("plain startup line");
  });

  it("filters on a field value the collapsed row does not show", () => {
    const html = render({ filter: "checkout" });
    expect(html).toContain("request served");
    expect(html).not.toContain("job lost");
  });

  it("says when the filter matched nothing", () => {
    expect(render({ filter: "nothing here" })).toContain(
      "No lines match the filter",
    );
  });
});
