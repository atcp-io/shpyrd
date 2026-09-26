package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/internal/controller"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/ext/resources"
)

// Backups (RFC-0038): `shpyrd pg backups enable|disable|list`, `shpyrd pg
// backup` for one now, `shpyrd pg restore` into a new database.

func backupsLine(pg *shpyrdv1.Postgres) string {
	if pg.Spec.Backups == nil {
		return "off (shpyrd pg backups enable " + pg.Name + ")"
	}
	schedule, retention := "daily at 02:00 UTC", "14d"
	if pg.Spec.Backups.Schedule != "" {
		schedule = "cron " + pg.Spec.Backups.Schedule
	}
	if pg.Spec.Backups.Retention != "" {
		retention = pg.Spec.Backups.Retention
	}
	out := fmt.Sprintf("on, %s, kept %s", schedule, retention)
	if pg.Status.LastBackup != nil {
		out += ", last " + pg.Status.LastBackup.UTC().Format(time.RFC3339)
	} else {
		out += ", no backup completed yet"
	}
	if pg.Status.RecoverableFrom != nil {
		out += ", recoverable from " + pg.Status.RecoverableFrom.UTC().Format(time.RFC3339)
	}
	return out
}

func newBackupsCmd(g ext.CLIGlobals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "Backups of a database: turn them on or off, list them",
		Long: `Backups go to the platform's object store (extension object-storage):
continuous WAL archiving plus a base backup on a schedule, kept for the
retention period. With them, "shpyrd pg restore" recovers any point in time
inside that window into a new database.`,
	}
	var (
		project   string
		retention string
		schedule  string
	)
	enable := &cobra.Command{
		Use:   "enable <name>",
		Short: "Turn backups on (or change retention and schedule)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			if pg.Spec.Backups == nil {
				pg.Spec.Backups = &shpyrdv1.PostgresBackups{}
			}
			if retention != "" {
				pg.Spec.Backups.Retention = retention
			}
			if schedule != "" {
				pg.Spec.Backups.Schedule = schedule
			}
			if err := c.Update(ctx, pg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backups of %s: %s\n", pg.Name, backupsLine(pg))
			fmt.Fprintln(cmd.OutOrStdout(), "The first base backup starts now; follow it with `shpyrd pg backups list "+pg.Name+"`.")
			return nil
		},
	}
	projectFlag(enable, &project)
	enable.Flags().StringVar(&retention, "retention", "", "how long backups are kept, e.g. 14d")
	enable.Flags().StringVar(&schedule, "schedule", "", "cron of the base backup in UTC, e.g. \"0 2 * * *\"")

	var yes bool
	disable := &cobra.Command{
		Use:   "disable <name>",
		Short: "Turn backups off (existing backups stay until the database is deleted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			if !yes {
				return fmt.Errorf("this stops WAL archiving and scheduled backups of %q; re-run with --yes to confirm", pg.Name)
			}
			pg.Spec.Backups = nil
			if err := c.Update(ctx, pg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backups of %s are off. Existing backups remain restorable until the database is deleted.\n", pg.Name)
			return nil
		},
	}
	projectFlag(disable, &project)
	disable.Flags().BoolVar(&yes, "yes", false, "confirm")

	list := &cobra.Command{
		Use:     "list <name>",
		Aliases: []string{"ls"},
		Short:   "List the base backups of a database",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			backups, err := listBackups(ctx, c, pg)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Backups:    %s\n\n", backupsLine(pg))
			if len(backups) == 0 {
				fmt.Fprintln(out, "No base backups yet.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTARTED\tSTATUS\tKIND")
			for _, b := range backups {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Name, b.Started, b.Phase, b.Kind)
			}
			return tw.Flush()
		},
	}
	projectFlag(list, &project)
	cmd.AddCommand(enable, disable, list)
	return cmd
}

type backupRow struct {
	Name, Started, Phase, Kind string
	at                         time.Time
}

func listBackups(ctx context.Context, c client.Client, pg *shpyrdv1.Postgres) ([]backupRow, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(controller.CNPGBackupGVK.GroupVersion().WithKind("BackupList"))
	if err := c.List(ctx, list, client.InNamespace(pg.Namespace)); err != nil {
		if strings.Contains(err.Error(), "no matches for kind") {
			return nil, errors.New(resources.ExtensionHint(Name))
		}
		return nil, err
	}
	var out []backupRow
	for _, item := range list.Items {
		if cl, _, _ := unstructured.NestedString(item.Object, "spec", "cluster", "name"); cl != pg.Name {
			continue
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		started, _, _ := unstructured.NestedString(item.Object, "status", "startedAt")
		kind := "on demand"
		for _, o := range item.GetOwnerReferences() {
			if o.Kind == "ScheduledBackup" {
				kind = "scheduled"
			}
		}
		row := backupRow{Name: item.GetName(), Phase: firstNonEmpty(phase, "pending"), Kind: kind, Started: "-"}
		if t, err := time.Parse(time.RFC3339, started); err == nil {
			row.at, row.Started = t, t.UTC().Format(time.RFC3339)
		} else {
			row.at = item.GetCreationTimestamp().Time
		}
		if msg, _, _ := unstructured.NestedString(item.Object, "status", "error"); msg != "" {
			row.Phase += ": " + msg
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out, nil
}

func newBackupCmd(g ext.CLIGlobals) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "backup <name>",
		Short: "Take a base backup now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			pg, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			if pg.Spec.Backups == nil {
				return fmt.Errorf("backups of %q are off: `shpyrd pg backups enable %s --project %s` first", pg.Name, pg.Name, project)
			}
			b := &unstructured.Unstructured{}
			b.SetGroupVersionKind(controller.CNPGBackupGVK)
			b.SetName(fmt.Sprintf("%s-%s", pg.Name, time.Now().UTC().Format("20060102-150405")))
			b.SetNamespace(pg.Namespace)
			b.SetLabels(map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", "shpyrd.io/postgres": pg.Name})
			b.Object["spec"] = map[string]interface{}{
				"cluster":             map[string]interface{}{"name": pg.Name},
				"method":              "plugin",
				"pluginConfiguration": map[string]interface{}{"name": controller.BarmanPluginName},
			}
			if err := c.Create(ctx, b); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backup %s started...\n", b.GetName())
			deadline := time.Now().Add(10 * time.Minute)
			for time.Now().Before(deadline) {
				time.Sleep(3 * time.Second)
				cur := &unstructured.Unstructured{}
				cur.SetGroupVersionKind(controller.CNPGBackupGVK)
				if err := c.Get(ctx, client.ObjectKeyFromObject(b), cur); err != nil {
					return err
				}
				phase, _, _ := unstructured.NestedString(cur.Object, "status", "phase")
				switch phase {
				case "completed":
					fmt.Fprintf(cmd.OutOrStdout(), "Backup %s completed.\n", b.GetName())
					return nil
				case "failed":
					msg, _, _ := unstructured.NestedString(cur.Object, "status", "error")
					return fmt.Errorf("backup failed: %s", msg)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Still running; check with `shpyrd pg backups list "+pg.Name+"`.")
			return nil
		},
	}
	projectFlag(cmd, &project)
	return cmd
}

func newRestoreCmd(g ext.CLIGlobals) *cobra.Command {
	var (
		project string
		to      string
		as      string
		size    string
		storage string
	)
	cmd := &cobra.Command{
		Use:   "restore <name> --as <new-name> [--to <time>]",
		Short: "Restore a database's backups into a new database, at a point in time",
		Long: `Creates a new database from the backups of an existing one, recovered to
the given moment (RFC 3339, e.g. 2026-09-25T10:00:00Z; the latest possible
when omitted). The source keeps running; attach the app to the new database
with "shpyrd attach" when it is ready.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cliContext()
			if as == "" {
				return errors.New("--as names the new database")
			}
			_, c, err := resources.Connect(g)
			if err != nil {
				return err
			}
			source, err := get(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			rec := &shpyrdv1.PostgresRecovery{From: source.Name}
			if to != "" {
				t, err := time.Parse(time.RFC3339, to)
				if err != nil {
					return fmt.Errorf("--to %q: use RFC 3339, e.g. 2026-09-25T10:00:00Z", to)
				}
				rec.TargetTime = &metav1.Time{Time: t}
			}
			pg := &shpyrdv1.Postgres{
				ObjectMeta: metav1.ObjectMeta{Name: as, Namespace: source.Namespace, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd", shpyrdv1.LabelProject: project}},
				Spec:       shpyrdv1.PostgresSpec{Version: source.Spec.Version, Size: firstNonEmpty(size, source.Spec.Size), Storage: source.Spec.Storage, Instances: source.Spec.Instances, Recovery: rec},
			}
			if storage != "" {
				q, err := parseStorage(storage)
				if err != nil {
					return err
				}
				pg.Spec.Storage = &q
			}
			if err := c.Create(ctx, pg); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("database %q already exists in project %s", as, project)
				}
				return err
			}
			when := "the latest point"
			if rec.TargetTime != nil {
				when = rec.TargetTime.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restoring %s from the backups of %s to %s...\n", as, source.Name, when)
			return waitReady(ctx, cmd, c, pg, 15*time.Minute)
		},
	}
	projectFlag(cmd, &project)
	cmd.Flags().StringVar(&as, "as", "", "name of the new database (required)")
	cmd.Flags().StringVar(&to, "to", "", "point in time to recover to, RFC 3339 (default: latest)")
	cmd.Flags().StringVar(&size, "size", "", "instance size of the new database (default: the source's)")
	cmd.Flags().StringVar(&storage, "storage", "", "data volume of the new database (default: the source's)")
	return cmd
}

func parseStorage(v string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(v)
	if err != nil || q.Sign() <= 0 {
		return q, fmt.Errorf("invalid --storage %q (use e.g. 10Gi)", v)
	}
	return q, nil
}
