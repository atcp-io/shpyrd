import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

const styles: Record<string, string> = {
  Running:
    "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border-emerald-500/30",
  Building: "bg-sky-500/15 text-sky-700 dark:text-sky-400 border-sky-500/30",
  Deploying:
    "bg-amber-500/15 text-amber-700 dark:text-amber-400 border-amber-500/30",
  Pending: "bg-muted text-muted-foreground border-border",
  Failed: "bg-red-500/15 text-red-700 dark:text-red-400 border-red-500/30",
};

export function PhaseBadge({ phase }: { phase?: string }) {
  const p = phase || "Pending";
  const live = p === "Building" || p === "Deploying";
  return (
    <Badge
      variant="outline"
      className={cn("gap-1.5 font-medium", styles[p] ?? styles.Pending)}
    >
      <span
        className={cn(
          "inline-block size-1.5 rounded-full bg-current",
          live && "animate-pulse",
        )}
      />
      {p}
    </Badge>
  );
}
