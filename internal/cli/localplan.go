package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"shpyrd/pkg/install"
	"shpyrd/pkg/kexec"
	"shpyrd/pkg/localnet"
)

// Local names and front door (RFC-0057): `cluster create` looks at the
// machine, proposes the fitting setup and records it; `cluster init` reuses
// the record; `cluster destroy` removes what was written on the machine.

const (
	frontDoorAuto      = "auto"
	defaultLocalDomain = "shpyrd.test"
)

// confirm asks a yes/no question on the terminal. --yes answers yes (it
// means "do not ask", including for destructive questions that default to
// no); a non-terminal stdin takes the default.
func confirm(cmd *cobra.Command, yes bool, question string, defaultYes bool) bool {
	if yes {
		return true
	}
	if !kexec.StdinIsTerminal() {
		return defaultYes
	}
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s ", question, hint)
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" {
		return defaultYes
	}
	return answer == "y" || answer == "yes"
}

// planLocal decides domain, ports and front door for a new cluster from
// the flags and what runs on the machine. It mutates flags in place so
// runInit records the outcome. kindPortsGiven says the user chose ports.
func planLocal(cmd *cobra.Command, flags *initFlags, domainGiven, kindPortsGiven bool) error {
	out := cmd.OutOrStdout()
	caddy, hasCaddy := localnet.DetectCaddy()
	busy := localnet.BusyPorts(flags.httpPort, flags.httpsPort)

	switch flags.frontDoor {
	case install.FrontDoorCaddy:
		if !hasCaddy {
			return fmt.Errorf("--front-door caddy: no Caddy answers on %s; start Caddy (brew services start caddy) or use --front-door kind", localnet.CaddyAdmin)
		}
	case install.FrontDoorKind:
		if len(busy) > 0 {
			return busyPortsError(busy, hasCaddy)
		}
	case frontDoorAuto:
		switch {
		case hasCaddy && (len(busy) > 0 || !kindPortsGiven):
			fmt.Fprintf(out, "Caddy %sis serving %s on this machine.\n", versionWord(caddy), portsWord(busy))
			if confirm(cmd, flags.yes, "Use it as the front door (clean https URLs, certificates from Caddy's CA)?", true) {
				flags.frontDoor = install.FrontDoorCaddy
			} else if len(busy) > 0 {
				return busyPortsError(busy, false)
			} else {
				flags.frontDoor = install.FrontDoorKind
			}
		case len(busy) > 0:
			return busyPortsError(busy, false)
		default:
			flags.frontDoor = install.FrontDoorKind
		}
	default:
		return fmt.Errorf("--front-door must be auto, kind or caddy (got %q)", flags.frontDoor)
	}

	if flags.frontDoor == install.FrontDoorCaddy {
		if !kindPortsGiven || len(busy) > 0 {
			flags.httpPort, flags.httpsPort = localnet.FreePortPair(8080, 8443)
		}
		flags.caddy = caddy
		if !domainGiven {
			// Clean names need local DNS; offer it, fall back to nip.io.
			st := localnet.CheckDNS(defaultLocalDomain)
			switch {
			case st.Resolves:
				flags.domain = defaultLocalDomain
			case confirm(cmd, flags.yes, fmt.Sprintf("Make *.%s resolve to this machine with dnsmasq (one sudo prompt), instead of using %s?", defaultLocalDomain, defaultDomain), true):
				flags.domain = defaultLocalDomain
				flags.localDNS = true
			}
		}
	}
	return nil
}

// ensureLocalDNS configures dnsmasq when asked (or when the domain cannot
// resolve otherwise) and records whether names resolve locally.
func ensureLocalDNS(cmd *cobra.Command, flags *initFlags) error {
	out := cmd.OutOrStdout()
	if localnet.IsNipIO(flags.domain) {
		flags.localDNS = false
		return nil
	}
	st := localnet.CheckDNS(flags.domain)
	if st.Resolves && !flags.localDNS {
		flags.localDNS = true
		fmt.Fprintf(out, "Names: *.%s already resolve to this machine.\n", flags.domain)
		return nil
	}
	if !st.Resolves && !flags.localDNS {
		if !confirm(cmd, flags.yes, fmt.Sprintf("*.%s does not resolve to this machine. Configure dnsmasq and /etc/resolver for it (one sudo prompt)?", flags.domain), true) {
			fmt.Fprintf(out, "Warning: *.%s does not resolve here; point it at 127.0.0.1 yourself (dnsmasq: %s).\n", flags.domain, localnet.DnsmasqRule(localnet.ResolverZone(flags.domain)))
			return nil
		}
		flags.localDNS = true
	}
	if !st.Resolves {
		if err := localnet.SetupDNS(flags.domain, func(m string) { fmt.Fprintln(out, m) }); err != nil {
			return err
		}
		if !localnet.CheckDNS(flags.domain).Resolves {
			fmt.Fprintf(out, "Warning: *.%s still does not resolve; a restart of dnsmasq or of the resolver may be needed (sudo killall -HUP mDNSResponder).\n", flags.domain)
		} else {
			fmt.Fprintf(out, "Names: *.%s resolve to this machine (dnsmasq).\n", flags.domain)
		}
	}
	return nil
}

// setupFrontDoor writes the Caddy site, wires the import line and reloads
// Caddy, then checks the dashboard answers through it.
func setupFrontDoor(ctx context.Context, cmd *cobra.Command, flags *initFlags) error {
	out := cmd.OutOrStdout()
	caddy := flags.caddy
	if caddy == nil {
		var ok bool
		if caddy, ok = localnet.DetectCaddy(); !ok {
			return fmt.Errorf("no Caddy answers on %s; start it, then run `shpyrd cluster init` again", localnet.CaddyAdmin)
		}
	}
	path, err := localnet.WriteSiteFile(flags.domain, flags.httpPort)
	if err != nil {
		return err
	}
	dir, _ := localnet.CaddyDir()
	fmt.Fprintf(out, "Front door: wrote %s (Caddy on 443 -> kind :%d)\n", path, flags.httpPort)
	if caddy.Caddyfile == "" {
		fmt.Fprintf(out, "Add this line to your Caddyfile, then reload Caddy:\n  %s\n", localnet.ImportLine(dir))
		return nil
	}
	if !localnet.HasImport(caddy.Caddyfile, dir) {
		if confirm(cmd, flags.yes, fmt.Sprintf("Add `%s` to %s?", localnet.ImportLine(dir), caddy.Caddyfile), true) {
			if err := localnet.AddImport(caddy.Caddyfile, dir); err != nil {
				return fmt.Errorf("%w\nAdd the line yourself: %s", err, localnet.ImportLine(dir))
			}
		} else {
			fmt.Fprintf(out, "Add this line to %s yourself, then reload Caddy:\n  %s\n", caddy.Caddyfile, localnet.ImportLine(dir))
			return nil
		}
	}
	if err := localnet.Reload(ctx, caddy.Caddyfile); err != nil {
		return err
	}
	host := "shpyrd." + flags.domain
	if localnet.Serves(ctx, host) {
		fmt.Fprintf(out, "Front door: Caddy reloaded; https://%s answers through it.\n", host)
	} else {
		fmt.Fprintf(out, "Front door: Caddy reloaded, but https://%s does not answer yet; check `caddy validate --config %s` and that *.%s resolves here.\n", host, caddy.Caddyfile, flags.domain)
	}
	return nil
}

// teardownLocal removes the Caddy site and, on request, the DNS rule.
func teardownLocal(ctx context.Context, cmd *cobra.Command, yes bool, domain, frontDoor string, localDNS bool) {
	out := cmd.OutOrStdout()
	if removed, err := localnet.RemoveSiteFile(); err == nil && removed {
		fmt.Fprintln(out, "Removed the Caddy site file.")
		if caddy, ok := localnet.DetectCaddy(); ok && caddy.Caddyfile != "" {
			if err := localnet.Reload(ctx, caddy.Caddyfile); err != nil {
				fmt.Fprintf(out, "Reload Caddy yourself: %v\n", err)
			}
		}
	} else if frontDoor == install.FrontDoorCaddy && err != nil {
		fmt.Fprintf(out, "Could not remove the Caddy site file: %v\n", err)
	}
	if localDNS && domain != "" && localnet.CheckDNS(domain).Managed {
		if confirm(cmd, false, fmt.Sprintf("Remove the dnsmasq rule and resolver file shpyrd wrote for *.%s?", domain), false) {
			if err := localnet.RemoveDNS(domain, func(m string) { fmt.Fprintln(out, m) }); err != nil {
				fmt.Fprintf(out, "%v\n", err)
			}
		}
	}
}

// describeLocal is the "Names ... Front door ..." status line.
func describeLocal(vars map[string]string) string {
	domain := vars[install.VarDomain]
	names := "public DNS (" + domain + ")"
	if vars[install.VarLocalDNS] == "true" {
		names = "dnsmasq (*." + domain + ")"
	}
	door := "kind on " + firstNonEmpty(vars[install.VarHTTPPort], "80") + "/" + firstNonEmpty(vars[install.VarHTTPSPort], "443")
	switch vars[install.VarFrontDoor] {
	case install.FrontDoorCaddy:
		door = "Caddy on 443 -> kind :" + firstNonEmpty(vars[install.VarHTTPPort], "8080")
	case install.FrontDoorLB:
		door = "cloud load balancer on 80/443 · Certificates: " + firstNonEmpty(vars[install.VarClusterIssuer], "letsencrypt")
	}
	return "Names: " + names + " · Front door: " + door
}

func busyPortsError(busy []int, hasCaddy bool) error {
	ports := make([]string, 0, len(busy))
	for _, p := range busy {
		ports = append(ports, fmt.Sprint(p))
	}
	msg := fmt.Sprintf("host port(s) %s already in use", strings.Join(ports, ", "))
	if hasCaddy {
		return errors.New(msg + "; pass --front-door caddy to put the cluster behind the running Caddy, or --http-port 8080 --https-port 8443")
	}
	return errors.New(msg + "; stop the process using them, or pass --http-port 8080 --https-port 8443")
}

func versionWord(c *localnet.Caddy) string {
	if c != nil && c.Version != "" {
		return c.Version + " "
	}
	return ""
}

func portsWord(busy []int) string {
	if len(busy) == 0 {
		return "port 443"
	}
	parts := make([]string, 0, len(busy))
	for _, p := range busy {
		parts = append(parts, fmt.Sprint(p))
	}
	return "port(s) " + strings.Join(parts, " and ")
}

// printLocalSummary closes `cluster init` output with what to trust.
func printLocalSummary(out io.Writer, vars map[string]string) {
	fmt.Fprintf(out, "  %s\n", describeLocal(vars))
	switch vars[install.VarFrontDoor] {
	case install.FrontDoorCaddy:
		fmt.Fprintln(out, "\nCertificates come from Caddy's local CA. If the browser warns, run `caddy trust` once.")
	case install.FrontDoorLB:
		fmt.Fprintln(out, "\nCertificates are publicly trusted (Let's Encrypt); nothing to install. Open the dashboard with `shpyrd cluster dashboard`.")
	default:
		fmt.Fprintln(out, "\nRun `shpyrd cluster trust-ca` once so your browser trusts the development CA.")
	}
}
