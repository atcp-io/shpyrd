import { useQuery } from "@tanstack/react-query";

import { api } from "@/lib/api";
import { bytes } from "@/lib/format";
import { cn } from "@/lib/utils";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

/**
 * The platform's object store (RFC-0046): capacity of the volume and the
 * buckets extensions declared, each with its own credential.
 */
export function ObjectStorageCard() {
  const q = useQuery({
    queryKey: ["object-storage"],
    queryFn: api.objectStorage,
    refetchInterval: 60_000,
  });
  const r = q.data;
  const used = r?.usedBytes ?? 0;
  const cap = r?.totalBytes ?? 0;
  const pct = cap > 0 ? (100 * used) / cap : 0;
  const hot = pct >= 80;

  return (
    <Card>
      <CardHeader>
        <CardTitle>Object storage</CardTitle>
        <CardDescription>
          The platform&apos;s S3-compatible store (Garage). Extensions that need
          durable storage — database backups, platform backups — get a bucket
          here with a credential that opens only that bucket.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {q.isLoading && <Skeleton className="h-16 w-full" />}
        {q.error && (
          <p className="text-sm text-destructive">
            {(q.error as Error).message}
          </p>
        )}
        {r && (
          <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
            <div>
              <dt className="text-xs text-muted-foreground">Endpoint</dt>
              <dd className="font-mono text-xs">{r.endpoint}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Volume</dt>
              <dd>
                {cap > 0 ? (
                  <div className="grid gap-1">
                    <span
                      className={cn("font-mono text-xs", hot && "text-red-500")}
                    >
                      {bytes(used)} of {bytes(cap)} ({pct.toFixed(0)}%)
                    </span>
                    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
                      <div
                        className={cn(
                          "h-full rounded-full",
                          hot ? "bg-red-500" : "bg-primary",
                        )}
                        style={{ width: `${Math.min(100, pct)}%` }}
                      />
                    </div>
                  </div>
                ) : (
                  <span className="text-muted-foreground">
                    {r.message || "measuring"}
                  </span>
                )}
              </dd>
            </div>
          </dl>
        )}
        {r && r.buckets.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No buckets yet. They appear when an extension needs storage.
          </p>
        )}
        {r && r.buckets.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Bucket</TableHead>
                <TableHead>Owner</TableHead>
                <TableHead>Used</TableHead>
                <TableHead>Objects</TableHead>
                <TableHead>Retention</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {r.buckets.map((b) => (
                <TableRow key={b.namespace + "/" + b.name}>
                  <TableCell className="font-mono text-xs">
                    {b.bucket || "-"}
                  </TableCell>
                  <TableCell className="text-xs">
                    {b.namespace}/{b.name}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {bytes(b.usedBytes)}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {b.objects}
                  </TableCell>
                  <TableCell className="text-xs">
                    {b.retentionDays ? `${b.retentionDays} days` : "keep"}
                  </TableCell>
                  <TableCell className="text-xs" title={b.message}>
                    <span
                      className={cn(
                        b.phase === "Ready" && "text-emerald-500",
                        b.phase === "Failed" && "text-destructive",
                      )}
                    >
                      {b.phase}
                    </span>
                    {b.message && (
                      <span className="text-muted-foreground">
                        {" "}
                        · {b.message}
                      </span>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
