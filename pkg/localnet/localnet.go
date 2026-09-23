// Package localnet knows the developer's machine (RFC-0057): which ports are
// free, whether a Caddy already owns 443 and can be the front door, and
// whether *.<domain> resolves locally through dnsmasq and macOS resolver
// files. Everything it writes is marked "managed by shpyrd" and removed the
// same way.
package localnet

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FrontDoor modes recorded as SHPYRD_FRONT_DOOR.
const (
	FrontDoorKind  = "kind"  // kind maps the host ports itself (80/443 or high ports)
	FrontDoorCaddy = "caddy" // an existing Caddy on 443 proxies to kind's HTTP port
)

// PortFree reports whether a TCP port is unused on this machine: nothing
// answers on 127.0.0.1 and the IPv4 wildcard can be bound (a dual-stack
// wildcard listen would not conflict with a loopback-only socket on macOS).
func PortFree(port int) bool {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		return false
	}
	l, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// BusyPorts returns the ports of the list that are in use.
func BusyPorts(ports ...int) []int {
	var busy []int
	for _, p := range ports {
		if p != 0 && !PortFree(p) {
			busy = append(busy, p)
		}
	}
	return busy
}

// FreePortPair picks host ports for kind when 80/443 are taken: the
// preferred pair when free, else the next pairs up (8081/8444...).
func FreePortPair(preferHTTP, preferHTTPS int) (int, int) {
	for i := 0; i < 50; i++ {
		h, s := preferHTTP+i, preferHTTPS+i
		if PortFree(h) && PortFree(s) {
			return h, s
		}
	}
	return preferHTTP, preferHTTPS
}

// Dir is where shpyrd keeps what it writes on the machine (~/.shpyrd).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".shpyrd"), nil
}

// sudoRun runs a command through sudo with the terminal attached, so sudo
// itself asks for the password. The manual equivalent is returned with the
// error so the user can run it by hand when sudo is not available.
func sudoRun(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// brewPrefix returns Homebrew's prefix ("" when Homebrew is not installed).
func brewPrefix() string {
	for _, p := range []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"} {
		if _, err := os.Stat(filepath.Join(p, "bin", "brew")); err == nil {
			return p
		}
	}
	return ""
}

func fileContains(path, needle string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), needle)
}
