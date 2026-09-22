import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  ArrowLeft,
  ExternalLink,
  Hammer,
  KeyRound,
  HardDrive,
  Loader2,
  ScrollText,
  UsersRound,
  Minus,
  Plus,
  RefreshCw,
  Rocket,
  Trash2,
  Undo2,
  X,
} from "lucide-react";
import { toast } from "sonner";
import type { VolumeInfo, Member, AuditEntry } from "@/lib/api";
import { usePerms } from "@/lib/me";
import { api, apiStream, type AppDetail, type BuildInfo } from "@/lib/api";
import { ago, duration } from "@/lib/format";
import { PhaseBadge } from "@/components/phase-badge";
import { ProcessChips } from "@/components/process-chips";
import { MetricChart } from "@/components/metric-chart";
import { AppLogView, TextLogView, useLogStream } from "@/components/log-view";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

export function AppDetailPage() {
  const { ns = "", name = "" } = useParams();
  const qc = useQueryClient();
  const app = useQuery({
    queryKey: ["app", ns, name],
    queryFn: () => api.app(ns, name),
    refetchInterval: 4000,
  });
  const perms = usePerms(name);

  if (app.isLoading) {
    return (
      <div className="grid gap-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }
  if (app.error || !app.data) {
    return (
      <Alert variant="destructive">
        <AlertTitle>Could not load project</AlertTitle>
        <AlertDescription>
          {(app.error as Error)?.message ?? "not found"}
        </AlertDescription>
      </Alert>
    );
  }
  const a = app.data;
  const refresh = () => qc.invalidateQueries({ queryKey: ["app", ns, name] });
  const building = a.status.phase === "Building";
  const busy = isBusy(a);

  return (
    <div className="grid gap-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <Link
            to="/"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:underline"
          >
            <ArrowLeft className="size-3" /> Projects
          </Link>
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-semibold tracking-tight">{a.name}</h1>
            <PhaseBadge phase={a.status.phase} />
          </div>
          <div className="flex flex-wrap items-center gap-3 pt-1">
            <ProcessChips processes={a.processes} />
            {a.status.url && (
              <a
                href={a.status.url}
                target="_blank"
                rel="noreferrer"
                className="text-xs text-muted-foreground hover:underline"
              >
                {a.status.url.replace(/^https:\/\//, "")}
              </a>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2">
          {a.status.url && (
            <Button asChild variant="outline" size="sm">
              <a href={a.status.url} target="_blank" rel="noreferrer">
                Open <ExternalLink data-icon="inline-end" />
              </a>
            </Button>
          )}
          {perms.deploy && (
            <DeployDialog app={a} onDone={refresh} disabled={busy} />
          )}
          {perms.destroy && <DestroyDialog app={a} />}
        </div>
      </div>

      <ActivityPanel app={a} onChanged={refresh} />
      {building && <BuildBanner app={a} />}

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="metrics">Metrics</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="builds">Builds</TabsTrigger>
          <TabsTrigger value="config">Config</TabsTrigger>
        </TabsList>
        <TabsContent value="overview" className="grid gap-6 pt-4">
          <Overview app={a} onChanged={refresh} />
        </TabsContent>
        <TabsContent value="metrics" className="pt-4">
          <Metrics app={a} />
        </TabsContent>
        <TabsContent value="logs" className="pt-4">
          <Logs app={a} />
        </TabsContent>
        <TabsContent value="builds" className="pt-4">
          <Builds app={a} />
        </TabsContent>
        <TabsContent value="config" className="pt-4">
          <Config app={a} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

/** Streams a text endpoint into a lines state, batching updates (100 ms) so
 * long outputs do not re-render per line. Returns the effect cleanup. */
function streamText(
  path: string,
  ac: AbortController,
  setLines: (f: (prev: string[]) => string[]) => void,
  onError?: (e: Error) => void,
) {
  let first = true;
  let pending: string[] = [];
  let timer: ReturnType<typeof setTimeout> | null = null;
  const flush = () => {
    timer = null;
    if (pending.length === 0) return;
    const batch = pending;
    pending = [];
    setLines((prev) => {
      const base = first ? [] : prev;
      first = false;
      return base.concat(batch);
    });
  };
  apiStream(path, ac.signal, (l) => {
    pending.push(l);
    if (!timer) timer = setTimeout(flush, 100);
  })
    .then(flush)
    .catch((e: Error) => {
      if (e.name !== "AbortError") onError?.(e);
    });
  return () => {
    ac.abort();
    if (timer) clearTimeout(timer);
  };
}

// ---- activity: what is happening right now ------------------------------------

/** A release is in flight: building or rolling out. Actions that would start
 * another release are disabled meanwhile. */
function isBusy(app: AppDetail): boolean {
  return app.status.phase === "Building" || app.status.phase === "Deploying";
}

function ActivityPanel({
  app,
  onChanged,
}: {
  app: AppDetail;
  onChanged: () => void;
}) {
  const perms = usePerms(app.name);
  const current = app.status.releases[app.status.releases.length - 1];
  const previous = app.status.releases[app.status.releases.length - 2];
  const phase = app.status.phase;
  const procs = Object.entries(app.processes ?? {}).sort(([a], [b]) =>
    a.localeCompare(b),
  );
  const rollback = useMutation({
    mutationFn: (n: number) => api.rollback(app.namespace, app.name, n),
    onSuccess: (_, n) => {
      toast.success(`Rolling back to v${n}`);
      onChanged();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  if (phase === "Pending") {
    return (
      <Alert>
        <Rocket className="size-4" />
        <AlertTitle>Nothing deployed yet</AlertTitle>
        <AlertDescription>
          Use <strong>Deploy</strong> to build from a Git repository, or run{" "}
          <code className="font-mono text-xs">
            shpyrd deploy --project {app.name}
          </code>{" "}
          from a checkout.
        </AlertDescription>
      </Alert>
    );
  }
  if (phase === "Deploying") {
    return (
      <Card className="border-amber-500/30">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <Loader2 className="size-4 animate-spin text-amber-500" />
            Rolling out {current ? `v${current.number}` : ""}
            {current?.description && (
              <span className="font-normal text-muted-foreground">
                — {current.description}
              </span>
            )}
          </CardTitle>
          <CardDescription>
            New instances start one by one; previous instances keep serving
            until the new ones are ready.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2">
          {procs.map(([name, p]) => (
            <RolloutRow key={name} name={name} status={p} />
          ))}
        </CardContent>
      </Card>
    );
  }
  if (phase === "Failed") {
    return (
      <Card className="border-red-500/40">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm text-red-500">
            <AlertTriangle className="size-4" /> Release{" "}
            {current ? `v${current.number}` : ""} is not healthy
          </CardTitle>
          <CardDescription className="break-words">
            {app.status.message}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-center gap-3">
          {procs.map(([name, p]) => (
            <RolloutRow key={name} name={name} status={p} />
          ))}
          {previous && (
            <Button
              size="sm"
              variant="outline"
              className="ml-auto"
              disabled={rollback.isPending || !perms.deploy}
              onClick={() => rollback.mutate(previous.number)}
            >
              <Undo2 data-icon="inline-start" /> Roll back to v{previous.number}
            </Button>
          )}
        </CardContent>
      </Card>
    );
  }
  return null;
}

function RolloutRow({
  name,
  status,
}: {
  name: string;
  status: {
    desired: number;
    ready: number;
    updated?: number;
    failing?: number;
    reason?: string;
  };
}) {
  const updated = status.updated ?? 0;
  const pct = status.desired
    ? Math.round((Math.min(updated, status.desired) / status.desired) * 100)
    : 100;
  const failing = (status.failing ?? 0) > 0;
  return (
    <div className="grid min-w-56 flex-1 gap-1 text-xs">
      <div className="flex justify-between">
        <span className="font-medium">{name}</span>
        <span
          className={cn(
            "font-mono text-muted-foreground",
            failing && "text-red-500",
          )}
        >
          {failing
            ? `${status.failing} failing`
            : `${updated}/${status.desired} on new release · ${status.ready} serving`}
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div
          className={cn(
            "h-full rounded-full transition-all",
            failing ? "bg-red-500" : "bg-amber-500",
          )}
          style={{ width: `${pct}%` }}
        />
      </div>
      {failing && status.reason && (
        <span className="break-words text-[11px] text-red-500/80">
          {status.reason}
        </span>
      )}
    </div>
  );
}

// ---- build banner (live build output while Building) -------------------------

function BuildBanner({ app }: { app: AppDetail }) {
  const build = app.status.latestBuild;
  const [lines, setLines] = useState<string[]>([]);
  useEffect(() => {
    if (!build) return;
    const ac = new AbortController();
    return streamText(
      api.buildLogsPath(app.namespace, app.name, build, true),
      ac,
      setLines,
    );
  }, [app.namespace, app.name, build]);
  return (
    <Card className="border-sky-500/30">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <Hammer className="size-4 animate-pulse text-sky-400" /> Building{" "}
          {build ? (
            <span className="font-mono text-xs text-muted-foreground">
              {build}
            </span>
          ) : null}
        </CardTitle>
        <CardDescription>{app.status.message}</CardDescription>
      </CardHeader>
      <CardContent>
        <TextLogView
          lines={lines}
          className="h-64"
          empty={
            build
              ? "Waiting for the build to start..."
              : "Waiting for kpack to schedule the build..."
          }
        />
      </CardContent>
    </Card>
  );
}

// ---- overview -----------------------------------------------------------------

function Overview({
  app,
  onChanged,
}: {
  app: AppDetail;
  onChanged: () => void;
}) {
  const processes = Object.keys({
    ...(app.spec.processes ?? { web: {} }),
    ...(app.processes ?? {}),
  }).sort();
  const releases = [...app.status.releases].reverse();
  const current = releases[0];
  const busy = isBusy(app);
  const perms = usePerms(app.name);

  const rollback = useMutation({
    mutationFn: (n: number) => api.rollback(app.namespace, app.name, n),
    onSuccess: (_, n) => {
      toast.success(`Rolling back to v${n} (build and config)`);
      onChanged();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <>
      <div className="grid gap-4 md:grid-cols-3">
        <Card size="sm">
          <CardHeader>
            <CardTitle className="text-sm">Source</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-1 text-sm">
            {app.spec.source?.git && (
              <>
                <Row k="Git" v={app.spec.source.git.url} mono />
                <Row
                  k="Revision"
                  v={app.spec.source.git.revision || "main"}
                  mono
                />
              </>
            )}
            {app.spec.source?.blob && (
              <>
                <Row
                  k="Archive"
                  v={app.spec.source.blob.sha256?.slice(0, 12) ?? "-"}
                  mono
                />
                <Row
                  k="Commit"
                  v={app.spec.source.blob.ref || "local checkout"}
                  mono
                />
              </>
            )}
            {app.spec.source?.subPath && (
              <Row k="Directory" v={app.spec.source.subPath} mono />
            )}
            {app.spec.source && (
              <Row
                k="Build"
                v={
                  app.spec.build?.strategy === "dockerfile"
                    ? `Dockerfile (${app.spec.build.dockerfile || "Dockerfile"})`
                    : "Buildpacks"
                }
              />
            )}
            {!app.spec.source && !app.spec.pinnedDigest && (
              <p className="text-muted-foreground">
                No source yet. Use <strong>Deploy</strong> above or{" "}
                <code className="font-mono text-xs">
                  shpyrd deploy --project {app.name}
                </code>
                .
              </p>
            )}
            {app.spec.pinnedDigest && (
              <Row k="Pinned build" v={app.spec.pinnedDigest} mono />
            )}
            {app.spec.build?.env?.length ? (
              <Row
                k="Build env"
                v={app.spec.build.env
                  .map((e) => `${e.name}=${e.value ?? ""}`)
                  .join(" ")}
                mono
              />
            ) : null}
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardTitle className="text-sm">Release</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-1 text-sm">
            <Row
              k="Current"
              v={
                current
                  ? `v${current.number} · ${current.description ?? ""}`
                  : "-"
              }
            />
            <Row
              k="Build"
              v={
                current?.build
                  ? `#${current.build}`
                  : app.status.digest?.slice(0, 7) || "-"
              }
              mono
            />
            <Row
              k="Domains"
              v={(app.spec.domains?.length
                ? app.spec.domains
                : [app.status.url?.replace(/^https:\/\//, "") ?? "-"]
              ).join(", ")}
              mono
            />
          </CardContent>
        </Card>
        <ProcessesCard app={app} processes={processes} onChanged={onChanged} />
      </div>

      <ResourcesCard app={app} onChanged={onChanged} />

      <div className="grid gap-4 lg:grid-cols-2">
        {perms.members && <MembersCard app={app} />}
        <AuditCard app={app} />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Releases</CardTitle>
          <CardDescription>
            A release is a build plus its config vars. Deploys create new
            builds; config changes and rollbacks reuse existing ones. Rollback
            re-releases an earlier release exactly as it was.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {releases.length === 0 ? (
            <p className="text-sm text-muted-foreground">No releases yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Release</TableHead>
                  <TableHead>What changed</TableHead>
                  <TableHead>Build</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {releases.map((r, i) => {
                  const currentProcs = Object.keys(
                    app.spec.processes ?? { web: {} },
                  );
                  const missing = r.processes
                    ? currentProcs.filter((p) => !r.processes!.includes(p))
                    : [];
                  return (
                    <TableRow key={r.number}>
                      <TableCell className="font-mono text-xs">
                        v{r.number}{" "}
                        {i === 0 && (
                          <Badge variant="secondary" className="ml-1">
                            current
                          </Badge>
                        )}
                      </TableCell>
                      <TableCell className="text-sm">
                        <ReleaseKind kind={r.kind} />
                        {r.description}
                        {r.processes && r.processes.length > 0 && (
                          <span className="ml-2 font-mono text-[10px] text-muted-foreground">
                            {r.processes.join(" · ")}
                          </span>
                        )}
                        {i !== 0 && missing.length > 0 && (
                          <span
                            className="ml-2 text-[10px] text-amber-500"
                            title={`This build has no ${missing.join(", ")} process; those instances would fail to start`}
                          >
                            no {missing.join(", ")} process
                          </span>
                        )}
                      </TableCell>
                      <TableCell
                        className="font-mono text-xs text-muted-foreground"
                        title={
                          r.digest ? `image digest ${r.digest}` : undefined
                        }
                      >
                        {r.build
                          ? `#${r.build}`
                          : r.digest
                            ? r.digest.slice(0, 7)
                            : "-"}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {ago(r.createdAt)}
                      </TableCell>
                      <TableCell className="text-right">
                        <Button
                          variant="outline"
                          size="xs"
                          disabled={
                            i === 0 ||
                            busy ||
                            rollback.isPending ||
                            !perms.deploy
                          }
                          title={
                            !perms.deploy
                              ? "Your role cannot roll back"
                              : i === 0
                                ? "Current release"
                                : busy
                                  ? "Wait for the current release to finish rolling out"
                                  : missing.length
                                    ? `Re-release v${r.number}; its build has no ${missing.join(", ")} process`
                                    : `Re-release v${r.number}: its build and its config vars`
                          }
                          onClick={() => rollback.mutate(r.number)}
                        >
                          Rollback
                        </Button>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </>
  );
}

function ReleaseKind({ kind }: { kind: string }) {
  const cls =
    kind === "rollback"
      ? "border-violet-500/30 bg-violet-500/10 text-violet-600 dark:text-violet-300"
      : kind === "config"
        ? "border-sky-500/30 bg-sky-500/10 text-sky-600 dark:text-sky-300"
        : "border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300";
  return (
    <Badge
      variant="outline"
      className={cn("mr-2 text-[10px] uppercase tracking-wide", cls)}
    >
      {kind}
    </Badge>
  );
}

function Row({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <span className="shrink-0 text-muted-foreground">{k}</span>
      <span
        className={cn("truncate text-right", mono && "font-mono text-xs")}
        title={v}
      >
        {v}
      </span>
    </div>
  );
}

type ProcessDraft = { size: string; replicas: number };

/** Sizes and instance counts are edited as a draft and applied together, so
 * a batch of changes yields one release and one rollout. */
function ProcessesCard({
  app,
  processes,
  onChanged,
}: {
  app: AppDetail;
  processes: string[];
  onChanged: () => void;
}) {
  const catalog = useQuery({
    queryKey: ["sizes"],
    queryFn: api.sizes,
    staleTime: 60_000,
  });
  const current = (p: string): ProcessDraft => ({
    size:
      app.spec.processes?.[p]?.size ||
      app.processes?.[p]?.size ||
      catalog.data?.default ||
      "",
    replicas:
      app.spec.processes?.[p]?.replicas ?? app.processes?.[p]?.desired ?? 1,
  });
  const [draft, setDraft] = useState<Record<string, ProcessDraft>>({});
  const value = (p: string): ProcessDraft => draft[p] ?? current(p);
  const changes = Object.fromEntries(
    processes
      .map((p) => {
        const cur = current(p);
        const v = value(p);
        const change: { size?: string; replicas?: number } = {};
        if (v.size && v.size !== cur.size && v.size !== "custom")
          change.size = v.size;
        if (v.replicas !== cur.replicas) change.replicas = v.replicas;
        return [p, change] as const;
      })
      .filter(([, ch]) => Object.keys(ch).length > 0),
  );
  const dirty = Object.keys(changes).length > 0;
  const busy = isBusy(app);
  const apply = useMutation({
    mutationFn: () => api.applyProcesses(app.namespace, app.name, changes),
    onSuccess: () => {
      const parts = Object.entries(changes).map(([p, ch]) =>
        [
          p,
          ch.size ? `→ ${ch.size}` : "",
          ch.replicas !== undefined ? `×${ch.replicas}` : "",
        ]
          .filter(Boolean)
          .join(" "),
      );
      toast.success(`Applying: ${parts.join(", ")}`);
      setDraft({});
      onChanged();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Card size="sm" className={cn(dirty && "ring-primary/40")}>
      <CardHeader>
        <CardTitle className="text-sm">Processes</CardTitle>
        <CardDescription>
          Instances and size per process type. Edit, then apply everything at
          once.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        {processes.map((p) => {
          const st = app.processes?.[p];
          const spec = app.spec.processes?.[p];
          const v = value(p);
          const cur = current(p);
          const ok = st ? st.ready >= st.desired && st.desired > 0 : false;
          const changed =
            draft[p] !== undefined &&
            (v.size !== cur.size || v.replicas !== cur.replicas);
          return (
            <div
              key={p}
              className={cn(
                "grid gap-1.5 rounded-md px-2 py-1.5 text-sm",
                changed && "bg-primary/5",
              )}
            >
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <span
                    className={cn(
                      "size-2 rounded-full",
                      !st || st.desired === 0
                        ? "bg-muted-foreground"
                        : ok
                          ? "bg-emerald-500"
                          : "animate-pulse bg-amber-500",
                    )}
                  />
                  <span className="font-medium">{p}</span>
                  {spec?.port && (
                    <span className="font-mono text-[10px] text-muted-foreground">
                      :{spec.port}
                    </span>
                  )}
                  <span className="text-xs text-muted-foreground">
                    {st
                      ? `${st.ready} of ${st.desired} running`
                      : "not deployed"}
                  </span>
                </div>
                <div className="flex items-center gap-1">
                  <Button
                    variant="outline"
                    size="icon-xs"
                    disabled={v.replicas <= 0 || busy}
                    onClick={() =>
                      setDraft({
                        ...draft,
                        [p]: { ...v, replicas: v.replicas - 1 },
                      })
                    }
                  >
                    <Minus />
                  </Button>
                  <span
                    className={cn(
                      "w-6 text-center font-mono text-xs",
                      v.replicas !== cur.replicas && "text-primary",
                    )}
                  >
                    {v.replicas}
                  </span>
                  <Button
                    variant="outline"
                    size="icon-xs"
                    disabled={
                      busy || (!!app.processes?.[p]?.pinned && v.replicas >= 1)
                    }
                    title={
                      app.processes?.[p]?.pinned
                        ? `${app.processes[p].pinned}: one instance`
                        : undefined
                    }
                    onClick={() =>
                      setDraft({
                        ...draft,
                        [p]: { ...v, replicas: v.replicas + 1 },
                      })
                    }
                  >
                    <Plus />
                  </Button>
                </div>
              </div>
              <div className="flex items-center justify-between gap-2 pl-4">
                <Select
                  value={v.size}
                  onValueChange={(size) =>
                    setDraft({ ...draft, [p]: { ...v, size } })
                  }
                  disabled={busy || !catalog.data}
                >
                  <SelectTrigger
                    className={cn(
                      "h-7 w-44 text-xs",
                      v.size !== cur.size && "border-primary text-primary",
                    )}
                    size="sm"
                  >
                    <SelectValue placeholder="size" />
                  </SelectTrigger>
                  <SelectContent>
                    {catalog.data?.sizes.map((sz) => (
                      <SelectItem key={sz.name} value={sz.name}>
                        <span className="font-mono text-xs">{sz.name}</span>
                        <span className="ml-2 text-xs text-muted-foreground">
                          {sz.cpu} CPU · {sz.memory}
                        </span>
                      </SelectItem>
                    ))}
                    {cur.size === "custom" && (
                      <SelectItem value="custom">custom</SelectItem>
                    )}
                  </SelectContent>
                </Select>
                <span className="font-mono text-[11px] text-muted-foreground">
                  {st?.cpu && st?.memory ? `${st.cpu} CPU · ${st.memory}` : ""}
                </span>
              </div>
            </div>
          );
        })}
        {dirty && (
          <div className="flex items-center justify-between gap-2 rounded-md border border-primary/40 bg-primary/5 px-3 py-2 text-xs">
            <span>
              {Object.keys(changes).length} process
              {Object.keys(changes).length > 1 ? "es" : ""} changed
              {Object.values(changes).some((c) => c.size)
                ? " · a new release will roll out"
                : ""}
            </span>
            <div className="flex gap-2">
              <Button
                size="xs"
                variant="outline"
                onClick={() => setDraft({})}
                disabled={apply.isPending}
              >
                Cancel
              </Button>
              <Button
                size="xs"
                onClick={() => apply.mutate()}
                disabled={busy || apply.isPending}
                title={
                  busy ? "Wait for the current release to finish" : undefined
                }
              >
                Apply
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// ---- metrics ------------------------------------------------------------------

function Metrics({ app }: { app: AppDetail }) {
  const [range, setRange] = useState("1h");
  const config = useQuery({
    queryKey: ["config"],
    queryFn: api.config,
    staleTime: 60_000,
  });
  const m = useQuery({
    queryKey: ["metrics", app.namespace, app.name, range],
    queryFn: () => api.metrics(app.namespace, app.name, range),
    refetchInterval: 30_000,
    enabled: config.data?.metrics !== false,
  });
  if (config.data && !config.data.metrics) {
    return (
      <Alert>
        <AlertTitle>Metrics disabled</AlertTitle>
        <AlertDescription>
          The server has no Prometheus configured (install the monitoring
          component).
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div className="grid gap-4">
      <div className="flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">
          Traffic measured at the edge; CPU and memory as a percentage of each
          process allocation. Orange dashed lines mark releases, the red line is
          100%.
        </p>
        <Select value={range} onValueChange={setRange}>
          <SelectTrigger className="w-32" size="sm">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="1h">Last hour</SelectItem>
            <SelectItem value="6h">Last 6 hours</SelectItem>
            <SelectItem value="24h">Last 24 hours</SelectItem>
            <SelectItem value="7d">Last 7 days</SelectItem>
          </SelectContent>
        </Select>
      </div>
      {m.error && (
        <Alert variant="destructive">
          <AlertTitle>Could not load metrics</AlertTitle>
          <AlertDescription>{(m.error as Error).message}</AlertDescription>
        </Alert>
      )}
      <div className="grid gap-4 md:grid-cols-2">
        {(m.data?.charts ?? []).map((c) => (
          <MetricChart
            key={c.id}
            chart={c}
            range={range}
            releases={m.data?.releases}
          />
        ))}
        {m.isLoading &&
          [0, 1, 2, 3].map((i) => <Skeleton key={i} className="h-60 w-full" />)}
      </div>
    </div>
  );
}

// ---- logs ---------------------------------------------------------------------

function Logs({ app }: { app: AppDetail }) {
  const processes = Object.keys(
    app.processes ?? app.spec.processes ?? { web: {} },
  ).sort();
  const [process, setProcess] = useState<string>("all");
  const [follow, setFollow] = useState(true);
  const [filter, setFilter] = useState("");
  const path = useMemo(
    () =>
      api.logsPath(app.namespace, app.name, {
        process: process === "all" ? undefined : process,
        tail: 300,
        follow,
      }),
    [app.namespace, app.name, process, follow],
  );
  const { lines, error, restart } = useLogStream(path, []);

  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={process} onValueChange={setProcess}>
          <SelectTrigger className="w-40" size="sm">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All processes</SelectItem>
            {processes.map((p) => (
              <SelectItem key={p} value={p}>
                {p}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter lines..."
          className="h-7 w-56 text-xs"
        />
        <Button
          variant={follow ? "default" : "outline"}
          size="sm"
          onClick={() => setFollow((f) => !f)}
        >
          {follow ? "Live" : "Paused"}
        </Button>
        <Button variant="outline" size="sm" onClick={restart}>
          <RefreshCw data-icon="inline-start" /> Reload
        </Button>
        <span className="ml-auto text-xs text-muted-foreground">
          {lines.length} lines
        </span>
      </div>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <AppLogView
        lines={lines}
        follow={follow}
        filter={filter}
        empty={error ? "" : "Waiting for log lines..."}
      />
    </div>
  );
}

// ---- builds -------------------------------------------------------------------

function Builds({ app }: { app: AppDetail }) {
  const builds = useQuery({
    queryKey: ["builds", app.namespace, app.name],
    queryFn: () => api.builds(app.namespace, app.name),
    refetchInterval: app.status.phase === "Building" ? 5000 : 30_000,
  });
  const [selected, setSelected] = useState<string | null>(null);
  const list = builds.data ?? [];
  const active = list.find((b) => b.name === selected) ?? list[0];
  const [lines, setLines] = useState<string[]>([]);
  const activeName = active?.name;
  const activeStatus = active?.status;
  useEffect(() => {
    if (!activeName) return;
    const ac = new AbortController();
    return streamText(
      api.buildLogsPath(
        app.namespace,
        app.name,
        activeName,
        activeStatus === "Building",
      ),
      ac,
      setLines,
      (e) => setLines([`(${e.message})`]),
    );
  }, [app.namespace, app.name, activeName, activeStatus]);

  return (
    <div className="grid gap-4 lg:grid-cols-[20rem_1fr]">
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-sm">Builds</CardTitle>
          <CardDescription>
            Each deploy compiles the source into a build; releases (including
            config changes and rollbacks) run one of these builds.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-1">
          {list.length === 0 && (
            <p className="text-sm text-muted-foreground">No builds yet.</p>
          )}
          {list.map((b) => (
            <BuildRow
              key={b.name}
              build={b}
              active={b.name === active?.name}
              onSelect={() => setSelected(b.name)}
              releases={app.status.releases
                .filter((r) => r.build === b.number)
                .map((r) => r.number)}
            />
          ))}
        </CardContent>
      </Card>
      <div className="grid gap-2">
        {active && (
          <div className="flex items-center gap-2 text-sm">
            <span className="font-mono text-xs">{active.name}</span>
            <BuildStatus status={active.status} />
            {active.reason && (
              <span className="text-xs text-muted-foreground">
                reason: {active.reason.toLowerCase()}
              </span>
            )}
            {active.digest && (
              <span className="ml-auto font-mono text-xs text-muted-foreground">
                build {active.digest}
              </span>
            )}
          </div>
        )}
        <TextLogView
          lines={lines}
          follow={active?.status === "Building"}
          empty={active ? "Loading..." : "Select a build"}
        />
      </div>
    </div>
  );
}

function BuildRow({
  build,
  active,
  onSelect,
  releases,
}: {
  build: BuildInfo;
  active: boolean;
  onSelect: () => void;
  releases: number[];
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={cn(
        "grid gap-0.5 rounded-md border px-3 py-2 text-left text-sm transition-colors hover:bg-accent",
        active && "border-primary/50 bg-accent",
      )}
    >
      <div className="flex items-center justify-between">
        <span className="font-medium">
          Build #{build.number}
          {build.strategy === "dockerfile" && (
            <span className="ml-2 text-[10px] font-normal uppercase tracking-wide text-muted-foreground">
              Dockerfile
            </span>
          )}
        </span>
        <BuildStatus status={build.status} />
      </div>
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>{build.source || "-"}</span>
        <span>
          {ago(build.startedAt)} ·{" "}
          {duration(build.startedAt, build.completedAt)}
        </span>
      </div>
      {releases.length > 0 && (
        <div className="text-[10px] text-muted-foreground">
          used by {releases.map((n) => `v${n}`).join(", ")}
        </div>
      )}
    </button>
  );
}

function BuildStatus({ status }: { status: BuildInfo["status"] }) {
  const cls =
    status === "Succeeded"
      ? "bg-emerald-500/15 text-emerald-500 border-emerald-500/30"
      : status === "Failed"
        ? "bg-red-500/15 text-red-500 border-red-500/30"
        : "bg-sky-500/15 text-sky-400 border-sky-500/30";
  return (
    <Badge variant="outline" className={cn("text-[10px]", cls)}>
      {status}
    </Badge>
  );
}

// ---- config (write-only config vars) -----------------------------------------

function Config({ app }: { app: AppDetail }) {
  const qc = useQueryClient();
  const perms = usePerms(app.name);
  const key = ["config-vars", app.namespace, app.name];
  const vars = useQuery({
    queryKey: key,
    queryFn: () => api.configVars(app.namespace, app.name),
    refetchInterval: 15_000,
  });
  const update = useMutation({
    mutationFn: (body: {
      set?: Record<string, string>;
      unset?: string[];
      dotenv?: string;
    }) => api.updateConfigVars(app.namespace, app.name, body),
    onSuccess: (_, body) => {
      const n =
        Object.keys(body.set ?? {}).length +
        (body.dotenv
          ? body.dotenv.split("\n").filter((l) => l.includes("=")).length
          : 0);
      toast.success(
        body.unset?.length
          ? `Removed ${body.unset.join(", ")}`
          : `Saved ${n} config var${n === 1 ? "" : "s"}; restarting processes`,
      );
      qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const [newName, setNewName] = useState("");
  const [newValue, setNewValue] = useState("");
  const [editing, setEditing] = useState<string | null>(null);
  const [editValue, setEditValue] = useState("");
  const validName = /^[A-Za-z_][A-Za-z0-9_]*$/.test(newName);

  return (
    <div className="grid gap-4 lg:grid-cols-[1fr_20rem]">
      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-4">
          <div>
            <CardTitle className="flex items-center gap-2 text-sm">
              <KeyRound className="size-4" /> Config vars
            </CardTitle>
            <CardDescription>
              Injected into every process as environment variables. Values are
              write-only: they can be replaced or removed but never read back.
              Changing them creates a release and restarts the processes.
            </CardDescription>
          </div>
          {perms.config && (
            <BulkDialog
              onSubmit={(dotenv) => update.mutate({ dotenv })}
              pending={update.isPending}
            />
          )}
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead className="text-right"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(vars.data?.vars ?? []).length === 0 && (
                <TableRow>
                  <TableCell
                    colSpan={4}
                    className="py-6 text-center text-sm text-muted-foreground"
                  >
                    No config vars yet.
                  </TableCell>
                </TableRow>
              )}
              {vars.data?.vars.map((v) => (
                <TableRow key={v.name}>
                  <TableCell className="font-mono text-xs">{v.name}</TableCell>
                  <TableCell>
                    {editing === v.name ? (
                      <form
                        className="flex items-center gap-2"
                        onSubmit={(e) => {
                          e.preventDefault();
                          update.mutate({ set: { [v.name]: editValue } });
                          setEditing(null);
                          setEditValue("");
                        }}
                      >
                        <Input
                          type="password"
                          autoFocus
                          autoComplete="off"
                          value={editValue}
                          onChange={(e) => setEditValue(e.target.value)}
                          placeholder="New value"
                          className="h-7 text-xs"
                        />
                        <Button
                          type="submit"
                          size="xs"
                          disabled={update.isPending}
                        >
                          Save
                        </Button>
                        <Button
                          type="button"
                          size="icon-xs"
                          variant="ghost"
                          onClick={() => setEditing(null)}
                        >
                          <X />
                        </Button>
                      </form>
                    ) : (
                      <span className="font-mono text-xs text-muted-foreground">
                        ••••••••
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {v.updatedAt ? ago(v.updatedAt) : "-"}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="inline-flex gap-1">
                      <Button
                        size="xs"
                        variant="outline"
                        onClick={() => setEditing(v.name)}
                        disabled={editing === v.name || !perms.config}
                      >
                        Replace
                      </Button>
                      <Button
                        size="xs"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => update.mutate({ unset: [v.name] })}
                        disabled={update.isPending || !perms.config}
                      >
                        Remove
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
              {vars.data?.bound?.map((b) => (
                <TableRow key={"bound-" + b.name} className="bg-muted/30">
                  <TableCell className="font-mono text-xs">{b.name}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    provided by{" "}
                    <span className="font-medium text-foreground">
                      {b.provider}
                    </span>{" "}
                    (read-only; wins over a config var of the same name)
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    -
                  </TableCell>
                  <TableCell />
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <form
            className="mt-4 grid gap-2 sm:grid-cols-[1fr_1fr_auto]"
            onSubmit={(e) => {
              e.preventDefault();
              if (!validName) return;
              update.mutate({ set: { [newName]: newValue } });
              setNewName("");
              setNewValue("");
            }}
          >
            <Input
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              placeholder="NAME"
              className="font-mono text-xs"
              autoComplete="off"
            />
            <Input
              type="password"
              value={newValue}
              onChange={(e) => setNewValue(e.target.value)}
              placeholder="value"
              autoComplete="off"
            />
            <Button
              type="submit"
              size="sm"
              disabled={!validName || update.isPending || !perms.config}
              title={
                !perms.config
                  ? "Your role cannot change config vars"
                  : undefined
              }
            >
              <Plus data-icon="inline-start" /> Add
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-sm">Plain environment</CardTitle>
          <CardDescription>
            Non-secret variables from the app spec, plus PORT for processes with
            a port.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {app.spec.env?.length ? (
            <ul className="grid gap-1 font-mono text-xs">
              {app.spec.env.map((e) => (
                <li
                  key={e.name}
                  className="flex justify-between rounded border px-2 py-1"
                >
                  <span>{e.name}</span>
                  <span className="text-muted-foreground">
                    {e.value ?? "(from ref)"}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">None.</p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function BulkDialog({
  onSubmit,
  pending,
}: {
  onSubmit: (dotenv: string) => void;
  pending: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          Bulk edit
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add from .env</DialogTitle>
          <DialogDescription>
            Paste KEY=VALUE lines. Existing names are replaced, others are kept.
            Nothing is echoed back.
          </DialogDescription>
        </DialogHeader>
        <textarea
          className="h-48 w-full rounded-md border bg-background p-2 font-mono text-xs"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={"DATABASE_URL=postgres://...\nREDIS_URL=redis://..."}
          autoComplete="off"
          spellCheck={false}
        />
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            Cancel
          </Button>
          <Button
            disabled={!text.trim() || pending}
            onClick={() => {
              onSubmit(text);
              setText("");
              setOpen(false);
            }}
          >
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---- deploy / destroy dialogs -------------------------------------------------

function DeployDialog({
  app,
  onDone,
  disabled,
}: {
  app: AppDetail;
  onDone: () => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [git, setGit] = useState(app.spec.source?.git?.url ?? "");
  const [ref, setRef] = useState(app.spec.source?.git?.revision ?? "");
  const [path, setPath] = useState(app.spec.source?.subPath ?? "");
  const [strategy, setStrategy] = useState<"buildpacks" | "dockerfile">(
    app.spec.build?.strategy ?? "buildpacks",
  );
  const [dockerfile, setDockerfile] = useState(
    app.spec.build?.dockerfile ?? "",
  );
  const deploy = useMutation({
    mutationFn: () =>
      api.deploy(app.namespace, app.name, {
        git: { url: git.trim(), revision: ref.trim() || "main" },
        subPath: path.trim() || undefined,
        strategy,
        dockerfile:
          strategy === "dockerfile"
            ? dockerfile.trim() || undefined
            : undefined,
      }),
    onSuccess: () => {
      toast.success("Deploy requested; building");
      setOpen(false);
      onDone();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button
          size="sm"
          disabled={disabled}
          title={
            disabled ? "Wait for the current release to finish" : undefined
          }
        >
          <Rocket data-icon="inline-start" /> Deploy
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deploy from Git</DialogTitle>
          <DialogDescription>
            Builds the repository with buildpacks or its Dockerfile and releases
            it. With buildpacks, new commits on the branch rebuild
            automatically. To deploy a local checkout use{" "}
            <code className="font-mono text-xs">shpyrd deploy</code>.
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (git.trim()) deploy.mutate();
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="dep-git">Repository URL</Label>
            <Input
              id="dep-git"
              value={git}
              onChange={(e) => setGit(e.target.value)}
              placeholder="https://github.com/org/repo"
              autoFocus
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="dep-ref">Branch, tag or commit</Label>
              <Input
                id="dep-ref"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                placeholder="main"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="dep-path">Directory</Label>
              <Input
                id="dep-path"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="services/api"
              />
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="dep-strategy">Build with</Label>
              <Select
                value={strategy}
                onValueChange={(v) =>
                  setStrategy(v as "buildpacks" | "dockerfile")
                }
              >
                <SelectTrigger id="dep-strategy">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="buildpacks">
                    Buildpacks (auto-detected stack)
                  </SelectItem>
                  <SelectItem value="dockerfile">Dockerfile</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {strategy === "dockerfile" && (
              <div className="grid gap-2">
                <Label htmlFor="dep-dockerfile">Dockerfile path</Label>
                <Input
                  id="dep-dockerfile"
                  value={dockerfile}
                  onChange={(e) => setDockerfile(e.target.value)}
                  placeholder="Dockerfile"
                />
              </div>
            )}
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!git.trim() || deploy.isPending}>
              Deploy
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function DestroyDialog({ app }: { app: AppDetail }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState("");
  const resources = useQuery({
    queryKey: ["resources", app.namespace],
    queryFn: () => api.resources(app.namespace),
    enabled: open,
  });
  // Resources holding data go first so they are not missed.
  const doomed = [...(resources.data ?? [])].sort(
    (a, b) => Number(b.data) - Number(a.data),
  );
  const destroy = useMutation({
    mutationFn: () => api.deleteApp(app.namespace, app.name),
    onSuccess: () => {
      toast.success(`Deleting ${app.name}`);
      qc.invalidateQueries({ queryKey: ["apps"] });
      navigate("/");
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          className="text-destructive hover:text-destructive"
        >
          <Trash2 data-icon="inline-start" /> Destroy
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Destroy {app.name}?</DialogTitle>
          <DialogDescription>
            This deletes the project with all its resources: builds, releases,
            config vars and running processes. Type its name to confirm.
          </DialogDescription>
        </DialogHeader>
        {doomed.length > 0 && (
          <ul className="grid gap-1 rounded-md border p-3 text-sm">
            {doomed.map((r) => (
              <li
                key={r.kind + r.name}
                className="flex items-center justify-between gap-3"
              >
                <span>
                  <span className="text-muted-foreground">{r.kind}</span>{" "}
                  <span className="font-medium">{r.name}</span>
                  {r.details?.size && (
                    <span className="text-muted-foreground">
                      {" "}
                      · {r.details.size}
                    </span>
                  )}
                </span>
                {r.data && (
                  <span className="text-xs font-medium text-destructive">
                    data is lost
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
        <Input
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          placeholder={app.name}
          autoComplete="off"
        />
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            onClick={() => destroy.mutate()}
            disabled={confirm !== app.name || destroy.isPending}
          >
            Destroy project
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---- resources -----------------------------------------------------------------

function ResourcesCard({
  app,
  onChanged,
}: {
  app: AppDetail;
  onChanged: () => void;
}) {
  const qc = useQueryClient();
  const resources = useQuery({
    queryKey: ["resources", app.namespace],
    queryFn: () => api.resources(app.namespace),
    refetchInterval: 5000,
  });
  const volumes = useQuery({
    queryKey: ["volumes", app.namespace],
    queryFn: () => api.volumes(app.namespace),
    refetchInterval: 5000,
  });
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["resources", app.namespace] });
    qc.invalidateQueries({ queryKey: ["volumes", app.namespace] });
    onChanged();
  };
  const removeVolume = useMutation({
    mutationFn: (v: VolumeInfo) =>
      api.deleteVolume(app.namespace, v.name, v.mountedBy.length > 0),
    onSuccess: (_, v) => {
      toast.success(`Deleted volume ${v.name}`);
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const list = resources.data ?? [];
  const volumeOf = (name: string) => volumes.data?.find((v) => v.name === name);
  const perms = usePerms(app.name);
  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="flex items-center gap-2">
            <HardDrive className="size-4" /> Resources
          </CardTitle>
          <CardDescription>
            Everything in this project: the app, its volumes and, as they
            arrive, databases and caches. Attached resources inject their
            connection details as config vars.
          </CardDescription>
        </div>
        {perms.resource && <VolumeDialog app={app} onDone={refresh} />}
      </CardHeader>
      <CardContent>
        {resources.isLoading ? (
          <Skeleton className="h-10 w-full" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Type</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Details</TableHead>
                <TableHead>Used by</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((r) => {
                const vol = r.kind === "Volume" ? volumeOf(r.name) : undefined;
                return (
                  <TableRow key={r.kind + r.name}>
                    <TableCell className="text-xs text-muted-foreground">
                      {r.kind}
                    </TableCell>
                    <TableCell className="font-medium">{r.name}</TableCell>
                    <TableCell>
                      <span
                        className={cn(
                          "text-xs",
                          (r.phase === "Bound" || r.phase === "Running") &&
                            "text-emerald-500",
                          r.phase === "Failed" && "text-destructive",
                          (r.phase === "Pending" ||
                            r.phase === "Building" ||
                            r.phase === "Deploying") &&
                            "text-muted-foreground",
                        )}
                        title={r.message}
                      >
                        {r.phase}
                        {r.message &&
                        r.phase !== "Bound" &&
                        r.phase !== "Running"
                          ? ` · ${r.message}`
                          : ""}
                      </span>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.kind === "App"
                        ? [
                            r.details?.release,
                            r.details?.build,
                            r.endpoint?.replace(/^https:\/\//, ""),
                          ]
                            .filter(Boolean)
                            .join(" · ")
                        : [
                            r.details?.capacity &&
                            r.details.capacity !== r.details.size
                              ? `${r.details.capacity} → ${r.details.size}`
                              : r.details?.size,
                            r.details?.mode,
                          ]
                            .filter(Boolean)
                            .join(" · ")}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.attachedTo.length ? r.attachedTo.join(", ") : "-"}
                    </TableCell>
                    <TableCell className="text-right">
                      {vol && perms.resource && (
                        <div className="flex justify-end gap-1">
                          <VolumeDialog
                            app={app}
                            onDone={refresh}
                            resize={vol}
                          />
                          <Button
                            variant="ghost"
                            size="xs"
                            className="text-destructive"
                            disabled={removeVolume.isPending}
                            onClick={() => {
                              const mounted = vol.mountedBy.length > 0;
                              const msg = mounted
                                ? `Volume ${vol.name} is mounted by ${vol.mountedBy.join(", ")}. Delete it and ALL its data anyway?`
                                : `Delete volume ${vol.name} and all its data (${vol.size})?`;
                              if (window.confirm(msg)) removeVolume.mutate(vol);
                            }}
                          >
                            Delete
                          </Button>
                        </div>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
        <p className="mt-3 text-xs text-muted-foreground">
          Postgres and Redis resources arrive with RFC-0009 and RFC-0010;
          volumes are created here or with{" "}
          <code className="font-mono">
            shpyrd volumes create data --size 5Gi
          </code>
          .
        </p>
      </CardContent>
    </Card>
  );
}

function VolumeDialog({
  app,
  onDone,
  resize,
}: {
  app: AppDetail;
  onDone: () => void;
  resize?: VolumeInfo;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [size, setSize] = useState(resize?.size ?? "5Gi");
  const [shared, setShared] = useState(false);
  const save = useMutation({
    mutationFn: () =>
      resize
        ? api.resizeVolume(app.namespace, resize.name, size.trim())
        : api.createVolume(app.namespace, {
            name: name.trim(),
            size: size.trim(),
            shared,
          }),
    onSuccess: () => {
      toast.success(
        resize
          ? `Resizing ${resize.name} to ${size}`
          : `Created volume ${name}; mount it in shpyrd.yaml and deploy`,
      );
      setOpen(false);
      setName("");
      onDone();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {resize ? (
          <Button variant="ghost" size="xs">
            Resize
          </Button>
        ) : (
          <Button size="sm" variant="outline">
            <Plus data-icon="inline-start" /> New volume
          </Button>
        )}
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {resize ? `Resize ${resize.name}` : "New volume"}
          </DialogTitle>
          <DialogDescription>
            {resize
              ? "Volumes only grow, and only when the storage class allows expansion."
              : "A persistent disk for this project. Mount it from shpyrd.yaml: processes.<type>.volumes: [{name, path}]."}
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          {!resize && (
            <div className="grid gap-2">
              <Label htmlFor="vol-name">Name</Label>
              <Input
                id="vol-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="data"
                autoFocus
              />
            </div>
          )}
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="vol-size">Size</Label>
              <Input
                id="vol-size"
                value={size}
                onChange={(e) => setSize(e.target.value)}
                placeholder="5Gi"
              />
            </div>
            {!resize && (
              <div className="grid gap-2">
                <Label htmlFor="vol-mode">Mode</Label>
                <Select
                  value={shared ? "shared" : "single"}
                  onValueChange={(v) => setShared(v === "shared")}
                >
                  <SelectTrigger id="vol-mode">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="single">
                      Single-instance (block storage)
                    </SelectItem>
                    <SelectItem value="shared">
                      Shared (needs ReadWriteMany)
                    </SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}
          </div>
          {!resize && shared && (
            <p className="text-xs text-muted-foreground">
              Shared volumes are unsafe for SQLite (file locking over a network
              filesystem); use Postgres for databases.
            </p>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={
                save.isPending || !size.trim() || (!resize && !name.trim())
              }
            >
              {resize ? "Resize" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- members and audit -----------------------------------------------------

const roleHelp: Record<string, string> = {
  viewer: "sees everything, changes nothing",
  developer: "deploys, rolls back, scales, edits config vars, opens shells",
  admin: "also manages members and resources, and can destroy the project",
};

function MembersCard({ app }: { app: AppDetail }) {
  const qc = useQueryClient();
  const members = useQuery({
    queryKey: ["members", app.namespace],
    queryFn: () => api.members(app.namespace),
    retry: false,
  });
  const teams = useQuery({
    queryKey: ["teams"],
    queryFn: api.teams,
    retry: false,
  });
  const [kind, setKind] = useState<"user" | "team">("user");
  const [subject, setSubject] = useState("");
  const [role, setRole] = useState<"viewer" | "developer" | "admin">(
    "developer",
  );
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["members", app.namespace] });
    qc.invalidateQueries({ queryKey: ["me"] });
  };
  const add = useMutation({
    mutationFn: () =>
      api.addMember(app.namespace, {
        role,
        user: kind === "user" ? subject.trim() : undefined,
        team: kind === "team" ? subject.trim() : undefined,
      }),
    onSuccess: () => {
      toast.success(`${subject.trim()} is now ${role} on ${app.name}`);
      setSubject("");
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const remove = useMutation({
    mutationFn: (m: Member) => api.removeMember(app.namespace, m.name),
    onSuccess: refresh,
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <UsersRound className="size-4" /> Members
        </CardTitle>
        <CardDescription>
          Who can do what on this project. Platform admins have access
          everywhere; roles here add to that.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        {members.error && (
          <p className="text-xs text-destructive">
            {(members.error as Error).message}
          </p>
        )}
        {(members.data ?? []).length === 0 && !members.error && (
          <p className="text-sm text-muted-foreground">
            No roles granted yet. Until the cluster has a team or a member,
            every signed-in user is an administrator.
          </p>
        )}
        {(members.data ?? []).length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Who</TableHead>
                <TableHead>Role</TableHead>
                <TableHead className="text-right"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.data?.map((m) => (
                <TableRow key={m.name}>
                  <TableCell className="font-mono text-xs">
                    {m.user ?? (
                      <>
                        <span className="text-muted-foreground">team </span>
                        {m.team}
                      </>
                    )}
                  </TableCell>
                  <TableCell className="text-xs" title={roleHelp[m.role]}>
                    {m.role}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      size="xs"
                      variant="ghost"
                      className="text-destructive"
                      disabled={remove.isPending}
                      onClick={() => remove.mutate(m)}
                    >
                      Remove
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <form
          className="grid gap-2 sm:grid-cols-[auto_1fr_auto_auto]"
          onSubmit={(e) => {
            e.preventDefault();
            if (subject.trim()) add.mutate();
          }}
        >
          <Select
            value={kind}
            onValueChange={(v) => setKind(v as "user" | "team")}
          >
            <SelectTrigger className="h-8 w-24 text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="user">User</SelectItem>
              <SelectItem value="team">Team</SelectItem>
            </SelectContent>
          </Select>
          {kind === "team" && (teams.data ?? []).length > 0 ? (
            <Select value={subject} onValueChange={setSubject}>
              <SelectTrigger className="h-8 text-xs">
                <SelectValue placeholder="team" />
              </SelectTrigger>
              <SelectContent>
                {teams.data?.map((t) => (
                  <SelectItem key={t.name} value={t.name}>
                    {t.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <Input
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              placeholder={kind === "user" ? "email" : "team name"}
              className="h-8 text-xs"
              autoComplete="off"
            />
          )}
          <Select value={role} onValueChange={(v) => setRole(v as typeof role)}>
            <SelectTrigger className="h-8 w-32 text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="viewer">viewer</SelectItem>
              <SelectItem value="developer">developer</SelectItem>
              <SelectItem value="admin">admin</SelectItem>
            </SelectContent>
          </Select>
          <Button
            type="submit"
            size="sm"
            disabled={!subject.trim() || add.isPending}
          >
            <Plus data-icon="inline-start" /> Grant
          </Button>
        </form>
        <p className="text-[11px] text-muted-foreground">{roleHelp[role]}</p>
      </CardContent>
    </Card>
  );
}

function AuditCard({ app }: { app: AppDetail }) {
  const audit = useQuery({
    queryKey: ["audit", app.namespace, app.name],
    queryFn: () => api.audit(app.namespace, app.name, 30),
    refetchInterval: 15_000,
    retry: false,
  });
  const entries: AuditEntry[] = audit.data ?? [];
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <ScrollText className="size-4" /> Recent actions
        </CardTitle>
        <CardDescription>
          Who did what on this project, from the dashboard, the API and the CLI.
          Kept as cluster events (about an hour) until durable storage arrives.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {entries.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            Nothing recorded recently.
          </p>
        ) : (
          <ul className="grid gap-1.5 text-sm">
            {entries.map((e, i) => (
              <li key={i} className="flex items-baseline gap-2">
                <span className="w-16 shrink-0 text-xs text-muted-foreground">
                  {ago(e.time)}
                </span>
                <span className="font-medium">{e.actor}</span>
                <span className="text-muted-foreground">
                  {e.action}
                  {e.target && e.target !== app.name ? ` ${e.target}` : ""}
                  {e.detail ? ` · ${e.detail}` : ""}
                </span>
                <span className="ml-auto text-[10px] uppercase text-muted-foreground">
                  {e.via}
                </span>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}
