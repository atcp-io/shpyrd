package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/api"
	"shpyrd/pkg/kube"
)

// Volume snapshots and restores (RFC-0060). Everything goes through the
// server, which knows the profile's snapshot class and explains when the
// cluster has none.

func newVolumesSnapshotCmd(g *globalFlags) *cobra.Command {
	var (
		appName  string
		snapName string
		noWait   bool
	)
	cmd := &cobra.Command{
		Use:   "snapshot <volume> [--name <snapshot>]",
		Short: "Take a snapshot of a volume",
		Long: `Takes a point-in-time snapshot of a volume with the provider's snapshot
service (on Oracle Cloud: an incremental block volume backup). Snapshots
are the way back before a risky release or migration: restore one into a
new volume, or in place, with "shpyrd volumes restore".

Snapshots are consistent at the block level; for a database prefer its
own backups over a disk snapshot.

Local clusters and providers without CSI snapshots say so.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			body, _ := json.Marshal(api.CreateSnapshotRequest{Name: snapName})
			raw, err := serverRequest(ctx, k, "POST", "api/projects/"+name+"/volumes/"+args[0]+"/snapshots", body, "application/json")
			if err != nil {
				return err
			}
			var snap api.SnapshotView
			if err := json.Unmarshal(raw, &snap); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Taking snapshot %s of volume %s...\n", snap.Name, args[0])
			if noWait {
				return nil
			}
			ready, err := waitSnapshotReady(ctx, k, name, args[0], snap.Name, 3*time.Minute)
			if err != nil {
				return err
			}
			if ready == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Still in progress; check with `shpyrd volumes snapshots %s --project %s`.\n", args[0], name)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Snapshot %s ready%s. Restore with `shpyrd volumes restore %s --from %s [--to <new-volume>]`.\n", ready.Name, sizeSuffix(ready.Size), args[0], ready.Name)
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&snapName, "name", "", "snapshot name (default: <volume>-<date>-<time>)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as the snapshot is requested")
	cmd.AddCommand(newVolumesSnapshotListCmd(g), newVolumesSnapshotDeleteCmd(g))
	return cmd
}

// newVolumesSnapshotsCmd is `shpyrd volumes snapshots <volume>`, the form
// the RFC names; the same as `snapshot list`.
func newVolumesSnapshotsCmd(g *globalFlags) *cobra.Command {
	cmd := newVolumesSnapshotListCmd(g)
	cmd.Use = "snapshots <volume>"
	cmd.Aliases = nil
	return cmd
}

func newVolumesSnapshotListCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:     "list <volume>",
		Aliases: []string{"ls"},
		Short:   "List the snapshots of a volume",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			snaps, err := listSnapshots(ctx, k, name, args[0])
			if err != nil {
				return err
			}
			if len(snaps) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No snapshots of volume %s. Take one with `shpyrd volumes snapshot %s --project %s`.\n", args[0], args[0], name)
				return nil
			}
			printSnapshots(cmd.OutOrStdout(), snaps)
			return nil
		},
	}
	appFlag(cmd, &appName)
	return cmd
}

func newVolumesSnapshotDeleteCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		yes     bool
	)
	cmd := &cobra.Command{
		Use:     "rm <volume> <snapshot>",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete a snapshot",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			if !yes {
				return fmt.Errorf("this deletes snapshot %q of volume %s for good; re-run with --yes to confirm", args[1], args[0])
			}
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			if _, err := serverRequest(ctx, k, "DELETE", "api/projects/"+name+"/volumes/"+args[0]+"/snapshots/"+args[1], nil, ""); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted snapshot %s of volume %s\n", args[1], args[0])
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the deletion")
	return cmd
}

func newVolumesRestoreCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		from    string
		to      string
		yes     bool
	)
	cmd := &cobra.Command{
		Use:   "restore <volume> --from <snapshot> [--to <new-volume>]",
		Short: "Restore a volume from one of its snapshots",
		Long: `Restores a snapshot into a new volume (--to), which you then mount, or in
place: the processes mounting the volume stop, the disk is replaced with
the snapshot's contents and the processes start again. Neither creates a
release. Restoring in place discards what is on the volume now; take a
snapshot first if you may want it back.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			if to != "" && !volumeNameRe.MatchString(to) {
				return errors.New("new volume names use lowercase letters, digits and dashes (max 40 chars)")
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			vol, err := ac.getVolume(ctx, name, args[0])
			if err != nil {
				return err
			}
			if to == "" && !yes {
				msg := fmt.Sprintf("this replaces the contents of volume %q with snapshot %q", args[0], from)
				if len(vol.Status.MountedBy) > 0 {
					msg += fmt.Sprintf(" and stops %s while the disk is swapped", strings.Join(vol.Status.MountedBy, ", "))
				}
				return fmt.Errorf("%s; re-run with --yes to confirm, or restore into a new volume with --to <name>", msg)
			}
			body, _ := json.Marshal(api.RestoreVolumeRequest{Snapshot: from, To: to})
			raw, err := serverRequest(ctx, ac.k, "POST", "api/projects/"+name+"/volumes/"+args[0]+"/restore", body, "application/json")
			if err != nil {
				return err
			}
			var res api.RestoreVolumeResponse
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
			}
			out := cmd.OutOrStdout()
			if !res.InPlace {
				fmt.Fprintf(out, "Created volume %s (%s) from snapshot %s.\n", res.Volume.Name, res.Volume.Size, from)
				fmt.Fprintf(out, "Mount it in shpyrd.yaml under processes.<type>.volumes: [{name: %s, path: /data}] and deploy.\n", res.Volume.Name)
				return nil
			}
			fmt.Fprintf(out, "Restoring volume %s from snapshot %s in place...\n", args[0], from)
			// Follow the controller until the volume is bound again.
			deadline := time.Now().Add(10 * time.Minute)
			last := ""
			for time.Now().Before(deadline) {
				if err := sleepCtx(ctx, 2*time.Second); err != nil {
					return err
				}
				cur, err := ac.getVolume(ctx, name, args[0])
				if err != nil {
					return err
				}
				requested := cur.Annotations[shpyrdv1.AnnotationRestoreFrom] != ""
				switch {
				case cur.Status.Phase == shpyrdv1.VolumeFailed:
					return errors.New(cur.Status.Message)
				case requested || cur.Status.Phase == shpyrdv1.VolumeRestoring:
					if cur.Status.Message != last && cur.Status.Message != "" {
						fmt.Fprintf(out, "  %s\n", cur.Status.Message)
						last = cur.Status.Message
					}
				case cur.Status.RestoredFrom == from:
					// Request cleared: the new disk carries the snapshot and the
					// processes are coming back (the disk binds when they mount it).
					fmt.Fprintf(out, "Volume %s restored from snapshot %s", args[0], from)
					if len(cur.Status.MountedBy) > 0 {
						fmt.Fprintf(out, "; %s starting again", strings.Join(cur.Status.MountedBy, ", "))
					}
					fmt.Fprintln(out, ".")
					return nil
				default:
					// The controller dropped the request without restoring.
					return fmt.Errorf("restore cancelled: %s", firstNonEmpty(cur.Status.Message, "see `shpyrd volumes list`"))
				}
			}
			fmt.Fprintln(out, "Still restoring; follow it with `shpyrd volumes list`.")
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&from, "from", "", "snapshot to restore (required)")
	cmd.Flags().StringVar(&to, "to", "", "restore into a new volume with this name instead of in place")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm an in-place restore")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

func listSnapshots(ctx context.Context, k *kube.Client, project, volume string) ([]api.SnapshotView, error) {
	raw, err := serverRequest(ctx, k, "GET", "api/projects/"+project+"/volumes/"+volume+"/snapshots", nil, "")
	if err != nil {
		return nil, err
	}
	var snaps []api.SnapshotView
	if err := json.Unmarshal(raw, &snaps); err != nil {
		return nil, fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
	}
	return snaps, nil
}

// waitSnapshotReady polls until the snapshot is ready, fails, or the wait
// runs out (nil, nil).
func waitSnapshotReady(ctx context.Context, k *kube.Client, project, volume, snapshot string, wait time.Duration) (*api.SnapshotView, error) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, 3*time.Second); err != nil {
			return nil, err
		}
		snaps, err := listSnapshots(ctx, k, project, volume)
		if err != nil {
			return nil, err
		}
		for i := range snaps {
			if snaps[i].Name != snapshot {
				continue
			}
			if snaps[i].Ready {
				return &snaps[i], nil
			}
			if snaps[i].Message != "" && snaps[i].Message != "taking snapshot" {
				return nil, fmt.Errorf("snapshot %s failed: %s", snapshot, snaps[i].Message)
			}
		}
	}
	return nil, nil
}

func printSnapshots(w io.Writer, snaps []api.SnapshotView) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVOLUME\tSIZE\tSTATUS\tAGE")
	for _, s := range snaps {
		status := "ready"
		if !s.Ready {
			status = firstNonEmpty(s.Message, "in progress")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Volume, firstNonEmpty(s.Size, "-"), status, age(metav1.NewTime(s.CreatedAt)))
	}
	_ = tw.Flush()
}

func sizeSuffix(size string) string {
	if size == "" {
		return ""
	}
	return " (" + size + ")"
}
