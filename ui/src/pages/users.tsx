import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { KeyRound, Plus, Trash2, UserRound } from "lucide-react";
import { api, type LocalUser } from "@/lib/api";
import { ago } from "@/lib/format";
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

// Local accounts of the auth-local extension (RFC-0007). Every account is an
// administrator until roles arrive (RFC-0008).
export function UsersPage() {
  const qc = useQueryClient();
  const users = useQuery({
    queryKey: ["users"],
    queryFn: api.users,
    retry: false,
  });
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    staleTime: 60_000,
    retry: false,
  });
  const refresh = () => qc.invalidateQueries({ queryKey: ["users"] });
  const remove = useMutation({
    mutationFn: (u: LocalUser) => api.deleteUser(u.email),
    onSuccess: (_, u) => {
      toast.success(`Deleted ${u.email}`);
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <div className="grid gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold">
            <UserRound className="size-6" /> Users
          </h1>
          <p className="text-sm text-muted-foreground">
            Accounts that sign in to this dashboard with email and password.
            Every account is an administrator for now; roles and project
            membership come with teams.
          </p>
        </div>
        <UserDialog onDone={refresh} />
      </div>

      {users.error && (
        <Alert variant="destructive">
          <AlertTitle>Accounts are unavailable</AlertTitle>
          <AlertDescription>{(users.error as Error).message}</AlertDescription>
        </Alert>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Accounts</CardTitle>
          <CardDescription>
            Also from the CLI:{" "}
            <code className="font-mono text-xs">
              shpyrd users add you@example.com
            </code>
          </CardDescription>
        </CardHeader>
        <CardContent>
          {users.isLoading ? (
            <Skeleton className="h-10 w-full" />
          ) : (users.data ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">No accounts yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Email</TableHead>
                  <TableHead>Name</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.data?.map((u) => {
                  const self =
                    me.data?.email?.toLowerCase() === u.email.toLowerCase();
                  return (
                    <TableRow key={u.email}>
                      <TableCell className="font-mono text-xs">
                        {u.email}
                        {self && (
                          <span className="ml-2 text-[10px] uppercase text-muted-foreground">
                            you
                          </span>
                        )}
                      </TableCell>
                      <TableCell>{u.name || "-"}</TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {ago(u.createdAt)}
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          <UserDialog onDone={refresh} passwordOf={u} />
                          <Button
                            variant="ghost"
                            size="xs"
                            className="text-destructive"
                            disabled={self || remove.isPending}
                            title={
                              self
                                ? "You cannot delete the account you are signed in with"
                                : undefined
                            }
                            onClick={() => {
                              if (
                                window.confirm(
                                  `Delete ${u.email}? Their sessions end when they expire.`,
                                )
                              )
                                remove.mutate(u);
                            }}
                          >
                            <Trash2 data-icon="inline-start" /> Delete
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function UserDialog({
  onDone,
  passwordOf,
}: {
  onDone: () => void;
  passwordOf?: LocalUser;
}) {
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [repeat, setRepeat] = useState("");
  const save = useMutation({
    mutationFn: async () => {
      if (passwordOf) {
        await api.setUserPassword(passwordOf.email, password);
      } else {
        await api.createUser({
          email: email.trim(),
          name: name.trim() || undefined,
          password,
        });
      }
    },
    onSuccess: () => {
      toast.success(
        passwordOf
          ? `Password of ${passwordOf.email} changed`
          : `Created ${email.trim()}`,
      );
      setOpen(false);
      setEmail("");
      setName("");
      setPassword("");
      setRepeat("");
      onDone();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const valid =
    password.length >= 8 &&
    password === repeat &&
    (passwordOf || email.trim().includes("@"));
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {passwordOf ? (
          <Button variant="ghost" size="xs">
            <KeyRound data-icon="inline-start" /> Password
          </Button>
        ) : (
          <Button size="sm">
            <Plus data-icon="inline-start" /> New user
          </Button>
        )}
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {passwordOf ? `Change password of ${passwordOf.email}` : "New user"}
          </DialogTitle>
          <DialogDescription>
            {passwordOf
              ? "The new password takes effect at their next sign-in."
              : "The person signs in with this email and password. Passwords need at least 8 characters."}
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (valid) save.mutate();
          }}
        >
          {!passwordOf && (
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-2">
                <Label htmlFor="u-email">Email</Label>
                <Input
                  id="u-email"
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  autoFocus
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="u-name">Name</Label>
                <Input
                  id="u-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="optional"
                />
              </div>
            </div>
          )}
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="u-pw">Password</Label>
              <Input
                id="u-pw"
                type="password"
                autoComplete="new-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="u-pw2">Repeat</Label>
              <Input
                id="u-pw2"
                type="password"
                autoComplete="new-password"
                value={repeat}
                onChange={(e) => setRepeat(e.target.value)}
              />
            </div>
          </div>
          {repeat && password !== repeat && (
            <p className="text-xs text-destructive">Passwords do not match.</p>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || save.isPending}>
              {passwordOf ? "Change password" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
