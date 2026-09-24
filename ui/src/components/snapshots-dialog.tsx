import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Camera, Loader2, Undo2 } from "lucide-react";
import { toast } from "sonner";

import { api, type SnapshotInfo, type VolumeInfo } from "@/lib/api";
import { ago } from "@/lib/format";
import { Button } from "@/components/ui/button";
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";

/**
 * Snapshots of one volume (RFC-0060): take one, restore one into a new
 * volume (the safe path, offered first) or in place, delete one.
 */
export function SnapshotsDialog({
  slug,
  volume,
  onChanged,
}: {
  slug: string;
  volume: VolumeInfo;
  onChanged: () => void;
}) {
  const [open, setOpen] = useState(false);
  const qc = useQueryClient();
  const key = ["snapshots", slug, volume.name];
  const snapshots = useQuery({
    queryKey: key,
    queryFn: () => api.snapshots(slug, volume.name),
    enabled: open,
    refetchInterval: (q) =>
      q.state.data?.some((s) => !s.ready) ? 3000 : false,
  });
  const refresh = () => {
    qc.invalidateQueries({ queryKey: key });
    onChanged();
  };
  const [snapName, setSnapName] = useState("");
  const take = useMutation({
    mutationFn: () =>
      api.createSnapshot(slug, volume.name, snapName.trim() || undefined),
    onSuccess: (s) => {
      toast.success(`Taking snapshot ${s.name} of ${volume.name}`);
      setSnapName("");
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const remove = useMutation({
    mutationFn: (s: SnapshotInfo) =>
      api.deleteSnapshot(slug, volume.name, s.name),
    onSuccess: (_, s) => {
      toast.success(`Deleted snapshot ${s.name}`);
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const restore = useMutation({
    mutationFn: (body: { snapshot: string; to?: string }) =>
      api.restoreVolume(slug, volume.name, body),
    onSuccess: (res) => {
      toast.success(res.message);
      setRestoring(null);
      if (res.inPlace) setOpen(false);
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const [restoring, setRestoring] = useState<SnapshotInfo | null>(null);
  const [newName, setNewName] = useState("");
  const list = snapshots.data ?? [];
  const canSnapshot = volume.phase === "Bound";

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) setRestoring(null);
      }}
    >
      <DialogTrigger asChild>
        <Button variant="ghost" size="xs" title="Snapshots of this volume">
          <Camera data-icon="inline-start" /> Snapshots
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Snapshots of {volume.name}</DialogTitle>
          <DialogDescription>
            Point-in-time copies of the disk, taken by the provider. Restore
            one into a new volume, or in place: the instances mounting the
            volume stop while the disk is replaced and start again after.
            Neither creates a release.
          </DialogDescription>
        </DialogHeader>

        {restoring ? (
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              restore.mutate({
                snapshot: restoring.name,
                to: newName.trim() || undefined,
              });
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="restore-to">
                Restore {restoring.name} into a new volume named
              </Label>
              <Input
                id="restore-to"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder={`${volume.name}-restored`}
                autoFocus
              />
              <p className="text-xs text-muted-foreground">
                Leave the name empty to restore in place instead: what is on{" "}
                {volume.name} now is discarded
                {volume.mountedBy.length > 0 &&
                  ` and ${volume.mountedBy.join(", ")} stops while the disk is swapped`}
                .
              </p>
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => setRestoring(null)}
              >
                Back
              </Button>
              <Button
                type="submit"
                variant={newName.trim() ? "default" : "destructive"}
                disabled={restore.isPending}
              >
                {restore.isPending && <Loader2 className="animate-spin" />}
                {newName.trim()
                  ? `Create ${newName.trim()}`
                  : `Restore ${volume.name} in place`}
              </Button>
            </DialogFooter>
          </form>
        ) : (
          <>
            {snapshots.isLoading ? (
              <Skeleton className="h-10 w-full" />
            ) : snapshots.isError ? (
              <p className="text-sm text-destructive">
                {(snapshots.error as Error).message}
              </p>
            ) : list.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No snapshots yet. Take one before a risky release or
                migration.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Name</TableHead>
                    <TableHead>Size</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Taken</TableHead>
                    <TableHead className="text-right">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {list.map((s) => (
                    <TableRow key={s.name}>
                      <TableCell className="font-medium">{s.name}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {s.size ?? "-"}
                      </TableCell>
                      <TableCell className="text-xs">
                        {s.ready ? (
                          <span className="text-emerald-500">ready</span>
                        ) : (
                          <span className="text-muted-foreground">
                            {s.message || "in progress"}
                          </span>
                        )}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {ago(s.createdAt)}
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="outline"
                            size="xs"
                            disabled={!s.ready || volume.phase === "Restoring"}
                            onClick={() => {
                              setNewName("");
                              setRestoring(s);
                            }}
                          >
                            <Undo2 data-icon="inline-start" /> Restore
                          </Button>
                          <Button
                            variant="ghost"
                            size="xs"
                            className="text-destructive"
                            disabled={remove.isPending}
                            onClick={() => {
                              if (
                                window.confirm(
                                  `Delete snapshot ${s.name} for good?`,
                                )
                              )
                                remove.mutate(s);
                            }}
                          >
                            Delete
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
            {!snapshots.isError && (
              <form
                className="flex items-end gap-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  take.mutate();
                }}
              >
                <div className="grid flex-1 gap-2">
                  <Label htmlFor="snap-name">New snapshot</Label>
                  <Input
                    id="snap-name"
                    value={snapName}
                    onChange={(e) => setSnapName(e.target.value)}
                    placeholder={`${volume.name}-<date> (optional name)`}
                  />
                </div>
                <Button
                  type="submit"
                  disabled={take.isPending || !canSnapshot}
                  title={
                    canSnapshot
                      ? undefined
                      : "The disk exists once a process has mounted the volume"
                  }
                >
                  {take.isPending && <Loader2 className="animate-spin" />}
                  Take snapshot
                </Button>
              </form>
            )}
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
