package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"
	"shpyrd/pkg/api"
	"shpyrd/pkg/install"
)

// Log drains (RFC-0023): `shpyrd drains add|list|remove`, per project or,
// with --cluster, for every project.

func newDrainsCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		cluster bool
	)
	cmd := &cobra.Command{
		Use:   "drains",
		Short: "Forward logs to an external receiver (HTTPS or syslog)",
		Long: `Log drains forward a project's log lines to a receiver as they are written:
JSON over HTTPS (Datadog, Better Stack, Axiom, your own collector) or RFC 5424
syslog over TCP/TLS (Papertrail, rsyslog). A cluster drain (--cluster, platform
admins) receives every project's lines, labelled with the project.

  shpyrd drains add https://in.logs.example.com/ingest --header "Authorization: Bearer ..." --project shop
  shpyrd drains add syslog+tls://logs.example.com:6514 --cluster
  shpyrd drains list --project shop
  shpyrd drains remove in-logs-example-com --project shop

Header values are stored in the cluster and never printed again. Needs the
logs-agent extension (shpyrd extensions enable logs-agent).`,
	}
	add := &cobra.Command{
		Use:   "add <url>",
		Short: "Add a drain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			format, _ := cmd.Flags().GetString("format")
			rawHeaders, _ := cmd.Flags().GetStringArray("header")
			processes, _ := cmd.Flags().GetStringSlice("processes")
			ns, project, err := drainScope(cluster, appName)
			if err != nil {
				return err
			}
			headers := map[string]string{}
			for _, h := range rawHeaders {
				k, v, ok := strings.Cut(h, ":")
				if !ok || strings.TrimSpace(k) == "" {
					return fmt.Errorf("--header expects \"Name: value\", got %q", h)
				}
				headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			name, err = api.DrainName(name, args[0])
			if err != nil {
				return err
			}
			d := &shpyrdv1.LogDrain{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
				Spec:       shpyrdv1.LogDrainSpec{URL: strings.TrimSpace(args[0]), Format: format, Processes: processes},
			}
			if err := controller.ValidateDrainURL(d.Spec.URL, d.EffectiveFormat()); err != nil {
				return err
			}
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if project != "" {
				if _, err := ac.getApp(ctx, project); err != nil {
					return err
				}
			}
			if len(headers) > 0 {
				secName := "drain-" + name + "-headers"
				sec := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: ns, Labels: map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}},
					Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{},
				}
				for k, v := range headers {
					sec.Data[k] = []byte(v)
				}
				if err := ac.c.Create(ctx, sec); err != nil {
					if !apierrors.IsAlreadyExists(err) {
						return err
					}
					if err := ac.c.Update(ctx, sec); err != nil {
						return err
					}
				}
				d.Spec.HeadersFrom = &corev1.LocalObjectReference{Name: secName}
			}
			if err := ac.c.Create(ctx, d); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("drain %q already exists", name)
				}
				return err
			}
			if project == "" {
				ac.auditCluster(ctx, "drain.add", name+" -> "+d.Spec.URL, "cluster drain ("+d.EffectiveFormat()+")")
				fmt.Fprintf(cmd.OutOrStdout(), "Added cluster drain %s: every project's logs -> %s (%s)\n", name, d.Spec.URL, d.EffectiveFormat())
			} else {
				ac.audit(ctx, project, "drain.add", name+" -> "+d.Spec.URL, d.EffectiveFormat())
				fmt.Fprintf(cmd.OutOrStdout(), "Added drain %s to project %s: logs -> %s (%s)\n", name, project, d.Spec.URL, d.EffectiveFormat())
			}
			if !contains(recordedExtensions(ctx, ac.k), "logs-agent") {
				fmt.Fprintln(cmd.OutOrStdout(), "The logs-agent extension is not enabled; nothing is forwarded until you run `shpyrd extensions enable logs-agent`.")
			}
			return nil
		},
	}
	add.Flags().String("name", "", "drain name (default: derived from the url host)")
	add.Flags().String("format", "", "json or syslog (default: from the url scheme)")
	add.Flags().StringArray("header", nil, `HTTP header for json drains, "Name: value" (repeatable; values are stored, never shown)`)
	add.Flags().StringSlice("processes", nil, "only these process types (default: all)")

	list := &cobra.Command{
		Use:     "list",
		Short:   "List drains and their delivery status",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, project, err := drainScope(cluster, appName)
			if err != nil {
				return err
			}
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if project != "" {
				if _, err := ac.getApp(ctx, project); err != nil {
					return err
				}
			}
			var drains shpyrdv1.LogDrainList
			if err := ac.c.List(ctx, &drains, client.InNamespace(ns)); err != nil {
				return err
			}
			if len(drains.Items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no drains; add one with `shpyrd drains add <url>`")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tURL\tFORMAT\tPROCESSES\tSTATUS\tLAST DELIVERY")
			for _, d := range drains.Items {
				procs := "all"
				if len(d.Spec.Processes) > 0 {
					procs = strings.Join(d.Spec.Processes, ",")
				}
				last := "-"
				if d.Status.LastDeliveryAt != nil {
					last = age(*d.Status.LastDeliveryAt) + " ago"
				}
				status := firstNonEmpty(d.Status.Phase, shpyrdv1.DrainPending)
				if d.Status.Message != "" && d.Status.Phase != shpyrdv1.DrainActive {
					status += " (" + d.Status.Message + ")"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.Name, d.Spec.URL, d.EffectiveFormat(), procs, status, last)
			}
			return tw.Flush()
		},
	}

	remove := &cobra.Command{
		Use:     "remove <name>",
		Short:   "Remove a drain",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, project, err := drainScope(cluster, appName)
			if err != nil {
				return err
			}
			ctx := signalContext()
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			d := &shpyrdv1.LogDrain{}
			if err := ac.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: args[0]}, d); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("no drain %q; see `shpyrd drains list`", args[0])
				}
				return err
			}
			if d.Spec.HeadersFrom != nil {
				_ = ac.c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: d.Spec.HeadersFrom.Name}})
			}
			if err := ac.c.Delete(ctx, d); err != nil {
				return err
			}
			if project == "" {
				ac.auditCluster(ctx, "drain.remove", args[0], "")
			} else {
				ac.audit(ctx, project, "drain.remove", args[0], "")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed drain %s\n", args[0])
			return nil
		},
	}

	for _, c := range []*cobra.Command{add, list, remove} {
		appFlag(c, &appName)
		c.Flags().BoolVar(&cluster, "cluster", false, "every project's logs (platform admins)")
	}
	cmd.AddCommand(add, list, remove)
	return cmd
}

// drainScope resolves --cluster / --project into a namespace.
func drainScope(cluster bool, appName string) (ns, project string, err error) {
	if cluster {
		if appName != "" {
			return "", "", errors.New("--cluster and --project are exclusive")
		}
		return install.DefaultSystemNamespace, "", nil
	}
	project, err = resolveAppName(appName)
	if err != nil {
		return "", "", err
	}
	return appNamespace(project), project, nil
}
