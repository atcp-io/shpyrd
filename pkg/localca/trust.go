package localca

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// TrustResult describes what Trust did or what the user must do manually.
type TrustResult struct {
	Installed    bool
	Instructions string
}

// Trust installs the CA certificate in the operating system trust store.
// It needs administrator rights (sudo) on macOS and Linux; when they are not
// available the returned instructions explain the manual step. Firefox and
// some tools keep their own stores; the instructions mention that too.
func (c *CA) Trust() (*TrustResult, error) {
	switch runtime.GOOS {
	case "darwin":
		return c.trustDarwin()
	case "linux":
		return c.trustLinux()
	default:
		return &TrustResult{Instructions: c.manualInstructions()}, nil
	}
}

func (c *CA) trustDarwin() (*TrustResult, error) {
	cmd := exec.Command("sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain", c.CertPath())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &TrustResult{Instructions: c.manualInstructions()},
			fmt.Errorf("security add-trusted-cert: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return &TrustResult{Installed: true, Instructions: firefoxNote}, nil
}

func (c *CA) trustLinux() (*TrustResult, error) {
	type target struct {
		dir, file, update string
	}
	candidates := []target{
		{"/usr/local/share/ca-certificates", "shpyrd-dev-ca.crt", "update-ca-certificates"},        // Debian, Ubuntu
		{"/etc/pki/ca-trust/source/anchors", "shpyrd-dev-ca.pem", "update-ca-trust"},               // Fedora, RHEL
		{"/etc/ca-certificates/trust-source/anchors", "shpyrd-dev-ca.crt", "trust extract-compat"}, // Arch
	}
	for _, t := range candidates {
		if _, err := exec.LookPath(strings.Fields(t.update)[0]); err != nil {
			continue
		}
		if st, err := os.Stat(t.dir); err != nil || !st.IsDir() {
			continue
		}
		dest := filepath.Join(t.dir, t.file)
		cp := exec.Command("sudo", "cp", c.CertPath(), dest)
		cp.Stdin, cp.Stdout, cp.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cp.Run(); err != nil {
			return &TrustResult{Instructions: c.manualInstructions()}, fmt.Errorf("copy certificate: %w", err)
		}
		parts := strings.Fields(t.update)
		up := exec.Command("sudo", parts...)
		up.Stdin, up.Stdout, up.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := up.Run(); err != nil {
			return &TrustResult{Instructions: c.manualInstructions()}, fmt.Errorf("%s: %w", t.update, err)
		}
		return &TrustResult{Installed: true, Instructions: firefoxNote}, nil
	}
	return &TrustResult{Instructions: c.manualInstructions()}, nil
}

const firefoxNote = "Firefox keeps its own store: set security.enterprise_roots.enabled=true in about:config or import the certificate under Settings > Certificates."

func (c *CA) manualInstructions() string {
	p := c.CertPath()
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("Run: sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain %s\n%s", p, firefoxNote)
	case "linux":
		return fmt.Sprintf("Debian/Ubuntu: sudo cp %s /usr/local/share/ca-certificates/shpyrd-dev-ca.crt && sudo update-ca-certificates\nFedora/RHEL: sudo cp %s /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust\n%s", p, p, firefoxNote)
	case "windows":
		return fmt.Sprintf("Run in an elevated prompt: certutil -addstore -f Root %s", p)
	}
	return "Import " + p + " as a trusted root certificate."
}
