package localnet

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolverZone(t *testing.T) {
	cases := map[string]string{
		"shpyrd.test":        "test",
		"Apps.Example.":      "example",
		"dev.acme.internal":  "dev.acme.internal",
		"myapps.example.com": "myapps.example.com",
		"localhost":          "localhost",
		"platform.localhost": "localhost",
		"something.invalid":  "invalid",
		"127.0.0.1.nip.io":   "127.0.0.1.nip.io",
	}
	for in, want := range cases {
		if got := ResolverZone(in); got != want {
			t.Errorf("ResolverZone(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsNipIO("127.0.0.1.nip.io") || IsNipIO("shpyrd.test") {
		t.Error("IsNipIO")
	}
}

func TestSiteFile(t *testing.T) {
	s := SiteFile("shpyrd.test", 8080)
	for _, want := range []string{"*.shpyrd.test, shpyrd.test {", "tls internal", "reverse_proxy 127.0.0.1:8080", "header_up X-Forwarded-Proto https", "flush_interval -1", "managed by shpyrd"} {
		if !strings.Contains(s, want) {
			t.Errorf("site file lacks %q:\n%s", want, s)
		}
	}
}

func TestImportDetection(t *testing.T) {
	dir := t.TempDir()
	caddyfile := filepath.Join(dir, "Caddyfile")
	caddyDir := "/Users/me/.shpyrd/caddy"
	if err := os.WriteFile(caddyfile, []byte("{\n\tadmin localhost:2019\n}\nimport /Users/me/.platform2/caddy/Caddyfile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasImport(caddyfile, caddyDir) {
		t.Fatal("import must not be detected yet")
	}
	if err := AddImport(caddyfile, caddyDir); err != nil {
		t.Fatal(err)
	}
	if !HasImport(caddyfile, caddyDir) {
		t.Fatal("import not detected after AddImport")
	}
	b, _ := os.ReadFile(caddyfile)
	if !strings.Contains(string(b), ImportLine(caddyDir)) || !strings.Contains(string(b), "import /Users/me/.platform2/caddy/Caddyfile") {
		t.Errorf("Caddyfile after AddImport:\n%s", b)
	}
	// A wider glob covering the directory counts too.
	if err := os.WriteFile(caddyfile, []byte("import /Users/me/.shpyrd/caddy/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasImport(caddyfile, caddyDir) {
		t.Error("wider glob must count as an import")
	}
}

func TestPorts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	busyPort := l.Addr().(*net.TCPAddr).Port
	if PortFree(busyPort) {
		t.Errorf("port %d is bound but reported free", busyPort)
	}
	if b := BusyPorts(busyPort, 0); len(b) != 1 || b[0] != busyPort {
		t.Errorf("BusyPorts = %v", b)
	}
	h, s := FreePortPair(busyPort, busyPort+1)
	if h == busyPort {
		t.Errorf("FreePortPair returned the busy port %d", h)
	}
	if s-h != 1 {
		t.Errorf("FreePortPair pair not aligned: %d/%d", h, s)
	}
}

func TestDnsmasqRule(t *testing.T) {
	if DnsmasqRule("test") != "address=/.test/127.0.0.1" {
		t.Error(DnsmasqRule("test"))
	}
	if ResolverFile("test") != "/etc/resolver/test" {
		t.Error(ResolverFile("test"))
	}
}

// On Linux a port below net.ipv4.ip_unprivileged_port_start (1024, so the
// default 80 and 443) cannot be bound by a normal user. That is the kernel
// refusing this process, not a conflict: kind maps host ports through the
// Docker daemon, which binds them as root.
func TestBindDenied(t *testing.T) {
	denied := &net.OpError{Op: "listen", Net: "tcp4", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EACCES}}
	inUse := &net.OpError{Op: "listen", Net: "tcp4", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}}
	if !bindDenied(denied) {
		t.Errorf("bindDenied(permission denied) = false, want true")
	}
	if bindDenied(inUse) {
		t.Errorf("bindDenied(address in use) = true, want false")
	}
	if bindDenied(nil) {
		t.Errorf("bindDenied(nil) = true, want false")
	}
}

func TestPortFreeOnPrivilegedPort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: privileged ports are bindable, nothing to prove")
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:80", 200*time.Millisecond); err == nil {
		t.Skip("something is serving port 80 on this machine")
	}
	if !PortFree(80) {
		t.Errorf("PortFree(80) = false with nothing listening; a permission error is not a conflict")
	}
}
