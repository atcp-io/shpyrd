import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { Building2, Trash2 } from "lucide-react";

import { api, type Person } from "@/lib/api";
import { ago } from "@/lib/format";
import { usePerms } from "@/lib/me";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { TeamsPage } from "@/pages/teams";
import { UsersPage } from "@/pages/users";
import { SignInSettings } from "@/components/signin-settings";
import { TokensCard } from "@/components/tokens-card";

/**
 * The workspace (RFC-0033): the tenant every project belongs to. Its name,
 * the people it has seen sign in, its teams and — with auth-local — the
 * accounts it manages. One tab per concern; the old /teams and /users
 * routes land here.
 */
export function WorkspacePage() {
  const { tab } = useParams();
  const navigate = useNavigate();
  const perms = usePerms();
  const config = useQuery({
    queryKey: ["config"],
    queryFn: api.config,
    staleTime: 60_000,
  });
  const ws = useQuery({ queryKey: ["workspace"], queryFn: api.workspace });
  const usersEnabled =
    config.data?.extensions?.includes("auth-local") && perms.clusterAdmin;
  const current = tab ?? "overview";

  return (
    <div className="grid gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold">
            <Building2 className="size-6" />
            {ws.data?.name ?? (
              <Skeleton className="inline-block h-7 w-40 align-middle" />
            )}
          </h1>
          <p className="text-sm text-muted-foreground">
            The workspace: the people who sign in here, the teams projects grant
            roles to
            {usersEnabled ? ", the accounts it manages" : ""}.
            {ws.data?.domain ? (
              <>
                {" "}
                Apps live under{" "}
                <code className="text-xs">*.{ws.data.domain}</code>.
              </>
            ) : null}
          </p>
        </div>
      </div>

      <Tabs
        value={current}
        onValueChange={(v) =>
          navigate(v === "overview" ? "/workspace" : `/workspace/${v}`)
        }
      >
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          {perms.clusterAdmin && (
            <TabsTrigger value="people">People</TabsTrigger>
          )}
          {perms.clusterAdmin && config.data?.authRequired && (
            <TabsTrigger value="teams">Teams</TabsTrigger>
          )}
          {usersEnabled && <TabsTrigger value="users">Accounts</TabsTrigger>}
          {perms.clusterAdmin && (
            <TabsTrigger value="signin">Sign-in</TabsTrigger>
          )}
          {config.data?.authRequired && (
            <TabsTrigger value="tokens">API tokens</TabsTrigger>
          )}
        </TabsList>
        <TabsContent value="overview" className="mt-4">
          <WorkspaceCard readOnly={!perms.clusterAdmin} />
        </TabsContent>
        <TabsContent value="people" className="mt-4">
          <PeopleCard />
        </TabsContent>
        <TabsContent value="teams" className="mt-4">
          <TeamsPage />
        </TabsContent>
        <TabsContent value="users" className="mt-4">
          <UsersPage />
        </TabsContent>
        <TabsContent value="signin" className="mt-4">
          <SignInSettings
            authLocal={!!config.data?.extensions?.includes("auth-local")}
          />
        </TabsContent>
        <TabsContent value="tokens" className="mt-4">
          <TokensCard />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function WorkspaceCard({ readOnly }: { readOnly: boolean }) {
  const qc = useQueryClient();
  const ws = useQuery({ queryKey: ["workspace"], queryFn: api.workspace });
  const [name, setName] = useState("");
  useEffect(() => {
    if (ws.data) setName(ws.data.name);
  }, [ws.data]);
  const rename = useMutation({
    mutationFn: (n: string) => api.updateWorkspace({ name: n }),
    onSuccess: () => {
      toast.success("Workspace renamed");
      qc.invalidateQueries({ queryKey: ["workspace"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const dirty = ws.data ? name.trim() !== ws.data.name : false;

  return (
    <Card>
      <CardHeader>
        <CardTitle>Workspace</CardTitle>
        <CardDescription>
          Every project, team and person on this platform belongs to this
          workspace: its name here, the people who signed in, the teams projects
          grant roles to.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {ws.isLoading && <Skeleton className="h-10 w-full" />}
        {ws.error && (
          <p className="text-sm text-destructive">
            {(ws.error as Error).message}
          </p>
        )}
        {ws.data && (
          <>
            <div className="grid gap-2 sm:max-w-md">
              <Label htmlFor="ws-name">Name</Label>
              <div className="flex gap-2">
                <Input
                  id="ws-name"
                  value={name}
                  disabled={readOnly}
                  maxLength={80}
                  onChange={(e) => setName(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && dirty && name.trim())
                      rename.mutate(name.trim());
                  }}
                />
                {!readOnly && (
                  <Button
                    size="sm"
                    disabled={!dirty || !name.trim() || rename.isPending}
                    onClick={() => rename.mutate(name.trim())}
                  >
                    Save
                  </Button>
                )}
              </div>
            </div>
            <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-3">
              <div>
                <dt className="text-xs text-muted-foreground">Identifier</dt>
                <dd className="font-mono text-xs">
                  {ws.data.slug}
                  {ws.data.implicit ? " (implicit)" : ""}
                </dd>
              </div>
              {ws.data.domain && (
                <div>
                  <dt className="text-xs text-muted-foreground">Domain</dt>
                  <dd className="font-mono text-xs">{ws.data.domain}</dd>
                </div>
              )}
              <div>
                <dt className="text-xs text-muted-foreground">Created</dt>
                <dd>{ago(ws.data.createdAt)}</dd>
              </div>
            </dl>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function PeopleCard() {
  const qc = useQueryClient();
  const people = useQuery({
    queryKey: ["people"],
    queryFn: api.people,
    retry: false,
  });
  const forget = useMutation({
    mutationFn: (p: Person) => api.forgetPerson(p.email),
    onSuccess: (_, p) => {
      toast.success(`Forgot ${p.email}`);
      qc.invalidateQueries({ queryKey: ["people"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const status = useMutation({
    mutationFn: (p: Person) =>
      api.setPersonStatus(
        p.email,
        p.status === "suspended" ? "active" : "suspended",
      ),
    onSuccess: (r) => {
      toast.success(
        r.status === "suspended"
          ? `Suspended ${r.email}`
          : `Reactivated ${r.email}`,
      );
      qc.invalidateQueries({ queryKey: ["people"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>People</CardTitle>
        <CardDescription>
          Everyone who has signed in to this workspace, with the login method
          they used last. Roles come from teams and project grants, not from
          this list. Suspending someone switches their access off at once —
          every app, every page — until reactivated; forgetting someone removes
          the record, and the next sign-in creates it again.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {people.isLoading && <Skeleton className="h-24 w-full" />}
        {people.error && (
          <p className="text-sm text-destructive">
            {(people.error as Error).message}
          </p>
        )}
        {people.data && people.data.length === 0 && (
          <p className="text-sm text-muted-foreground">
            Nobody has signed in through an account yet (the admin token is not
            a person).
          </p>
        )}
        {people.data && people.data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Email</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Signed in with</TableHead>
                <TableHead>Groups</TableHead>
                <TableHead className="text-right">Last seen</TableHead>
                <TableHead className="w-48" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {people.data.map((p) => (
                <TableRow key={p.realm + p.email}>
                  <TableCell className="font-mono text-xs">{p.email}</TableCell>
                  <TableCell>
                    {p.name || <span className="text-muted-foreground">—</span>}
                  </TableCell>
                  <TableCell>
                    <Badge variant="secondary">{p.provider || "—"}</Badge>
                    {p.realm !== "workspace" && (
                      <Badge variant="outline" className="ml-1">
                        {p.realm}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {p.groups.length ? p.groups.join(", ") : "—"}
                  </TableCell>
                  <TableCell className="text-right text-muted-foreground">
                    {ago(p.lastSeenAt)}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-1">
                      {p.status === "suspended" && (
                        <Badge variant="destructive">suspended</Badge>
                      )}
                      <Button
                        variant="ghost"
                        size="xs"
                        disabled={status.isPending}
                        onClick={() => status.mutate(p)}
                      >
                        {p.status === "suspended" ? "Reactivate" : "Suspend"}
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={`Forget ${p.email}`}
                        onClick={() => forget.mutate(p)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
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
