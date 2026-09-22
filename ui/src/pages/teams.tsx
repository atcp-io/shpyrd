import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Trash2, UsersRound } from "lucide-react";
import { api, type Team } from "@/lib/api";
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

// Teams (RFC-0008): groups of users that projects grant roles to, optionally
// holding a platform role. Platform admins only.
export function TeamsPage() {
  const qc = useQueryClient();
  const teams = useQuery({
    queryKey: ["teams"],
    queryFn: api.teams,
    retry: false,
  });
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["teams"] });
    qc.invalidateQueries({ queryKey: ["me"] });
  };
  const remove = useMutation({
    mutationFn: (t: Team) => api.deleteTeam(t.name),
    onSuccess: (_, t) => {
      toast.success(`Deleted team ${t.name}`);
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const list = teams.data ?? [];
  return (
    <div className="grid gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold">
            <UsersRound className="size-6" /> Teams
          </h1>
          <p className="text-sm text-muted-foreground">
            Groups of users that projects grant roles to. A team can also hold a
            platform role: platform admins manage everything, platform viewers
            read everything.
          </p>
        </div>
        <TeamDialog onDone={refresh} />
      </div>
      {teams.error && (
        <Alert variant="destructive">
          <AlertTitle>Teams are unavailable</AlertTitle>
          <AlertDescription>{(teams.error as Error).message}</AlertDescription>
        </Alert>
      )}
      {list.length === 0 && !teams.isLoading && !teams.error && (
        <Alert>
          <AlertTitle>No teams yet: roles are not enforced</AlertTitle>
          <AlertDescription>
            Every signed-in user is an administrator until the first team or
            project member exists. Start with a team holding the platform-admin
            role that includes you, then grant project roles from each
            project&apos;s Members card.
          </AlertDescription>
        </Alert>
      )}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Teams</CardTitle>
          <CardDescription>
            Also from the CLI:{" "}
            <code className="font-mono text-xs">
              shpyrd teams create web --member ada@example.com
            </code>
          </CardDescription>
        </CardHeader>
        <CardContent>
          {teams.isLoading ? (
            <Skeleton className="h-10 w-full" />
          ) : list.length === 0 ? (
            <p className="text-sm text-muted-foreground">No teams.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Members</TableHead>
                  <TableHead>Groups</TableHead>
                  <TableHead>Platform role</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.map((t) => (
                  <TableRow key={t.name}>
                    <TableCell className="font-medium">
                      {t.name}
                      {t.description && (
                        <div className="text-xs text-muted-foreground">
                          {t.description}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {t.members.length ? t.members.join(", ") : "-"}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {t.groups.length ? t.groups.join(", ") : "-"}
                    </TableCell>
                    <TableCell className="text-xs">
                      {t.platformRole || "-"}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <TeamDialog onDone={refresh} team={t} />
                        <Button
                          variant="ghost"
                          size="xs"
                          className="text-destructive"
                          disabled={remove.isPending}
                          onClick={() => {
                            if (
                              window.confirm(
                                `Delete team ${t.name} and the project roles granted to it?`,
                              )
                            )
                              remove.mutate(t);
                          }}
                        >
                          <Trash2 data-icon="inline-start" /> Delete
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
    </div>
  );
}

function TeamDialog({ onDone, team }: { onDone: () => void; team?: Team }) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(team?.name ?? "");
  const [description, setDescription] = useState(team?.description ?? "");
  const [members, setMembers] = useState(team?.members.join(", ") ?? "");
  const [groups, setGroups] = useState(team?.groups.join(", ") ?? "");
  const [platformRole, setPlatformRole] = useState(
    team?.platformRole || "none",
  );
  const split = (s: string) =>
    s
      .split(/[,\s]+/)
      .map((x) => x.trim())
      .filter(Boolean);
  const save = useMutation({
    mutationFn: () =>
      api.putTeam({
        name: name.trim(),
        description: description.trim() || undefined,
        members: split(members),
        groups: split(groups),
        platformRole: platformRole === "none" ? "" : platformRole,
      }),
    onSuccess: () => {
      toast.success(team ? `Updated team ${name}` : `Created team ${name}`);
      setOpen(false);
      onDone();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {team ? (
          <Button variant="ghost" size="xs">
            Edit
          </Button>
        ) : (
          <Button size="sm">
            <Plus data-icon="inline-start" /> New team
          </Button>
        )}
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {team ? `Edit team ${team.name}` : "New team"}
          </DialogTitle>
          <DialogDescription>
            Members are emails; groups are names from your identity
            provider&apos;s groups claim, so a directory group maps to the team
            without listing people.
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) save.mutate();
          }}
        >
          {!team && (
            <div className="grid gap-2">
              <Label htmlFor="t-name">Name</Label>
              <Input
                id="t-name"
                value={name}
                onChange={(e) => setName(e.target.value.toLowerCase())}
                placeholder="web"
                autoFocus
              />
            </div>
          )}
          <div className="grid gap-2">
            <Label htmlFor="t-desc">Description</Label>
            <Input
              id="t-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="optional"
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="t-members">Members (emails, comma separated)</Label>
            <Input
              id="t-members"
              value={members}
              onChange={(e) => setMembers(e.target.value)}
              placeholder="ada@example.com, bob@example.com"
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="t-groups">Identity provider groups</Label>
              <Input
                id="t-groups"
                value={groups}
                onChange={(e) => setGroups(e.target.value)}
                placeholder="engineering"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="t-role">Platform role</Label>
              <Select value={platformRole} onValueChange={setPlatformRole}>
                <SelectTrigger id="t-role">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">none</SelectItem>
                  <SelectItem value="platform-viewer">
                    platform-viewer
                  </SelectItem>
                  <SelectItem value="platform-admin">platform-admin</SelectItem>
                </SelectContent>
              </Select>
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
            <Button type="submit" disabled={!name.trim() || save.isPending}>
              {team ? "Save" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
