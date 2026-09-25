import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AppLogView } from "@/components/log-view";
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
