import type { ProcessStatus } from "@/lib/api";
import { cn } from "@/lib/utils";

/** Compact per-process health: "web 3/3" with a coloured dot. */
export function ProcessChips({
  processes,
  compact,
}: {
  processes?: Record<string, ProcessStatus>;
  compact?: boolean;
}) {
  if (!processes || Object.keys(processes).length === 0)
    return <span className="text-xs text-muted-foreground">-</span>;
  return (
    <div className={cn("flex flex-wrap gap-1.5", compact && "gap-1")}>
      {Object.entries(processes)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([name, p]) => {
          const failing = (p.failing ?? 0) > 0;
          const updating = (p.updated ?? p.desired) < p.desired;
          const ok =
            !failing && !updating && p.desired > 0 && p.ready >= p.desired;
          const off = p.desired === 0;
          const title = failing
            ? `${name}: ${p.failing} instance(s) failing - ${p.reason ?? "see logs"}`
            : updating
              ? `${name}: ${p.updated ?? 0} of ${p.desired} instances on the new release; ${p.ready} serving`
              : `${name}: ${p.ready} of ${p.desired} instances ready`;
          return (
            <span
              key={name}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 font-mono text-xs",
                compact && "px-1.5 text-[11px]",
                off
                  ? "text-muted-foreground"
                  : failing
                    ? "border-red-500/40 bg-red-500/10"
                    : ok
                      ? "border-emerald-500/30 bg-emerald-500/10"
                      : "border-amber-500/30 bg-amber-500/10",
              )}
              title={title}
            >
              <span
                className={cn(
                  "size-1.5 rounded-full",
                  off
                    ? "bg-muted-foreground"
                    : failing
                      ? "bg-red-500"
                      : ok
                        ? "bg-emerald-500"
                        : "animate-pulse bg-amber-500",
                )}
              />
              {name}
              <span className="text-muted-foreground">
                {updating
                  ? `${p.updated ?? 0}/${p.desired} updated`
                  : `${p.ready}/${p.desired}`}
              </span>
              {failing && (
                <span className="text-red-500">{p.failing} failing</span>
              )}
            </span>
          );
        })}
    </div>
  );
}
