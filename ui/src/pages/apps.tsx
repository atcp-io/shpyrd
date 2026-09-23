import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Plus } from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { ago } from "@/lib/format";
import { usePerms } from "@/lib/me";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { PhaseBadge } from "@/components/phase-badge";
import { ProcessChips } from "@/components/process-chips";
import { Button } from "@/components/ui/button";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

export function AppsPage() {
  const apps = useQuery({
    queryKey: ["apps"],
    queryFn: api.apps,
    refetchInterval: 5000,
  });
  const cluster = useQuery({
    queryKey: ["cluster"],
    queryFn: api.cluster,
    refetchInterval: 30_000,
  });

  const counts = cluster.data?.phases ?? {};
  const total = cluster.data?.apps ?? apps.data?.length ?? 0;
  const perms = usePerms();
  const config = useQuery({
    queryKey: ["config"],
    queryFn: api.config,
    staleTime: 60_000,
  });
  // Signed-in users without teams are all administrators (bootstrap): say so.
  const openAccess =
    perms.loaded &&
    !perms.enforced &&
    perms.me?.provider !== "token" &&
    (config.data?.auth?.providers?.length ?? 0) > 0;

  return (
    <div className="grid gap-6">
      {openAccess && (
        <Alert>
          <AlertTitle>Roles are not enforced yet</AlertTitle>
          <AlertDescription>
            Every signed-in user is an administrator until the first team or
            project member exists. Create an admin team with{" "}
            <code className="font-mono text-xs">
              shpyrd teams create platform --platform-role platform-admin
              --member you@example.com
            </code>{" "}
            or on the Teams page.
          </AlertDescription>
        </Alert>
      )}
      <div className="grid gap-4 sm:grid-cols-4">
        <Stat label="Projects" value={total} />
        <Stat
          label="Running"
          value={counts.Running ?? 0}
          tone="text-emerald-500"
        />
        <Stat
          label="Building / deploying"
          value={(counts.Building ?? 0) + (counts.Deploying ?? 0)}
          tone="text-sky-400"
        />
        <Stat
          label="Failed"
          value={counts.Failed ?? 0}
          tone={counts.Failed ? "text-red-500" : undefined}
        />
      </div>

      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-4">
          <div>
            <CardTitle>Projects</CardTitle>
            <CardDescription>
              Everything you deploy, from source to URL. A project without a web
              process is a background worker or agent.
            </CardDescription>
          </div>
          {perms.create && <NewAppDialog />}
        </CardHeader>
        <CardContent>
          {apps.isLoading ? (
            <div className="grid gap-2">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-9 w-full" />
              ))}
            </div>
          ) : apps.error ? (
            <Alert variant="destructive">
              <AlertTitle>Could not load projects</AlertTitle>
              <AlertDescription>
                {(apps.error as Error).message}
              </AlertDescription>
            </Alert>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Phase</TableHead>
                  <TableHead>Release</TableHead>
                  <TableHead>Processes</TableHead>
                  <TableHead>URL</TableHead>
                  <TableHead className="text-right">Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {apps.data?.length === 0 && (
                  <TableRow>
                    <TableCell
                      colSpan={6}
                      className="py-10 text-center text-muted-foreground"
                    >
                      No projects yet. Create one with the button above or{" "}
                      <code className="font-mono text-xs">
                        shpyrd projects create
                      </code>
                      .
                    </TableCell>
                  </TableRow>
                )}
                {apps.data?.map((a) => (
                  <TableRow key={a.slug}>
                    <TableCell className="font-medium">
                      <Link
                        to={`/projects/${a.slug}`}
                        className="hover:underline"
                      >
                        {a.displayName}
                      </Link>
                      {a.displayName !== a.slug && (
                        <span className="ml-2 font-mono text-xs font-normal text-muted-foreground">
                          {a.slug}
                        </span>
                      )}
                    </TableCell>
                    <TableCell>
                      <PhaseBadge phase={a.phase} />
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {a.release ? `v${a.release}` : "-"}
                    </TableCell>
                    <TableCell>
                      <ProcessChips processes={a.processes} compact />
                    </TableCell>
                    <TableCell>
                      {a.url ? (
                        <a
                          href={a.url}
                          target="_blank"
                          rel="noreferrer"
                          className="inline-flex items-center gap-1 text-xs hover:underline"
                        >
                          {a.url.replace(/^https?:\/\//, "")}{" "}
                          <ExternalLink className="size-3" />
                        </a>
                      ) : (
                        <span className="text-xs text-muted-foreground">-</span>
                      )}
                    </TableCell>
                    <TableCell className="text-right text-xs text-muted-foreground">
                      {ago(a.createdAt)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function Stat({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
  tone?: string;
}) {
  return (
    <Card size="sm">
      <CardContent>
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className={`text-2xl font-semibold tabular-nums ${tone ?? ""}`}>
          {value}
        </div>
      </CardContent>
    </Card>
  );
}

export function NewAppDialog() {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  // The slug follows the name until the user edits it.
  const [slugEdit, setSlugEdit] = useState<string | null>(null);
  const slug = slugEdit ?? slugify(name);
  const [git, setGit] = useState("");
  const [ref, setRef] = useState("");
  const [path, setPath] = useState("");
  const navigate = useNavigate();
  const qc = useQueryClient();

  const create = useMutation({
    mutationFn: () =>
      api.createApp({
        name: name.trim(),
        slug: slug !== slugify(name) ? slug : undefined,
        git: git.trim()
          ? { url: git.trim(), revision: ref.trim() || "main" }
          : undefined,
        subPath: path.trim() || undefined,
      }),
    onSuccess: (a) => {
      toast.success(`Created ${a.displayName}`);
      qc.invalidateQueries({ queryKey: ["apps"] });
      setOpen(false);
      navigate(`/projects/${a.slug}`);
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const valid = name.trim() !== "" && slugRe.test(slug);

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) setSlugEdit(null);
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm">
          <Plus data-icon="inline-start" /> New project
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>
            Give it a name. Optionally point it at a public Git repository to
            build and deploy right away; otherwise deploy from your machine with{" "}
            <code className="font-mono text-xs">shpyrd deploy</code>.
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (valid) create.mutate();
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="app-name">Name</Label>
            <Input
              id="app-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="My Service"
              autoFocus
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="app-slug">Slug</Label>
            <Input
              id="app-slug"
              value={slug}
              onChange={(e) => setSlugEdit(e.target.value.toLowerCase())}
              placeholder="my-service"
              className="font-mono"
              aria-invalid={slug !== "" && !slugRe.test(slug)}
            />
            <p className="text-xs text-muted-foreground">
              {slug && !slugRe.test(slug)
                ? "Lowercase letters, digits and dashes, up to 40 characters."
                : `Used in URLs and the CLI. Becomes https://${slug || "my-service"}.<domain>`}
            </p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="app-git">Git repository (optional)</Label>
            <Input
              id="app-git"
              value={git}
              onChange={(e) => setGit(e.target.value)}
              placeholder="https://github.com/org/repo"
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="app-ref">Branch or tag</Label>
              <Input
                id="app-ref"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                placeholder="main"
                disabled={!git.trim()}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="app-path">Directory</Label>
              <Input
                id="app-path"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="services/api"
                disabled={!git.trim()}
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || create.isPending}>
              {git.trim() ? "Create and deploy" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

const slugRe = /^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$/;

/** Mirrors pkg/project.Slug: lowercase ASCII, dashes between words, accents
 * stripped, at most 40 characters. */
function slugify(name: string): string {
  return name
    .normalize("NFD")
    .replace(/\p{M}/gu, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+/, "")
    .slice(0, 40)
    .replace(/-+$/, "");
}
