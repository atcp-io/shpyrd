import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Network, Plus, X } from "lucide-react";

import { api, type AllowEntry } from "@/lib/api";
import { usePerms } from "@/lib/me";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { useState } from "react";

/**
 * The allow list (RFC-0033 phase 5): which projects and platform callers may
 * reach this app from inside the cluster. Everything is refused by default.
 */
export function ConnectionsCard({ slug }: { slug: string }) {
  const app = { slug };
  const qc = useQueryClient();
  const perms = usePerms(slug);
  const allow = useQuery({
    queryKey: ["allow", slug],
    queryFn: () => api.allow(slug),
    retry: false,
  });
  const allApps = useQuery({
    queryKey: ["apps"],
    queryFn: api.apps,
    staleTime: 30_000,
    enabled: perms.members,
  });
  const [addKind, setAddKind] = useState<"project" | "platform">("project");
  const [addValue, setAddValue] = useState("");

  const setAllow = useMutation({
    mutationFn: (entries: AllowEntry[]) => api.setAllow(app.slug, entries),
    onSuccess: () => {
      toast.success("Connections updated");
      qc.invalidateQueries({ queryKey: ["allow", app.slug] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const remove = (e: AllowEntry) => {
    const next = (allow.data ?? []).filter(
      (x) => !(x.project === e.project && x.platform === e.platform),
    );
    setAllow.mutate(next);
  };

  const add = () => {
    const v = addValue.trim();
    if (!v) return;
    const entry: AllowEntry =
      addKind === "project"
        ? { project: v }
        : { platform: v as "actions" | "mcp" };
    const existing = allow.data ?? [];
    if (
      existing.some(
        (x) => x.project === entry.project && x.platform === entry.platform,
      )
    ) {
      toast.error("Already in the allow list");
      return;
    }
    setAllow.mutate([...existing, entry]);
    setAddValue("");
  };

  const otherApps = (allApps.data ?? []).filter((a) => a.slug !== slug);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <Network className="size-4" /> Connections
        </CardTitle>
        <CardDescription>
          Projects are network-isolated by default. Nothing on the cluster may
          reach this app except the ingress controller and monitoring. Add
          entries to open specific inbound paths — no release needed.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        {allow.isLoading && <Skeleton className="h-10 w-full" />}
        {allow.data && allow.data.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No allow entries — only the ingress and monitoring can reach this
            app.
          </p>
        )}
        {allow.data && allow.data.length > 0 && (
          <div className="flex flex-wrap gap-2">
            {allow.data.map((e) => (
              <Badge
                key={e.project ?? e.platform}
                variant="outline"
                className="gap-1.5 pl-2.5"
              >
                {e.project ? (
                  <>
                    <span className="text-xs text-muted-foreground">
                      project
                    </span>{" "}
                    {e.project}
                  </>
                ) : (
                  <>
                    <span className="text-xs text-muted-foreground">
                      platform
                    </span>{" "}
                    {e.platform}
                  </>
                )}
                {perms.members && (
                  <button
                    className="ml-0.5 rounded hover:text-destructive"
                    onClick={() => remove(e)}
                    aria-label={`Remove ${e.project ?? e.platform}`}
                  >
                    <X className="size-3" />
                  </button>
                )}
              </Badge>
            ))}
          </div>
        )}
        {perms.members && (
          <form
            className="grid gap-2 sm:grid-cols-[auto_1fr_auto]"
            onSubmit={(e) => {
              e.preventDefault();
              add();
            }}
          >
            <Select
              value={addKind}
              onValueChange={(v) => {
                setAddKind(v as "project" | "platform");
                setAddValue("");
              }}
            >
              <SelectTrigger className="h-8 w-28 text-xs">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="project">Project</SelectItem>
                <SelectItem value="platform">Platform</SelectItem>
              </SelectContent>
            </Select>
            {addKind === "project" ? (
              <Select value={addValue} onValueChange={setAddValue}>
                <SelectTrigger className="h-8 text-xs">
                  <SelectValue placeholder="choose a project" />
                </SelectTrigger>
                <SelectContent>
                  {otherApps.map((a) => (
                    <SelectItem key={a.slug} value={a.slug}>
                      {a.displayName}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            ) : (
              <Select value={addValue} onValueChange={setAddValue}>
                <SelectTrigger className="h-8 text-xs">
                  <SelectValue placeholder="choose a platform caller" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="mcp">mcp — the MCP connector</SelectItem>
                  <SelectItem value="actions">
                    actions — server acting for a user
                  </SelectItem>
                </SelectContent>
              </Select>
            )}
            <Button
              type="submit"
              size="sm"
              disabled={!addValue || setAllow.isPending}
            >
              <Plus data-icon="inline-start" /> Allow
            </Button>
          </form>
        )}
        <p className="text-[11px] text-muted-foreground">
          The ingress and monitoring are always admitted. Configure the same
          setting in <code>shpyrd.yaml</code>:
        </p>
        <pre className="rounded bg-muted p-2 text-[11px] leading-relaxed">
          {`allow:\n  - project: expenses   # another project\n  - platform: mcp       # the MCP connector`}
        </pre>
      </CardContent>
    </Card>
  );
}
