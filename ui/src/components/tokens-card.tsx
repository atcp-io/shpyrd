import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { KeyRound, Plus, Trash2 } from "lucide-react";

import { api, type APIToken } from "@/lib/api";
import { ago } from "@/lib/format";
import { useMe } from "@/lib/me";
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

/**
 * Personal API tokens (RFC-0031): for CI and scripts, scoped to the
 * creator's own roles. The value is shown once.
 */
export function TokensCard() {
  const qc = useQueryClient();
  const me = useMe();
  const tokens = useQuery({
    queryKey: ["tokens"],
    queryFn: api.tokens,
    retry: false,
  });
  const revoke = useMutation({
    mutationFn: (t: APIToken) => api.revokeToken(t.id),
    onSuccess: (_, t) => {
      toast.success(`Revoked ${t.name}`);
      qc.invalidateQueries({ queryKey: ["tokens"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4" /> API tokens
        </CardTitle>
        <CardDescription>
          Scoped credentials for CI, scripts and integrations. The token value
          is shown once; store it in <code>SHPYRD_TOKEN</code> or pass it to{" "}
          <code>shpyrd login --token</code>. Tokens carry no more power than you
          have when you create them.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {tokens.isLoading && <Skeleton className="h-16 w-full" />}
        {tokens.error && (
          <p className="text-sm text-destructive">
            {(tokens.error as Error).message}
          </p>
        )}
        {tokens.data && tokens.data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead className="w-20" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {tokens.data.map((t) => {
                const role =
                  t.platformRole ||
                  Object.entries(t.projectRoles ?? {})
                    .map(([p, r]) => `${p}=${r}`)
                    .join(", ") ||
                  "—";
                const expired = t.expiresAt
                  ? new Date(t.expiresAt) < new Date()
                  : false;
                return (
                  <TableRow key={t.id}>
                    <TableCell className="font-medium">
                      {t.name}
                      <span className="ml-2 font-mono text-xs text-muted-foreground">
                        {t.id.slice(0, 8)}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs">{role}</TableCell>
                    <TableCell className="text-xs">
                      {t.expiresAt ? (
                        <span className={expired ? "text-destructive" : ""}>
                          {expired ? "expired" : ago(t.expiresAt)}
                        </span>
                      ) : (
                        <span className="text-muted-foreground">never</span>
                      )}
                    </TableCell>
                    <TableCell className="text-muted-foreground text-xs">
                      {t.lastUsedAt ? ago(t.lastUsedAt) : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={`Revoke ${t.name}`}
                        disabled={revoke.isPending}
                        onClick={() => {
                          if (
                            window.confirm(
                              `Revoke token "${t.name}"? Any script using it will stop working immediately.`,
                            )
                          )
                            revoke.mutate(t);
                        }}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
        {tokens.data?.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No tokens yet. Create one for CI or for <code>shpyrd login</code>.
          </p>
        )}
        {me.data && (
          <CreateTokenDialog
            onCreated={() => qc.invalidateQueries({ queryKey: ["tokens"] })}
          />
        )}
      </CardContent>
    </Card>
  );
}

const PROJECT_ROLES = ["user", "viewer", "developer", "admin"] as const;
const PLATFORM_ROLES = ["platform-viewer", "platform-admin"] as const;

function rank<T extends readonly string[]>(order: T, role: string | undefined) {
  const i = order.indexOf(role ?? "");
  return i;
}

function CreateTokenDialog({ onCreated }: { onCreated: () => void }) {
  const me = useMe();
  const myPlatform = me.data?.roles?.platform;
  const myProjects = me.data?.roles?.projects ?? {};
  const projectSlugs = Object.keys(myProjects).sort();
  // Platform admins may scope a token to any project at any role.
  const isPlatformAdmin = myPlatform === "platform-admin";

  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [scope, setScope] = useState<"platform" | "project">(
    myPlatform ? "platform" : "project",
  );
  const [platformRole, setPlatformRole] = useState<string>(
    myPlatform ?? "platform-viewer",
  );
  const [project, setProject] = useState(projectSlugs[0] ?? "");
  const [projectRole, setProjectRole] = useState<string>(
    myProjects[projectSlugs[0] ?? ""] ?? "developer",
  );
  const [expires, setExpires] = useState("90d");
  const [secret, setSecret] = useState<string | null>(null);

  // Roles the caller may hand out: never above their own.
  const platformChoices = PLATFORM_ROLES.filter(
    (r) => rank(PLATFORM_ROLES, r) <= rank(PLATFORM_ROLES, myPlatform),
  );
  const maxProjectRole = isPlatformAdmin ? "admin" : myProjects[project];
  const projectChoices = PROJECT_ROLES.filter(
    (r) => rank(PROJECT_ROLES, r) <= rank(PROJECT_ROLES, maxProjectRole),
  );
  const canCreate =
    platformChoices.length > 0 || projectSlugs.length > 0 || isPlatformAdmin;

  const create = useMutation({
    mutationFn: () =>
      api.createToken({
        name,
        platformRole: scope === "platform" ? platformRole : undefined,
        projectRoles:
          scope === "project" && project
            ? { [project]: projectRole }
            : undefined,
        expiresIn: expires,
      }),
    onSuccess: (r) => {
      setSecret(r.token);
      onCreated();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        setOpen(v);
        if (!v) {
          setSecret(null);
          setName("");
        }
      }}
    >
      <DialogTrigger asChild>
        <Button
          size="sm"
          variant="outline"
          className="justify-self-start"
          disabled={!canCreate}
          title={canCreate ? undefined : "You have no roles to give a token"}
        >
          <Plus data-icon="inline-start" /> Create a token
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create an API token</DialogTitle>
          <DialogDescription>
            The token value is shown once after creation. A token carries at
            most the role you hold yourself and expires when you say.
          </DialogDescription>
        </DialogHeader>
        {secret ? (
          <div className="grid gap-3">
            <p className="text-sm font-medium">
              Copy your token now. It will not be shown again.
            </p>
            <pre className="rounded bg-muted p-3 text-xs break-all select-all">
              {secret}
            </pre>
            <p className="text-xs text-muted-foreground">
              Set <code>SHPYRD_TOKEN</code> in your CI, or run{" "}
              <code>shpyrd login --url {window.location.origin} --token …</code>
              .
            </p>
            <DialogFooter>
              <Button onClick={() => setOpen(false)}>Done</Button>
            </DialogFooter>
          </div>
        ) : (
          <form
            className="grid gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              if (name.trim()) create.mutate();
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="tok-name">Name</Label>
              <Input
                id="tok-name"
                placeholder="ci, laptop, deploy-bot"
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </div>
            {platformChoices.length > 0 && (
              <div className="grid gap-2">
                <Label>Scope</Label>
                <Select
                  value={scope}
                  onValueChange={(v) => setScope(v as typeof scope)}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="platform">Whole platform</SelectItem>
                    <SelectItem value="project">One project</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}
            {scope === "platform" && platformChoices.length > 0 ? (
              <div className="grid gap-2">
                <Label>Role</Label>
                <Select value={platformRole} onValueChange={setPlatformRole}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {platformChoices.map((r) => (
                      <SelectItem key={r} value={r}>
                        {r === "platform-admin"
                          ? "platform-admin: everything"
                          : "platform-viewer: read-only"}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            ) : (
              <div className="grid gap-2 sm:grid-cols-2">
                <div className="grid gap-2">
                  <Label htmlFor="tok-project">Project</Label>
                  {isPlatformAdmin ? (
                    <Input
                      id="tok-project"
                      placeholder="project slug"
                      value={project}
                      onChange={(e) => setProject(e.target.value)}
                      required
                    />
                  ) : (
                    <Select
                      value={project}
                      onValueChange={(v) => {
                        setProject(v);
                        setProjectRole(myProjects[v] ?? "user");
                      }}
                    >
                      <SelectTrigger id="tok-project">
                        <SelectValue placeholder="Pick a project" />
                      </SelectTrigger>
                      <SelectContent>
                        {projectSlugs.map((p) => (
                          <SelectItem key={p} value={p}>
                            {p}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  )}
                </div>
                <div className="grid gap-2">
                  <Label>Role</Label>
                  <Select value={projectRole} onValueChange={setProjectRole}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {(projectChoices.length
                        ? projectChoices
                        : PROJECT_ROLES
                      ).map((r) => (
                        <SelectItem key={r} value={r}>
                          {r}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              </div>
            )}
            <div className="grid gap-2">
              <Label>Expires in</Label>
              <Select value={expires} onValueChange={setExpires}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="30d">30 days</SelectItem>
                  <SelectItem value="90d">90 days</SelectItem>
                  <SelectItem value="365d">1 year</SelectItem>
                  <SelectItem value="3650d">10 years</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <DialogFooter>
              <Button
                type="submit"
                disabled={
                  !name.trim() ||
                  create.isPending ||
                  (scope === "project" && !project)
                }
              >
                Create
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
