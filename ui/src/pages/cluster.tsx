import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type ClusterMetrics } from "@/lib/api";
import { usePerms } from "@/lib/me";
import { ago, bytes } from "@/lib/format";
import { MetricChart } from "@/components/metric-chart";
import { SizesEditor } from "@/components/sizes-editor";
import { GlobalsEditor } from "@/components/globals-editor";
import { DrainsCard } from "@/components/drains-card";
import { RegistryCard } from "@/components/registry-card";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { cn } from "@/lib/utils";

export function ClusterPage() {
  const cluster = useQuery({
    queryKey: ["cluster"],
    queryFn: api.cluster,
    refetchInterval: 30_000,
  });
  const helm = useQuery({
    queryKey: ["helm"],
    queryFn: api.helmReleases,
    refetchInterval: 60_000,
  });
  const [range, setRange] = useState("1h");
  const config = useQuery({
    queryKey: ["config"],
    queryFn: api.config,
    staleTime: 60_000,
  });
  const perms = usePerms();
  const metrics = useQuery({
    queryKey: ["cluster-metrics", range],
    queryFn: () => api.clusterMetrics(range),
    refetchInterval: 30_000,
    enabled: config.data?.metrics !== false,
  });

  if (cluster.isLoading) return <Skeleton className="h-64 w-full" />;
  if (cluster.error || !cluster.data) {
    return (
      <Alert variant="destructive">
        <AlertTitle>Could not load cluster</AlertTitle>
        <AlertDescription>{(cluster.error as Error)?.message}</AlertDescription>
      </Alert>
    );
  }
  const c = cluster.data;
  const m = metrics.data;
  // The server reports its own build; the install record only knows which
  // CLI ran cluster init, which is worth a note when the two drift apart.
  const serverVersion = config.data?.version;
  const installerVersion = c.install?.version;

  return (
    <div className="grid gap-6">
      <div className="grid gap-4 md:grid-cols-4">
        <Info
          label="Environment profile"
          value={c.install?.profile ?? "-"}
          hint={profileHint(c.install?.profile)}
        />
        <Info
          label="shpyrd version"
          value={serverVersion ?? installerVersion ?? "-"}
          mono
          hint={
            serverVersion &&
            installerVersion &&
            installerVersion !== serverVersion
              ? `base stack installed with the ${installerVersion} CLI`
              : undefined
          }
        />
        <Info
          label="Domain"
          value={c.install?.domain ?? "-"}
          mono
          hint="Projects are published at <name>.<domain>"
        />
        <Info
          label="Base stack updated"
          value={c.install?.updatedAt ? ago(c.install.updatedAt) : "-"}
          hint="last shpyrd cluster init"
        />
      </div>

      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-4">
          <div>
            <CardTitle>Capacity</CardTitle>
            <CardDescription>
              <strong>Used</strong> is what the machines are doing right now;{" "}
              <strong>reserved</strong> is what running processes have
              requested, which is what limits how much more can be scheduled.
            </CardDescription>
          </div>
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
        </CardHeader>
        <CardContent className="grid gap-6">
          {metrics.error && (
            <Alert variant="destructive">
              <AlertDescription>
                {(metrics.error as Error).message}
              </AlertDescription>
            </Alert>
          )}
          {m && (
            <>
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
                <Gauge
                  label="CPU used"
                  pct={m.total.cpuUsedPct}
                  detail={`${m.total.cpuCores} cores`}
                />
                <Gauge
                  label="Memory used"
                  pct={m.total.memoryUsedPct}
                  detail={bytes(m.total.memoryBytes)}
                />
                <Gauge
                  label="CPU reserved"
                  pct={m.total.cpuRequestedPct}
                  detail="by instance requests"
                  tone="sky"
                />
                <Gauge
                  label="Memory reserved"
                  pct={m.total.memoryRequestedPct}
                  detail="by instance requests"
                  tone="sky"
                />
              </div>
              <div className="grid gap-4 md:grid-cols-2">
                {m.charts.map((ch) => (
                  <MetricChart
                    key={ch.id}
                    chart={ch}
                    range={range}
                    subtitle={
                      ch.id === "cpu"
                        ? "of each node's cores, whatever is running on it"
                        : "of each node's memory, whatever is running on it"
                    }
                  />
                ))}
              </div>
            </>
          )}
          {metrics.isLoading && <Skeleton className="h-40 w-full" />}
          <NodesTable nodes={c.nodes} usage={m} />
        </CardContent>
      </Card>

      {perms.clusterAdmin && <RegistryCard />}
      <SizesEditor readOnly={!perms.clusterAdmin} />
      {perms.clusterAdmin && <GlobalsEditor />}
      {perms.clusterAdmin && (
        <DrainsCard
          scope={{ kind: "cluster" }}
          agentEnabled={config.data?.extensions?.includes("logs-agent") ?? true}
        />
      )}

      <Card>
        <CardHeader>
          <CardTitle>Extensions</CardTitle>
          <CardDescription>
            Optional capabilities compiled into shpyrd and switched on per
            cluster with{" "}
            <code className="font-mono text-xs">
              shpyrd extensions enable &lt;name&gt;
            </code>
            .
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Extension</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Component</TableHead>
                <TableHead>What it adds</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(c.extensions ?? []).map((x) => (
                <TableRow key={x.name}>
                  <TableCell className="font-medium">{x.name}</TableCell>
                  <TableCell>
                    <Badge variant={x.enabled ? "default" : "outline"}>
                      {x.enabled ? "enabled" : "disabled"}
                    </Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {x.component || "-"}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {x.description}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className="grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Components</CardTitle>
            <CardDescription>
              Installed by shpyrd cluster init ({c.components.length})
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Component</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead className="text-right">Applied</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {c.components.map((comp) => (
                  <TableRow key={comp.name}>
                    <TableCell className="font-medium">{comp.name}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {comp.version || "-"}
                    </TableCell>
                    <TableCell className="text-right text-xs text-muted-foreground">
                      {ago(comp.appliedAt)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Helm releases</CardTitle>
            <CardDescription>Charts installed in the cluster</CardDescription>
          </CardHeader>
          <CardContent>
            {helm.isLoading ? (
              <Skeleton className="h-24 w-full" />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Release</TableHead>
                    <TableHead>Chart</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead className="text-right">Updated</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {helm.data?.map((r) => (
                    <TableRow key={`${r.namespace}/${r.name}`}>
                      <TableCell className="font-medium">{r.name}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {r.chart}-{r.chartVersion}
                      </TableCell>
                      <TableCell>
                        <Badge
                          variant={
                            r.status === "deployed"
                              ? "secondary"
                              : "destructive"
                          }
                        >
                          {r.status}
                        </Badge>
                      </TableCell>
                      <TableCell className="text-right text-xs text-muted-foreground">
                        {ago(r.updatedAt)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

// The installer profile says which environment the base stack was built for
// and therefore how load balancing, DNS, TLS and the registry are provided.
function profileHint(profile?: string): string | undefined {
  switch (profile) {
    case "local":
      return "Local kind cluster: host front door, local names, development CA, in-cluster registry";
    case "oci":
      return "Oracle Cloud (OKE): OCI load balancer, wildcard DNS record, Let's Encrypt certificates, OCIR";
    default:
      return undefined;
  }
}

function Info({
  label,
  value,
  mono,
  hint,
}: {
  label: string;
  value: string;
  mono?: boolean;
  hint?: string;
}) {
  return (
    <Card size="sm">
      <CardContent>
        <div className="text-xs text-muted-foreground">{label}</div>
        <div
          className={cn(
            "truncate text-lg font-semibold",
            mono && "font-mono text-base",
          )}
          title={value}
        >
          {value}
        </div>
        {hint && (
          <div className="mt-0.5 text-[11px] leading-snug text-muted-foreground">
            {hint}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function Gauge({
  label,
  pct,
  detail,
  tone = "orange",
}: {
  label: string;
  pct: number;
  detail?: string;
  tone?: "orange" | "sky";
}) {
  const v = Math.max(0, Math.min(100, pct || 0));
  const hot = v >= 85;
  return (
    <div className="grid gap-1.5 rounded-lg border p-3">
      <div className="flex items-baseline justify-between">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span
          className={cn(
            "font-mono text-sm font-semibold",
            hot && "text-red-500",
          )}
        >
          {v.toFixed(0)}%
        </span>
      </div>
      <Bar pct={v} tone={hot ? "red" : tone} />
      {detail && (
        <span className="text-[11px] text-muted-foreground">{detail}</span>
      )}
    </div>
  );
}

function Bar({ pct, tone }: { pct: number; tone: "orange" | "sky" | "red" }) {
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
      <div
        className={cn(
          "h-full rounded-full transition-all",
          tone === "red"
            ? "bg-red-500"
            : tone === "sky"
              ? "bg-sky-500"
              : "bg-primary",
        )}
        style={{ width: `${Math.max(0, Math.min(100, pct))}%` }}
      />
    </div>
  );
}

function NodesTable({
  nodes,
  usage,
}: {
  nodes: {
    name: string;
    ready: boolean;
    roles: string;
    arch: string;
    kubeletVersion: string;
    instanceType?: string;
    zone?: string;
  }[];
  usage?: ClusterMetrics;
}) {
  const byName = new Map(usage?.nodes.map((n) => [n.name, n]));
  // Nodes keep their Kubernetes names (kubectl's view; an IP on OKE); the
  // machine shape and zone underneath say what the name does not.
  const detail = (n: { instanceType?: string; zone?: string }) =>
    [n.instanceType, n.zone].filter(Boolean).join(" · ");
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Node</TableHead>
          <TableHead>Role</TableHead>
          <TableHead className="w-44">CPU used / reserved</TableHead>
          <TableHead className="w-44">Memory used / reserved</TableHead>
          <TableHead>Instances</TableHead>
          <TableHead className="text-right">Kubelet</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {nodes.map((n) => {
          const u = byName.get(n.name);
          return (
            <TableRow key={n.name}>
              <TableCell className="font-medium">
                <div className="flex items-center">
                  <span
                    className={cn(
                      "mr-2 inline-block size-2 shrink-0 rounded-full",
                      n.ready ? "bg-emerald-500" : "bg-red-500",
                    )}
                  />
                  {n.name}
                  <span className="ml-2 font-mono text-[10px] text-muted-foreground">
                    {n.arch}
                  </span>
                </div>
                {detail(n) && (
                  <div className="ml-4 text-[11px] font-normal text-muted-foreground">
                    {detail(n)}
                  </div>
                )}
              </TableCell>
              <TableCell className="text-xs">{n.roles}</TableCell>
              <TableCell>
                {u ? (
                  <UsageCell
                    used={u.cpuUsedPct}
                    reserved={u.cpuRequestedPct}
                    detail={`${u.cpuCores} cores`}
                  />
                ) : (
                  "-"
                )}
              </TableCell>
              <TableCell>
                {u ? (
                  <UsageCell
                    used={u.memoryUsedPct}
                    reserved={u.memoryRequestedPct}
                    detail={bytes(u.memoryBytes)}
                  />
                ) : (
                  "-"
                )}
              </TableCell>
              <TableCell className="font-mono text-xs">
                {u ? `${u.pods} / ${u.podCapacity}` : "-"}
              </TableCell>
              <TableCell className="text-right font-mono text-xs text-muted-foreground">
                {n.kubeletVersion}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

function UsageCell({
  used,
  reserved,
  detail,
}: {
  used: number;
  reserved: number;
  detail: string;
}) {
  return (
    <div className="grid gap-1">
      <div className="flex justify-between font-mono text-[11px]">
        <span>{used.toFixed(0)}%</span>
        <span className="text-sky-500">{reserved.toFixed(0)}%</span>
      </div>
      <Bar pct={used} tone={used >= 85 ? "red" : "orange"} />
      <Bar pct={reserved} tone="sky" />
      <span className="text-[10px] text-muted-foreground">{detail}</span>
    </div>
  );
}
