package localnet

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Reserved top-level domains (RFC 2606, RFC 6761) that never leak to public
// DNS; a domain under one of them gets a resolver rule for the whole TLD.
var reservedTLDs = map[string]bool{"test": true, "example": true, "invalid": true, "localhost": true}

// ResolverZone is the zone local DNS handles for a domain: the TLD when it
// is reserved ("test" for shpyrd.test), else the domain itself.
func ResolverZone(domain string) string {
	domain = strings.Trim(strings.ToLower(domain), ".")
	if i := strings.LastIndex(domain, "."); i >= 0 {
		if tld := domain[i+1:]; reservedTLDs[tld] {
			return tld
		}
	}
	return domain
}

// IsNipIO says the domain resolves publicly to an address embedded in it.
func IsNipIO(domain string) bool {
	return strings.HasSuffix(domain, ".nip.io") || strings.HasSuffix(domain, ".sslip.io") || domain == "localhost"
}

// ResolverFile is the macOS resolver file for the zone (/etc/resolver/test).
func ResolverFile(zone string) string { return filepath.Join("/etc/resolver", zone) }

// DnsmasqConfFile is where shpyrd's dnsmasq rule lives.
func DnsmasqConfFile() string {
	p := brewPrefix()
	if p == "" {
		return ""
	}
	return filepath.Join(p, "etc", "dnsmasq.d", "shpyrd.conf")
}

// DnsmasqRule maps every name under the zone to this machine.
func DnsmasqRule(zone string) string { return "address=/." + zone + "/127.0.0.1" }

// DNSStatus describes how *.<domain> resolves on this machine.
type DNSStatus struct {
	Zone string
	// Resolves says a name under the domain resolves to 127.0.0.1 through
	// the system resolver (however it is configured).
	Resolves bool
	// ResolverFile and DnsmasqRule say whether the two pieces exist (the
	// files may have been written by hand or by another tool).
	ResolverFile bool
	DnsmasqRule  bool
	// Managed says shpyrd wrote the dnsmasq rule (and so may remove it).
	Managed bool
}

// CheckDNS inspects the machine for a domain.
func CheckDNS(domain string) DNSStatus {
	zone := ResolverZone(domain)
	st := DNSStatus{Zone: zone, Resolves: resolvesLocally("probe." + domain)}
	st.ResolverFile = fileContains(ResolverFile(zone), "nameserver 127.0.0.1")
	if p := brewPrefix(); p != "" {
		rule := DnsmasqRule(zone)
		st.DnsmasqRule = fileContains(filepath.Join(p, "etc", "dnsmasq.conf"), rule)
		matches, _ := filepath.Glob(filepath.Join(p, "etc", "dnsmasq.d", "*.conf"))
		for _, m := range matches {
			if fileContains(m, rule) {
				st.DnsmasqRule = true
				if filepath.Base(m) == "shpyrd.conf" {
					st.Managed = true
				}
			}
		}
	}
	return st
}

// resolvesLocally asks the operating system's resolver (Go's own resolver
// ignores /etc/resolver on macOS) whether a name points at this machine.
func resolvesLocally(host string) bool {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/usr/bin/dscacheutil", "-q", "host", "-a", "name", host).Output()
		if err == nil {
			return strings.Contains(string(out), "ip_address: 127.0.0.1")
		}
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a == "127.0.0.1" || a == "::1" {
			return true
		}
	}
	return false
}

// SetupDNS makes *.<domain> resolve to this machine: a dnsmasq rule in
// Homebrew's dnsmasq.d, a resolver file pointing the zone at dnsmasq, and a
// restart of dnsmasq. One sudo invocation covers the privileged steps.
// dnsmasq is installed through Homebrew when missing. macOS only.
func SetupDNS(domain string, log func(string)) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("local DNS setup is automated on macOS only; on %s add %q to dnsmasq (or your resolver) yourself", runtime.GOOS, DnsmasqRule(ResolverZone(domain)))
	}
	prefix := brewPrefix()
	if prefix == "" {
		return fmt.Errorf("Homebrew is required to install dnsmasq (https://brew.sh)")
	}
	zone := ResolverZone(domain)
	if _, err := os.Stat(filepath.Join(prefix, "sbin", "dnsmasq")); err != nil {
		log("Installing dnsmasq with Homebrew...")
		cmd := exec.Command("brew", "install", "dnsmasq")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("brew install dnsmasq: %w", err)
		}
	}
	confDir := filepath.Join(prefix, "etc", "dnsmasq.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	rule := DnsmasqRule(zone)
	if err := os.WriteFile(filepath.Join(confDir, "shpyrd.conf"), []byte("# managed by shpyrd (RFC-0057): *."+zone+" resolves to this machine\n"+rule+"\n"), 0o644); err != nil {
		return err
	}
	mainConf := filepath.Join(prefix, "etc", "dnsmasq.conf")
	confDirLine := "conf-dir=" + confDir + "/,*.conf"
	if !fileContains(mainConf, confDirLine) && !fileContains(mainConf, "conf-dir="+confDir) {
		f, err := os.OpenFile(mainConf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(f, "\n# shpyrd: read rules from dnsmasq.d\n%s\n", confDirLine)
		f.Close()
	}
	resolver := ResolverFile(zone)
	log(fmt.Sprintf("Writing %s and restarting dnsmasq (administrator rights required)...", resolver))
	script := fmt.Sprintf(`mkdir -p /etc/resolver && printf '# managed by shpyrd (RFC-0057)\nnameserver 127.0.0.1\n' > %q && %s services restart dnsmasq >/dev/null && (dscacheutil -flushcache; killall -HUP mDNSResponder) 2>/dev/null || true`, resolver, filepath.Join(prefix, "bin", "brew"))
	if err := sudoRun("sh", "-c", script); err != nil {
		return fmt.Errorf("%w\nRun by hand:\n  sudo sh -c %q", err, script)
	}
	return nil
}

// RemoveDNS undoes SetupDNS for the zone, but only the pieces shpyrd wrote.
func RemoveDNS(domain string, log func(string)) error {
	zone := ResolverZone(domain)
	conf := DnsmasqConfFile()
	if conf == "" || !fileContains(conf, DnsmasqRule(zone)) {
		return fmt.Errorf("the dnsmasq rule for .%s was not written by shpyrd; leaving DNS untouched", zone)
	}
	if err := os.Remove(conf); err != nil && !os.IsNotExist(err) {
		return err
	}
	resolver := ResolverFile(zone)
	script := fmt.Sprintf(`grep -q 'managed by shpyrd' %[1]q && rm -f %[1]q; %[2]s services restart dnsmasq >/dev/null; (dscacheutil -flushcache; killall -HUP mDNSResponder) 2>/dev/null || true`, resolver, filepath.Join(brewPrefix(), "bin", "brew"))
	log(fmt.Sprintf("Removing %s and restarting dnsmasq (administrator rights required)...", resolver))
	return sudoRun("sh", "-c", script)
}

func insecureTransport() *http.Transport {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // local probe of the developer's own Caddy
}
