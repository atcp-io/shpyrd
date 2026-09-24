import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { ago, bytes } from "@/lib/format";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

// The image registry, for platform admins (RFC-0059): where images live,
// whether it is healthy, how full it is, what it holds, when space is
// reclaimed. Project pages never show any of this.
export function RegistryCard() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["registry"],
    queryFn: api.registry,
    refetchInterval: (query) =>
      query.state.data?.gc?.running ? 5_000 : 60_000,
  });
  const gc = useMutation({
    mutationFn: api.registryGC,
    onSuccess: () => {
      toast.success(
        "Garbage collection started; the registry is read-only until it finishes",
      );
      qc.invalidateQueries({ queryKey: ["registry"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const r = q.data;
  const used = r?.storage?.usedBytes ?? 0;
  const cap = r?.storage?.capacityBytes ?? 0;
  const pct = cap > 0 ? (100 * used) / cap : 0;
  const hot = pct >= 80;

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div>
          <CardTitle>Registry</CardTitle>
          <CardDescription>
            Where built images live. Builds push to it and nodes pull from it;
            when it is down, builds and new instances on nodes without the image
            wait.
          </CardDescription>
        </div>
        {r?.mode === "in-cluster" && (
          <Button
            size="sm"
            variant="outline"
            disabled={gc.isPending || r.gc?.running}
            onClick={() => gc.mutate()}
            title="Reclaims the space of deleted images. The registry is read-only for a few minutes: pulls work, builds wait."
          >
            {r.gc?.running ? "Collecting..." : "Collect now"}
          </Button>
        )}
      </CardHeader>
      <CardContent>
        {q.isLoading && <Skeleton className="h-24 w-full" />}
        {q.error && (
          <p className="text-sm text-destructive">
            {(q.error as Error).message}
          </p>
        )}
        {r && (
          <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
            <Row label="Mode">
              {r.mode === "in-cluster" ? (
                <>
                  in-cluster{" "}
                  <span className="font-mono text-xs text-muted-foreground">
                    {r.host}
                  </span>
                  {r.tls && (
                    <span className="text-muted-foreground">
                      {" "}
                      · TLS from the platform CA
                    </span>
                  )}
                </>
              ) : (
                <>
                  external{" "}
                  <span className="font-mono text-xs text-muted-foreground">
                    {r.host}
                  </span>
                </>
              )}
            </Row>
            <Row label="Health">
              <span
                className={cn(
                  "mr-2 inline-block size-2 rounded-full",
                  r.ready ? "bg-emerald-500" : "bg-red-500",
                )}
              />
              {r.ready ? "ready" : "not ready"}
              {r.message && (
                <span className="text-muted-foreground"> · {r.message}</span>
              )}
            </Row>
            {r.storage && (
              <Row label="Storage">
                {cap > 0 ? (
                  <div className="grid gap-1">
                    <div className="flex justify-between font-mono text-xs">
                      <span className={cn(hot && "text-red-500")}>
                        {bytes(used)} of {bytes(cap)} ({pct.toFixed(0)}%)
                      </span>
                    </div>
                    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
                      <div
                        className={cn(
                          "h-full rounded-full",
                          hot ? "bg-red-500" : "bg-primary",
                        )}
                        style={{ width: `${Math.min(100, pct)}%` }}
                      />
                    </div>
                    {hot && (
                      <span className="text-[11px] text-muted-foreground">
                        Grow it with{" "}
                        <code className="font-mono">
                          shpyrd cluster init --set
                          SHPYRD_REGISTRY_SIZE=&lt;size&gt;
                        </code>
                      </span>
                    )}
                  </div>
                ) : (
                  <span className="text-muted-foreground">
                    {r.storage.size} requested; usage needs the monitoring
                    component
                  </span>
                )}
              </Row>
            )}
            {r.images && (
              <Row label="Images">
                {r.images.error && r.images.repositories === 0 ? (
                  <span className="text-muted-foreground">not available</span>
                ) : (
                  <>
                    {r.images.repositories} repositories · {r.images.tags} tags
                    {r.images.largest.length > 0 && (
                      <ul className="mt-1 grid gap-0.5 font-mono text-[11px] text-muted-foreground">
                        {r.images.largest.slice(0, 5).map((x) => (
                          <li
                            key={x.name}
                            className="flex justify-between gap-4"
                          >
                            <span className="truncate">{x.name}</span>
                            <span>{x.tags}</span>
                          </li>
                        ))}
                      </ul>
                    )}
                  </>
                )}
              </Row>
            )}
            {r.gc && (
              <Row label="Garbage collection">
                {r.gc.schedule ? (
                  <>
                    <span className="font-mono text-xs">{r.gc.schedule}</span>
                    <span className="text-muted-foreground"> UTC</span>
                    {r.gc.nextRun && (
                      <span className="text-muted-foreground">
                        {" "}
                        · next {new Date(r.gc.nextRun).toLocaleString()}
                      </span>
                    )}
                  </>
                ) : (
                  <span className="text-muted-foreground">schedule off</span>
                )}
                <div className="text-xs text-muted-foreground">
                  {r.gc.running
                    ? `running since ${r.gc.startedAt ? new Date(r.gc.startedAt).toLocaleTimeString() : ""}`
                    : r.gc.lastRun
                      ? `last ${ago(r.gc.lastRun)}: ${
                          r.gc.lastResult === "ok"
                            ? `reclaimed ${bytes(r.gc.reclaimedBytes)} in ${r.gc.lastDuration}`
                            : r.gc.lastResult
                        }`
                      : "never run"}
                </div>
              </Row>
            )}
            {r.certificate && (
              <Row label="Certificate">
                from {r.certificate.issuer}, expires{" "}
                {new Date(r.certificate.notAfter).toLocaleDateString()}
                <span className="text-muted-foreground">
                  {" "}
                  · renews automatically
                </span>
              </Row>
            )}
          </dl>
        )}
      </CardContent>
    </Card>
  );
}

function Row({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}
