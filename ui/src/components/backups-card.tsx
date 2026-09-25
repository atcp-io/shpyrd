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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

/**
 * Platform backups (RFC-0037): encrypted archives of the platform's state in
 * the provider's object storage, on a schedule; a button runs one now. The
 * passphrase stays out of the browser (`shpyrd cluster backup key`).
 */
export function BackupsCard() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["backups"],
    queryFn: api.backups,
    refetchInterval: (query) =>
      query.state.data?.runs.some((r) => r.status === "running")
        ? 5_000
        : 60_000,
  });
  const run = useMutation({
    mutationFn: api.runBackup,
    onSuccess: () => {
      toast.success("Backup started");
      qc.invalidateQueries({ queryKey: ["backups"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const b = q.data;
  const running = b?.runs.some((r) => r.status === "running") ?? false;
  const lastRun = b?.runs[0];

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div className="grid gap-1.5">
          <CardTitle>Platform backups</CardTitle>
          <CardDescription>
            Projects, config vars, resources, sources, users and teams, as an
            encrypted archive in the provider&apos;s object storage. A new
            cluster restores from it with{" "}
            <code className="text-xs">shpyrd cluster restore</code>.
          </CardDescription>
        </div>
        {b?.enabled && (
          <Button
            size="sm"
            variant="outline"
            disabled={running || run.isPending}
            onClick={() => run.mutate()}
          >
            {running ? "Running…" : "Back up now"}
          </Button>
        )}
      </CardHeader>
      <CardContent className="grid gap-4">
        {q.isLoading && <Skeleton className="h-16 w-full" />}
        {q.error && (
          <p className="text-sm text-destructive">
            {(q.error as Error).message}
          </p>
        )}
        {b && !b.enabled && (
          <p className="text-sm text-muted-foreground">
            Not set up. Give{" "}
            <code className="text-xs">shpyrd cluster init</code> a target:{" "}
            <code className="text-xs">--backup-target s3://bucket/prefix</code>{" "}
            (contrib/&lt;provider&gt;/terraform/backups creates the bucket).
          </p>
        )}
        {b?.enabled && (
          <>
            <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
              <div>
                <dt className="text-xs text-muted-foreground">Target</dt>
                <dd className="font-mono text-xs">{b.target}</dd>
              </div>
              <div>
                <dt className="text-xs text-muted-foreground">Schedule</dt>
                <dd>
                  <span className="font-mono text-xs">{b.schedule}</span> UTC,
                  keeping {b.keep}
                </dd>
              </div>
              <div>
                <dt className="text-xs text-muted-foreground">
                  Last good backup
                </dt>
                <dd>{b.lastSuccessful ? ago(b.lastSuccessful) : "none yet"}</dd>
              </div>
              <div>
                <dt className="text-xs text-muted-foreground">Last run</dt>
                <dd>
                  {lastRun ? (
                    <>
                      <span
                        className={
                          lastRun.status === "failed"
                            ? "text-destructive"
                            : lastRun.status === "running"
                              ? "text-muted-foreground"
                              : ""
                        }
                      >
                        {lastRun.status}
                      </span>
                      {lastRun.started ? ` · ${ago(lastRun.started)}` : ""}
                      {lastRun.message ? ` · ${lastRun.message}` : ""}
                    </>
                  ) : (
                    "none yet"
                  )}
                </dd>
              </div>
            </dl>
            {b.error && (
              <p className="text-sm text-destructive">
                Cannot list the target: {b.error}
              </p>
            )}
            {!b.error && b.archives.length === 0 && (
              <p className="text-sm text-muted-foreground">No backups yet.</p>
            )}
            {b.archives.length > 0 && (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Archive</TableHead>
                    <TableHead className="text-right">Size</TableHead>
                    <TableHead className="text-right">Created</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {b.archives.slice(0, 7).map((a) => (
                    <TableRow key={a.name}>
                      <TableCell className="font-mono text-xs">
                        {a.name}
                      </TableCell>
                      <TableCell className="text-right">
                        {bytes(a.size)}
                      </TableCell>
                      <TableCell className="text-right text-muted-foreground">
                        {ago(a.modified)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
            <p className="text-xs text-muted-foreground">
              The passphrase that opens these archives is printed by{" "}
              <code>shpyrd cluster backup key</code>; keep it outside the
              cluster. Volume and database contents are not in a backup.
            </p>
          </>
        )}
      </CardContent>
    </Card>
  );
}
