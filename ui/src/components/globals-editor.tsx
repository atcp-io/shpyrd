import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Globe, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
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
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

const keyRe = /^[A-Za-z_][A-Za-z0-9_]*$/;

type Change = { set?: Record<string, string>; unset?: string[] };

/** Global config vars (RFC-0016): every project receives them, first in the
 * environment so a project's own var wins. Values are write-only. A change
 * makes a release in every project, so it asks first. */
export function GlobalsEditor({ readOnly = false }: { readOnly?: boolean }) {
  const qc = useQueryClient();
  const globals = useQuery({ queryKey: ["globals"], queryFn: api.globals });
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [pending, setPending] = useState<Change | null>(null);
  const save = useMutation({
    mutationFn: (c: Change) => api.updateGlobals(c),
    onSuccess: (r, c) => {
      const what = c.set
        ? `Set ${Object.keys(c.set).join(", ")}`
        : `Removed ${c.unset?.join(", ")}`;
      toast.success(`${what}; releasing to ${r.projects} project${r.projects === 1 ? "" : "s"}`);
      qc.setQueryData(["globals"], r);
      setPending(null);
      setName("");
      setValue("");
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const projects = globals.data?.projects ?? 0;
  const valid = keyRe.test(name) && value !== "";

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="flex items-center gap-2">
            <Globe className="size-4" /> Global config vars
          </CardTitle>
          <CardDescription>
            Injected into every process of every project (
            {projects} project{projects === 1 ? "" : "s"} today). A project&apos;s
            own config var of the same name wins, and attached resources win
            over both. Values are never shown; a change is a release in each
            project.
          </CardDescription>
        </div>
      </CardHeader>
      <CardContent>
        {globals.isLoading && <Skeleton className="h-16 w-full" />}
        {globals.data && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead className="w-10" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {globals.data.vars.length === 0 && (
                <TableRow>
                  <TableCell
                    colSpan={4}
                    className="py-6 text-center text-muted-foreground"
                  >
                    No global config vars.{" "}
                    <code className="font-mono text-xs">shpyrd globals set NAME=value</code>{" "}
                    does the same from the CLI.
                  </TableCell>
                </TableRow>
              )}
              {globals.data.vars.map((v) => (
                <TableRow key={v.name}>
                  <TableCell className="font-mono text-xs">{v.name}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    ••••••••
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {v.updatedAt ? ago(v.updatedAt) : "-"}
                  </TableCell>
                  <TableCell>
                    {!readOnly && (
                      <Button
                        size="icon-xs"
                        variant="ghost"
                        aria-label={`Remove ${v.name}`}
                        onClick={() => setPending({ unset: [v.name] })}
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
        {!readOnly && (
          <form
            className="mt-4 grid gap-2 sm:grid-cols-[1fr_1fr_auto]"
            onSubmit={(e) => {
              e.preventDefault();
              if (valid) setPending({ set: { [name]: value } });
            }}
          >
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="OPENAI_API_KEY"
              className="font-mono text-xs"
              autoComplete="off"
              aria-invalid={name !== "" && !keyRe.test(name)}
            />
            <Input
              type="password"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder="value"
              autoComplete="off"
            />
            <Button type="submit" size="sm" disabled={!valid || save.isPending}>
              <Plus data-icon="inline-start" /> Set
            </Button>
          </form>
        )}
        <Dialog open={pending !== null} onOpenChange={(o) => !o && setPending(null)}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>
                {pending?.set
                  ? `Set ${Object.keys(pending.set).join(", ")} for every project?`
                  : `Remove ${pending?.unset?.join(", ")} from every project?`}
              </DialogTitle>
              <DialogDescription>
                {projects} project{projects === 1 ? "" : "s"} will get a new
                release (&ldquo;Global config change&rdquo;) and restart their
                processes with the new configuration. Projects that set the
                same name themselves keep their own value.
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setPending(null)}>
                Cancel
              </Button>
              <Button
                onClick={() => pending && save.mutate(pending)}
                disabled={save.isPending}
              >
                {pending?.set ? "Set and release" : "Remove and release"}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </CardContent>
    </Card>
  );
}
