import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Radio, Trash2, X } from "lucide-react";
import { toast } from "sonner";
import { api, type CreateDrain, type Drain } from "@/lib/api";
import { ago } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

type Scope =
  | { kind: "project"; slug: string; processes: string[] }
  | { kind: "cluster" };

/**
 * Log drains (RFC-0023): forward log lines to an HTTPS or syslog receiver.
 * Used on the project page (that project's lines) and on the Cluster page
 * (every project's lines). Header values are write-only.
 */
export function DrainsCard({
  scope,
  readOnly = false,
  agentEnabled = true,
}: {
  scope: Scope;
  readOnly?: boolean;
  agentEnabled?: boolean;
}) {
  const qc = useQueryClient();
  const key = scope.kind === "cluster" ? ["drains", "cluster"] : ["drains", scope.slug];
  const drains = useQuery({
    queryKey: key,
    queryFn: () => (scope.kind === "cluster" ? api.clusterDrains() : api.drains(scope.slug)),
    refetchInterval: 15_000,
  });
  const remove = useMutation({
    mutationFn: (name: string) =>
      scope.kind === "cluster" ? api.deleteClusterDrain(name) : api.deleteDrain(scope.slug, name),
    onSuccess: (_, name) => {
      toast.success(`Removed drain ${name}`);
      qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="flex items-center gap-2">
            <Radio className="size-4" /> Log drains
          </CardTitle>
          <CardDescription>
            {scope.kind === "cluster"
              ? "Every project's log lines, labelled with the project, forwarded as they are written."
              : "This project's log lines forwarded as they are written: JSON over HTTPS or RFC 5424 syslog."}
            {!agentEnabled && (
              <>
                {" "}
                <span className="text-amber-700 dark:text-amber-400">
                  The logs-agent extension is off; nothing is forwarded until{" "}
                  <code className="font-mono text-xs">shpyrd extensions enable logs-agent</code>.
                </span>
              </>
            )}
          </CardDescription>
        </div>
        {!readOnly && <AddDrainDialog scope={scope} onAdded={() => qc.invalidateQueries({ queryKey: key })} />}
      </CardHeader>
      <CardContent>
        {drains.isLoading && <Skeleton className="h-16 w-full" />}
        {drains.data && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Receiver</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Last delivery</TableHead>
                <TableHead className="w-10" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {drains.data.length === 0 && (
                <TableRow>
                  <TableCell colSpan={5} className="py-6 text-center text-muted-foreground">
                    No drains.{" "}
                    <code className="font-mono text-xs">shpyrd drains add &lt;url&gt;</code> does the
                    same from the CLI.
                  </TableCell>
                </TableRow>
              )}
              {drains.data.map((d) => (
                <TableRow key={d.name}>
                  <TableCell className="font-medium">{d.name}</TableCell>
                  <TableCell>
                    <div className="font-mono text-xs">{d.url}</div>
                    <div className="text-xs text-muted-foreground">
                      {d.format}
                      {d.processes && d.processes.length > 0 && ` · ${d.processes.join(", ")}`}
                      {d.headers && d.headers.length > 0 && ` · headers: ${d.headers.join(", ")}`}
                    </div>
                  </TableCell>
                  <TableCell>
                    <DrainStatus d={d} />
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {d.lastDeliveryAt ? ago(d.lastDeliveryAt) : "-"}
                    {d.sent > 0 && <div>{d.sent.toLocaleString()} lines</div>}
                  </TableCell>
                  <TableCell>
                    {!readOnly && (
                      <Button
                        size="icon-xs"
                        variant="ghost"
                        aria-label={`Remove drain ${d.name}`}
                        onClick={() => remove.mutate(d.name)}
                        disabled={remove.isPending}
                      >
                        <Trash2 />
                      </Button>
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

function DrainStatus({ d }: { d: Drain }) {
  const color =
    d.phase === "Active"
      ? "bg-emerald-500"
      : d.phase === "Failing"
        ? "bg-red-500"
        : "bg-zinc-400";
  return (
    <div className="flex items-start gap-2 text-xs">
      <span className={cn("mt-1 inline-block size-2 rounded-full", color)} />
      <div>
        <div className="font-medium">{d.phase}</div>
        {d.message && <div className="text-muted-foreground">{d.message}</div>}
      </div>
    </div>
  );
}

function AddDrainDialog({ scope, onAdded }: { scope: Scope; onAdded: () => void }) {
  const [open, setOpen] = useState(false);
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const [format, setFormat] = useState<"auto" | "json" | "syslog">("auto");
  const [headers, setHeaders] = useState<{ k: string; v: string }[]>([]);
  const [processes, setProcesses] = useState<string[]>([]);
  const create = useMutation({
    mutationFn: (body: CreateDrain) =>
      scope.kind === "cluster" ? api.createClusterDrain(body) : api.createDrain(scope.slug, body),
    onSuccess: (d) => {
      toast.success(`Added drain ${d.name}`);
      onAdded();
      setOpen(false);
      setUrl("");
      setName("");
      setFormat("auto");
      setHeaders([]);
      setProcesses([]);
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const effectiveFormat = format === "auto" ? (url.startsWith("syslog") ? "syslog" : "json") : format;
  const valid = /^(https?|syslog(\+tls)?):\/\/[^\s/]+/.test(url.trim());

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <Plus data-icon="inline-start" /> Add drain
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{scope.kind === "cluster" ? "Add a cluster drain" : "Add a drain"}</DialogTitle>
          <DialogDescription>
            {scope.kind === "cluster"
              ? "Every project's lines go to this receiver, labelled with the project."
              : "Lines of this project go to the receiver as they are written."}{" "}
            HTTPS receivers get JSON, one object per line; syslog receivers get RFC 5424 over TCP
            (<code className="font-mono text-xs">syslog+tls://</code> for TLS).
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (!valid) return;
            const h: Record<string, string> = {};
            for (const { k, v } of headers) if (k.trim()) h[k.trim()] = v;
            create.mutate({
              url: url.trim(),
              name: name.trim() || undefined,
              format: format === "auto" ? undefined : format,
              headers: Object.keys(h).length ? h : undefined,
              processes: processes.length ? processes : undefined,
            });
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="drain-url">Receiver URL</Label>
            <Input
              id="drain-url"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://in.logs.example.com/ingest or syslog+tls://logs.example.com:6514"
              className="font-mono text-xs"
              autoFocus
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="drain-name">Name (optional)</Label>
              <Input
                id="drain-name"
                value={name}
                onChange={(e) => setName(e.target.value.toLowerCase())}
                placeholder="derived from the host"
                className="font-mono text-xs"
              />
            </div>
            <div className="grid gap-2">
              <Label>Format</Label>
              <Select value={format} onValueChange={(v) => setFormat(v as typeof format)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="auto">From the URL ({effectiveFormat})</SelectItem>
                  <SelectItem value="json">JSON over HTTPS</SelectItem>
                  <SelectItem value="syslog">Syslog (RFC 5424)</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          {effectiveFormat === "json" && (
            <div className="grid gap-2">
              <Label>Headers (API keys; values are stored, never shown again)</Label>
              {headers.map((h, i) => (
                <div key={i} className="grid grid-cols-[1fr_1fr_auto] gap-2">
                  <Input
                    value={h.k}
                    onChange={(e) => setHeaders(headers.map((x, j) => (j === i ? { ...x, k: e.target.value } : x)))}
                    placeholder="Authorization"
                    className="font-mono text-xs"
                  />
                  <Input
                    type="password"
                    value={h.v}
                    onChange={(e) => setHeaders(headers.map((x, j) => (j === i ? { ...x, v: e.target.value } : x)))}
                    placeholder="Bearer ..."
                    autoComplete="off"
                  />
                  <Button
                    type="button"
                    size="icon"
                    variant="ghost"
                    onClick={() => setHeaders(headers.filter((_, j) => j !== i))}
                    aria-label="Remove header"
                  >
                    <X />
                  </Button>
                </div>
              ))}
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="justify-self-start"
                onClick={() => setHeaders([...headers, { k: "", v: "" }])}
              >
                <Plus data-icon="inline-start" /> Header
              </Button>
            </div>
          )}
          {scope.kind === "project" && scope.processes.length > 1 && (
            <div className="grid gap-2">
              <Label>Processes (default: all)</Label>
              <div className="flex flex-wrap gap-2">
                {scope.processes.map((p) => {
                  const on = processes.includes(p);
                  return (
                    <Button
                      key={p}
                      type="button"
                      size="sm"
                      variant={on ? "default" : "outline"}
                      onClick={() => setProcesses(on ? processes.filter((x) => x !== p) : [...processes, p])}
                    >
                      {p}
                    </Button>
                  );
                })}
              </div>
            </div>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || create.isPending}>
              Add drain
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
