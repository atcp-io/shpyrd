package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/api"
)

var volumeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

func newVolumesCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "volumes",
		Aliases: []string{"volume", "vol"},
		Short:   "Persistent disks of a project",
		Long: `Volumes are persistent disks that processes mount at a path. They outlive
deploys, scaling and crashes and are deleted only explicitly.

A volume is single-instance by default (ReadWriteOnce block storage): the
process mounting it runs one instance and rolls out with Recreate (a few
seconds of downtime per deploy, no data risk). That is right for SQLite,
uploads and caches. A shared volume (--shared, ReadWriteMany) can be mounted
by many instances and processes, but needs a provisioner that offers it and
is unsafe for SQLite.

Mount in shpyrd.yaml:

  processes:
    web:
      volumes:
        - name: data
          path: /data`,
	}
	cmd.AddCommand(newVolumesCreateCmd(g), newVolumesListCmd(g), newVolumesResizeCmd(g), newVolumesDeleteCmd(g),
		newVolumesSnapshotCmd(g), newVolumesSnapshotsCmd(g), newVolumesRestoreCmd(g))
	return cmd
}

func newVolumesCreateCmd(g *globalFlags) *cobra.Command {
	var (
		appName      string
		size         string
		class        string
		shared       bool
		fromSnapshot string
	)
	cmd := &cobra.Command{
		Use:   "create <name> --size 5Gi",
		Short: "Create a volume in the project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			if !volumeNameRe.MatchString(args[0]) {
				return errors.New("volume names use lowercase letters, digits and dashes (max 40 chars)")
			}
			qty, err := parseVolumeSize(size)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if _, err := ac.getApp(ctx, name); err != nil {
				return err
			}
			// Through the server: it knows the profile's storage classes and
			// minimum size and says when it rounds a request up (RFC-0060).
			body, _ := json.Marshal(api.CreateVolumeRequest{Name: args[0], Size: qty.String(), StorageClass: class, Shared: shared, FromSnapshot: fromSnapshot})
			raw, err := serverRequest(ctx, ac.k, "POST", "api/projects/"+name+"/volumes", body, "application/json")
			if err != nil {
				return err
			}
			var view api.VolumeView
			if err := json.Unmarshal(raw, &view); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
			}
			kind := "single-instance"
			if shared {
				kind = "shared"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created %s volume %s (%s) in project %s\n", kind, args[0], view.Size, name)
			if view.Note != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Note: %s.\n", view.Note)
			}
			if fromSnapshot != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "The data comes from snapshot %s.\n", fromSnapshot)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Mount it in shpyrd.yaml under processes.<type>.volumes: [{name: %s, path: /data}] and deploy.\n", args[0])
			if shared {
				fmt.Fprintln(cmd.OutOrStdout(), "Note: shared volumes need a ReadWriteMany provisioner and are unsafe for SQLite.")
			}
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&size, "size", "", "size, e.g. 5Gi (required)")
	cmd.Flags().StringVar(&class, "class", "", "storage class (default: the profile's class for this kind of volume)")
	cmd.Flags().BoolVar(&shared, "shared", false, "ReadWriteMany: mountable by several instances and processes")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "start from a snapshot of this project instead of empty (see `shpyrd volumes snapshots`)")
	_ = cmd.MarkFlagRequired("size")
	return cmd
}

func newVolumesListCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the volumes of the project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			var list shpyrdv1.VolumeList
			if err := ac.c.List(ctx, &list, client.InNamespace(appNamespace(name))); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No volumes in project %s. Create one with `shpyrd volumes create data --size 5Gi`.\n", name)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tMODE\tSTATUS\tMOUNTED BY\tAGE")
			for _, v := range list.Items {
				mode := "single-instance"
				if v.Shared() {
					mode = "shared"
				}
				status := firstNonEmpty(v.Status.Phase, "Pending")
				if v.Status.Message != "" {
					status += ": " + v.Status.Message
				}
				if v.Status.RestoredFrom != "" && v.Status.Phase == shpyrdv1.VolumeBound {
					status += " (restored from " + v.Status.RestoredFrom + ")"
				}
				size := v.Spec.Size.String()
				if v.Status.Capacity != "" && v.Status.Capacity != size {
					size = v.Status.Capacity + " -> " + size
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, size, mode, status, firstNonEmpty(strings.Join(v.Status.MountedBy, ", "), "-"), age(v.CreationTimestamp))
			}
			return tw.Flush()
		},
	}
	appFlag(cmd, &appName)
	return cmd
}

func newVolumesResizeCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		size    string
	)
	cmd := &cobra.Command{
		Use:   "resize <name> --size 10Gi",
		Short: "Grow a volume (when the storage class allows expansion)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			qty, err := parseVolumeSize(size)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			vol, err := ac.getVolume(ctx, name, args[0])
			if err != nil {
				return err
			}
			floor := vol.Spec.Size
			if vol.Status.Capacity != "" {
				if q, err := resource.ParseQuantity(vol.Status.Capacity); err == nil {
					floor = q // a refused expansion can be reverted to the real size
				}
			}
			if qty.Cmp(floor) < 0 {
				return fmt.Errorf("volumes cannot shrink (currently %s)", floor.String())
			}
			vol.Spec.Size = qty
			if err := ac.c.Update(ctx, vol); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Resizing volume %s to %s...\n", args[0], qty.String())
			// The controller reports quickly whether the class allows it.
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if err := sleepCtx(ctx, time.Second); err != nil {
					return err
				}
				cur, err := ac.getVolume(ctx, name, args[0])
				if err != nil {
					return err
				}
				if cur.Status.ObservedGeneration < cur.Generation {
					continue
				}
				if cur.Status.Phase == shpyrdv1.VolumeFailed {
					return errors.New(cur.Status.Message)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Volume %s: %s%s\n", args[0], cur.Status.Phase, suffixMsg(cur.Status.Message))
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Resize requested. Check with `shpyrd volumes list`.")
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&size, "size", "", "new size, e.g. 10Gi (required)")
	_ = cmd.MarkFlagRequired("size")
	return cmd
}

func newVolumesDeleteCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		yes     bool
		force   bool
	)
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"destroy", "rm"},
		Short:   "Delete a volume and its data",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			vol, err := ac.getVolume(ctx, name, args[0])
			if err != nil {
				return err
			}
			if len(vol.Status.MountedBy) > 0 && !force {
				return fmt.Errorf("volume %q is mounted by %s: remove the mount from shpyrd.yaml and deploy first, or pass --force", args[0], strings.Join(vol.Status.MountedBy, ", "))
			}
			if !yes {
				return fmt.Errorf("this deletes volume %q and all its data (%s); re-run with --yes to confirm", args[0], vol.Spec.Size.String())
			}
			if err := ac.c.Delete(ctx, vol); client.IgnoreNotFound(err) != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted volume %s from project %s\n", args[0], name)
			ac.audit(ctx, name, "volume.delete", args[0], "")
			return nil
		},
	}
	appFlag(cmd, &appName)
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the deletion")
	cmd.Flags().BoolVar(&force, "force", false, "delete even while mounted (the processes lose the disk)")
	return cmd
}

func (a *appClient) getVolume(ctx context.Context, project, name string) (*shpyrdv1.Volume, error) {
	vol := &shpyrdv1.Volume{}
	if err := a.c.Get(ctx, types.NamespacedName{Namespace: appNamespace(project), Name: name}, vol); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("volume %q not found in project %s (see `shpyrd volumes list`)", name, project)
		}
		return nil, err
	}
	return vol, nil
}

// singleInstanceVolumes maps process types to the ReadWriteOnce volume they
// mount, for scale refusals.
func (a *appClient) singleInstanceVolumes(ctx context.Context, app *shpyrdv1.App) map[string]string {
	out := map[string]string{}
	for pname, p := range app.Spec.Processes {
		for _, m := range p.Volumes {
			vol := &shpyrdv1.Volume{}
			if err := a.c.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: m.Name}, vol); err == nil && !vol.Shared() {
				out[pname] = m.Name
			}
		}
	}
	return out
}

func parseVolumeSize(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, fmt.Errorf("invalid size %q (use e.g. 5Gi)", s)
	}
	if q.Sign() <= 0 {
		return q, errors.New("size must be positive")
	}
	return q, nil
}

func suffixMsg(m string) string {
	if m == "" {
		return ""
	}
	return " (" + m + ")"
}
