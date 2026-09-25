import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Globe2, Plus, Trash2 } from "lucide-react";
import { api, type DomainStatus } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

// Custom domains (RFC-0034). The project is always served at its own
// hostname; a custom domain points at it with a CNAME (or at the front door
// with an A record for a zone apex) and gets its certificate once DNS
// resolves here. The card shows the exact record to create and the state of
// each host, polling while anything is pending.
export function DomainsCard({
  slug,
  readOnly,
}: {
  slug: string;
  readOnly: boolean;
}) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["domains", slug],
    queryFn: () => api.domains(slug),
    refetchInterval: (query) =>
      query.state.data?.domains.some((d) => !settled(d)) ? 10_000 : 60_000,
  });
  const [host, setHost] = useState("");
  const add = useMutation({
    mutationFn: (h: string) => api.addDomain(slug, h),
    onSuccess: (r) => {
      toast.success(
        `Added ${r.host}. Create the DNS record shown; the certificate follows.`,
      );
      setHost("");
      qc.invalidateQueries({ queryKey: ["domains", slug] });
      qc.invalidateQueries({ queryKey: ["app", slug] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const remove = useMutation({
    mutationFn: (h: string) => api.removeDomain(slug, h),
    onSuccess: (_, h) => {
      toast.success(`Removed ${h}`);
      qc.invalidateQueries({ queryKey: ["domains", slug] });
      qc.invalidateQueries({ queryKey: ["app", slug] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const r = q.data;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Globe2 className="size-4" /> Domains
        </CardTitle>
        <CardDescription>
          The project is always served at{" "}
          <code className="font-mono text-xs">{r?.target ?? "..."}</code>. Add a
          domain you own and point it there with a CNAME
          {r?.address && (
            <>
              {" "}
              (or an {/^[0-9a-f.:]+$/i.test(r.address) ? "A" : "ALIAS"} record
              to <code className="font-mono text-xs">{r.address}</code> for a
              zone apex)
            </>
          )}
          ; the certificate is issued as soon as DNS resolves here.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {q.isLoading && <Skeleton className="h-16 w-full" />}
        {q.error && (
          <p className="text-sm text-destructive">
            {(q.error as Error).message}
          </p>
        )}
        {r && r.domains.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No custom domains yet.
          </p>
        )}
        {r && r.domains.length > 0 && (
          <ul className="grid gap-3">
            {r.domains.map((d) => (
              <li
                key={d.host}
                className="grid gap-1 rounded-lg border p-3 sm:grid-cols-[1fr_auto] sm:items-start"
              >
                <div className="grid gap-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <a
                      href={`https://${d.host}`}
                      target="_blank"
                      rel="noreferrer"
                      className="font-mono text-sm hover:underline"
                    >
                      {d.host}
                    </a>
                    <State
                      label="DNS"
                      ok={d.dns === "ok"}
                      pending={d.dns === "unknown"}
                      text={d.dns}
                    />
                    <State
                      label="Certificate"
                      ok={
                        d.certificate === "ready" ||
                        d.certificate === "wildcard"
                      }
                      pending={d.certificate === "issuing"}
                      text={
                        d.certificate === "wildcard"
                          ? "ready (wildcard)"
                          : d.certificate
                      }
                    />
                    {settled(d) && (
                      <span className="text-xs text-emerald-600 dark:text-emerald-400">
                        serving
                      </span>
                    )}
                  </div>
                  {!settled(d) && <Record d={d} />}
                  {d.certificate === "failed" && d.message && (
                    <p className="text-xs text-red-500">{d.message}</p>
                  )}
                </div>
                {!readOnly && (
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-muted-foreground hover:text-red-500"
                    disabled={remove.isPending}
                    onClick={() => remove.mutate(d.host)}
                    title="Stop serving this domain"
                  >
                    <Trash2 className="size-4" />
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}
        {!readOnly && (
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (host.trim()) add.mutate(host.trim());
            }}
          >
            <Input
              value={host}
              onChange={(e) => setHost(e.target.value)}
              placeholder="www.example.com"
              className="h-8 w-72 font-mono text-xs"
              spellCheck={false}
              autoCapitalize="off"
            />
            <Button
              type="submit"
              size="sm"
              disabled={add.isPending || !host.trim()}
            >
              <Plus data-icon="inline-start" /> Add domain
            </Button>
          </form>
        )}
      </CardContent>
    </Card>
  );
}

function settled(d: DomainStatus): boolean {
  return (
    d.dns === "ok" &&
    (d.certificate === "ready" || d.certificate === "wildcard")
  );
}

// Record shows the exact DNS record to create, in the form providers use.
function Record({ d }: { d: DomainStatus }) {
  if (d.dns === "ok") {
    return (
      <p className="text-xs text-muted-foreground">
        DNS points here; {d.message || "waiting for the certificate authority"}.
      </p>
    );
  }
  const apex = d.host.split(".").length === 2;
  return (
    <div className="grid gap-1 text-xs text-muted-foreground">
      <span>
        {d.dns === "wrong"
          ? "This name points elsewhere. Change its record to:"
          : "Create this record at your DNS provider:"}
      </span>
      <code className="rounded bg-muted px-2 py-1 font-mono text-[11px]">
        {apex && d.address ? (
          <>
            A &nbsp;&nbsp;&nbsp;&nbsp;{d.host} → {d.address}
            <span className="text-muted-foreground/70">
              {" "}
              (or ALIAS/ANAME → {d.target} where your provider supports it)
            </span>
          </>
        ) : (
          <>
            CNAME {d.host} → {d.target}
          </>
        )}
      </code>
    </div>
  );
}

function State({
  label,
  ok,
  pending,
  text,
}: {
  label: string;
  ok: boolean;
  pending: boolean;
  text: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px]",
        ok
          ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300"
          : pending
            ? "bg-muted text-muted-foreground"
            : "bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300",
      )}
      title={label}
    >
      <span
        className={cn(
          "inline-block size-1.5 rounded-full",
          ok
            ? "bg-emerald-500"
            : pending
              ? "bg-muted-foreground/60"
              : "bg-amber-500",
        )}
      />
      {label} {text}
    </span>
  );
}
