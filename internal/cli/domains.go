package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/api"
	"shpyrd/pkg/kube"
)

// shpyrd domains (RFC-0034): custom hostnames for a project. The project is
// always served at <slug>.<cluster domain>; a custom domain's DNS record
// points at that hostname (CNAME) or at the front door's address (A record,
// for a zone apex). Once DNS points here the certificate is issued
// automatically; nothing else is asked of the owner.
func newDomainsCmd(g *globalFlags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "Custom domains of a project",
		Long: `A project is always served at <slug>.<cluster domain>. Custom domains are
served alongside: point them at that hostname with a CNAME (or at the front
door's address with an A record when the domain is a zone apex) and the
certificate is issued as soon as DNS resolves here.

  shpyrd domains add www.myprod.com --project shop
  shpyrd domains list --project shop
  shpyrd domains rm www.myprod.com --project shop`,
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the custom domains and their DNS and certificate state",
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
			raw, err := serverRequest(ctx, k, "GET", "api/projects/"+name+"/domains", nil, "")
			if err != nil {
				return err
			}
			var res api.DomainsResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
			}
			printDomains(cmd.OutOrStdout(), &res)
			return nil
		},
	}
	var noWait bool
	add := &cobra.Command{
		Use:   "add HOST",
		Short: "Serve the project at a hostname you own",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]string{"host": args[0]})
			raw, err := serverRequest(ctx, k, "POST", "api/projects/"+name+"/domains", body, "application/json")
			if err != nil {
				return err
			}
			var res api.DomainsResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
			}
			host := res.Host
			fmt.Fprintf(out, "Added %s to %s. Create one DNS record at your provider:\n\n", host, name)
			fmt.Fprintf(out, "  %-8s %-40s -> %s\n", "CNAME", host, res.Target)
			if res.Address != "" {
				fmt.Fprintf(out, "  or, for a zone apex that cannot hold a CNAME:\n  %-8s %-40s -> %s\n", "A", host, res.Address)
			}
			fmt.Fprintln(out)
			if noWait {
				fmt.Fprintf(out, "The certificate is issued once %s resolves here; follow it with `shpyrd domains list`.\n", host)
				return nil
			}
			fmt.Fprintf(out, "Waiting for %s to resolve here and for its certificate (Ctrl-C to stop waiting; nothing is lost)...", host)
			deadline := time.Now().Add(30 * time.Minute)
			lastMsg := ""
			for {
				select {
				case <-ctx.Done():
					fmt.Fprintln(out)
					return nil
				case <-time.After(10 * time.Second):
				}
				raw, err := serverRequest(ctx, k, "GET", "api/projects/"+name+"/domains", nil, "")
				if err != nil {
					return err
				}
				if err := json.Unmarshal(raw, &res); err != nil {
					return err
				}
				for _, d := range res.Domains {
					if d.Host != host {
						continue
					}
					if d.DNS == "ok" && (d.Certificate == "ready" || d.Certificate == "wildcard") {
						fmt.Fprintf(out, " done.\n\nhttps://%s serves %s.\n", host, name)
						return nil
					}
					if d.Certificate == "failed" {
						fmt.Fprintln(out)
						return fmt.Errorf("certificate for %s failed: %s", host, d.Message)
					}
					if d.Message != lastMsg && d.DNS != "ok" && d.DNS != "unknown" {
						fmt.Fprintf(out, "\n  %s\n  still waiting...", d.Message)
						lastMsg = d.Message
					}
				}
				if time.Now().After(deadline) {
					fmt.Fprintln(out)
					return fmt.Errorf("%s did not come up within 30 minutes; check the record and `shpyrd domains list`", host)
				}
				fmt.Fprint(out, ".")
			}
		},
	}
	add.Flags().BoolVar(&noWait, "no-wait", false, "return after adding, without waiting for DNS and the certificate")
	rm := &cobra.Command{
		Use:     "rm HOST",
		Aliases: []string{"remove"},
		Short:   "Stop serving the project at a hostname",
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
			if _, err := serverRequest(ctx, k, "DELETE", "api/projects/"+name+"/domains/"+strings.ToLower(args[0]), nil, ""); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from %s (its certificate goes with it).\n", strings.ToLower(args[0]), name)
			return nil
		},
	}
	for _, c := range []*cobra.Command{list, add, rm} {
		appFlag(c, &appName)
	}
	cmd.AddCommand(list, add, rm)
	return cmd
}

func printDomains(out io.Writer, res *api.DomainsResult) {
	fmt.Fprintf(out, "Project hostname: %s (always served; the CNAME target)\n", res.Target)
	if res.Address != "" {
		fmt.Fprintf(out, "Front door:       %s (the %s record target for a zone apex)\n", res.Address, api.ApexRecordType(res.Address))
	}
	if len(res.Domains) == 0 {
		fmt.Fprintln(out, "\nNo custom domains. Add one with `shpyrd domains add www.example.com`.")
		return
	}
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tDNS\tCERTIFICATE\tNOTE")
	for _, d := range res.Domains {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Host, d.DNS, d.Certificate, domainNote(d))
	}
	w.Flush()
}

func domainNote(d shpyrdv1.DomainStatus) string {
	switch {
	case d.DNS == "ok" && (d.Certificate == "ready" || d.Certificate == "wildcard"):
		return "serving"
	case d.DNS == "missing":
		return fmt.Sprintf("create: CNAME %s -> %s (or A -> %s)", d.Host, d.Target, d.Address)
	case d.DNS == "wrong":
		return fmt.Sprintf("points elsewhere: CNAME %s -> %s (or A -> %s)", d.Host, d.Target, d.Address)
	case d.Certificate == "failed":
		return "certificate failed: " + d.Message
	default:
		return firstNonEmpty(d.Message, "waiting")
	}
}
