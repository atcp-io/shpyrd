// @vitest-environment jsdom
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { AppLogView } from "@/components/log-view";
import type { LogLine } from "@/lib/api";

afterEach(cleanup);

const lines: LogLine[] = [
  {
    t: "2026-09-26T07:15:31Z",
    i: "worker.1",
    p: "worker",
    m: '{"level":"error","msg":"job lost","job":{"id":"j-12","queue":"mail"},"meta":{"deep":{"deeper":"bottom"}}}',
  },
];

describe("expanding a line in the viewer", () => {
  it("opens the row, then the object inside it, then the object inside that", async () => {
    const user = userEvent.setup();
    render(<AppLogView lines={lines} follow={false} filter="" />);

    // Closed: the fields are not there at all.
    expect(screen.queryByText("job")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Show fields" }));
    expect(screen.getByText("job")).toBeTruthy();
    // The object reads as one line until it is opened.
    expect(screen.getByText('{"id":"j-12","queue":"mail"}')).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Show job" }));
    expect(screen.getByText("j-12")).toBeTruthy();
    expect(screen.getByText("queue")).toBeTruthy();
    expect(screen.queryByText('{"id":"j-12","queue":"mail"}')).toBeNull();

    // And one level further down.
    await user.click(screen.getByRole("button", { name: "Show meta" }));
    await user.click(screen.getByRole("button", { name: "Show deep" }));
    expect(screen.getByText("bottom")).toBeTruthy();
  });

  it("closes what it opened", async () => {
    const user = userEvent.setup();
    render(<AppLogView lines={lines} follow={false} filter="" />);

    await user.click(screen.getByRole("button", { name: "Show fields" }));
    await user.click(screen.getByRole("button", { name: "Show job" }));
    expect(screen.getByText("j-12")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Hide job" }));
    expect(screen.queryByText("j-12")).toBeNull();
    expect(screen.getByText('{"id":"j-12","queue":"mail"}')).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Hide fields" }));
    expect(screen.queryByText("job")).toBeNull();
  });

  it("opens the same field on one line without opening it on another", async () => {
    const user = userEvent.setup();
    const two: LogLine[] = [
      { ...lines[0], t: "2026-09-26T07:15:31Z" },
      { ...lines[0], t: "2026-09-26T07:15:32Z" },
    ];
    render(<AppLogView lines={two} follow={false} filter="" />);

    const rows = screen.getAllByRole("button", { name: "Show fields" });
    expect(rows).toHaveLength(2);
    await user.click(rows[0]);
    // Only the first line's fields opened.
    expect(screen.getAllByText("job")).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Show job" }));
    expect(screen.getAllByText("j-12")).toHaveLength(1);
    expect(
      screen.getAllByRole("button", { name: "Show fields" }),
    ).toHaveLength(1);
  });
});

// Keeps the import used when the file is read in isolation.
export const _ = within;
