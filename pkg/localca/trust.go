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
	Installed bool
	// Browsers is true when the CA also reached the NSS database the
	// Chromium family reads, which the system trust store does not cover.
	Browsers     bool
	Instructions string
}

// UntrustResult describes what Untrust removed.
type UntrustResult struct {
	// Removed names each store the certificate was taken out of.
	Removed      []string
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
	return &TrustResult{Installed: true, Instructions: browserNote}, nil
}

// nickname is how the certificate is listed in a browser's store: the CA's
// own common name, so a cluster CA and the development CA stay apart.
func (c *CA) nickname() string {
	if cn := c.Cert.Subject.CommonName; cn != "" {
		return cn
	}
	return "shpyrd platform CA"
}

// linuxStore is a system trust store and the command that rebuilds it.
type linuxStore struct {
	dir, file, update string
}

func linuxStores() []linuxStore {
	return []linuxStore{
		{"/usr/local/share/ca-certificates", "shpyrd-dev-ca.crt", "update-ca-certificates"},        // Debian, Ubuntu
		{"/etc/pki/ca-trust/source/anchors", "shpyrd-dev-ca.pem", "update-ca-trust"},               // Fedora, RHEL
		{"/etc/ca-certificates/trust-source/anchors", "shpyrd-dev-ca.crt", "trust extract-compat"}, // Arch
	}
}

func (c *CA) trustLinux() (*TrustResult, error) {
	for _, t := range linuxStores() {
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
		res := &TrustResult{Installed: true, Instructions: browserNote}
		// The Chromium family does not read the store just updated.
		if db, ok := nssDatabase(nssSharedDatabase()); ok {
			if err := nssImport(db, c.nickname(), c.CertPath()); err != nil {
				res.Instructions = fmt.Sprintf("Could not add the CA to the browser store (%v). Run: %s\n%s", err, nssCommand(c.nickname(), c.CertPath()), browserNote)
			} else {
				res.Browsers = true
			}
		} else {
			res.Instructions = fmt.Sprintf("Chromium-based browsers keep their own store. Once the browser has run, add the CA with: %s\n%s", nssCommand(c.nickname(), c.CertPath()), browserNote)
		}
		return res, nil
	}
	return &TrustResult{Instructions: c.manualInstructions()}, nil
}

// Untrust removes the CA from the operating system trust store and from the
// browser store on Linux. It needs administrator rights for the system
// store, the same as Trust.
func (c *CA) Untrust() (*UntrustResult, error) {
	switch runtime.GOOS {
	case "darwin":
		return c.untrustDarwin()
	case "linux":
		return c.untrustLinux()
	default:
		return &UntrustResult{Instructions: "Remove " + c.CertPath() + " from the trusted root certificates."}, nil
	}
}

func (c *CA) untrustDarwin() (*UntrustResult, error) {
	cmd := exec.Command("sudo", "security", "remove-trusted-cert", "-d", c.CertPath())
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &UntrustResult{Instructions: "Run: sudo security remove-trusted-cert -d " + c.CertPath()},
			fmt.Errorf("security remove-trusted-cert: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return &UntrustResult{Removed: []string{"system keychain"}}, nil
}

func (c *CA) untrustLinux() (*UntrustResult, error) {
	res := &UntrustResult{}
	for _, t := range linuxStores() {
		dest := filepath.Join(t.dir, t.file)
		if _, err := os.Stat(dest); err != nil {
			continue
		}
		rm := exec.Command("sudo", "rm", "-f", dest)
		rm.Stdin, rm.Stdout, rm.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := rm.Run(); err != nil {
			return res, fmt.Errorf("remove %s: %w", dest, err)
		}
		parts := strings.Fields(t.update)
		up := exec.Command("sudo", parts...)
		up.Stdin, up.Stdout, up.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := up.Run(); err != nil {
			return res, fmt.Errorf("%s: %w", t.update, err)
		}
		res.Removed = append(res.Removed, dest)
	}
	// A nickname that is not there fails, which is the same as done.
	if db, ok := nssDatabase(nssSharedDatabase()); ok {
		if err := nssDelete(db, c.nickname()); err == nil {
			res.Removed = append(res.Removed, db)
		}
	}
	if len(res.Removed) == 0 {
		res.Instructions = "The CA was not in any store this command manages."
	}
	return res, nil
}

// browserNote covers the browsers the system trust store does not reach.
// Chromium's database is handled by Trust when certutil is installed; the
// browser still has to be restarted to read it.
const browserNote = "Restart the browser to pick up the certificate. Firefox keeps a store per profile: set security.enterprise_roots.enabled=true in about:config, or import the certificate under Settings > Certificates."

func (c *CA) manualInstructions() string {
	p := c.CertPath()
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("Run: sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain %s\n%s", p, browserNote)
	case "linux":
		return fmt.Sprintf("Debian/Ubuntu: sudo cp %s /usr/local/share/ca-certificates/shpyrd-dev-ca.crt && sudo update-ca-certificates\nFedora/RHEL: sudo cp %s /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust\n%s", p, p, browserNote)
	case "windows":
		return fmt.Sprintf("Run in an elevated prompt: certutil -addstore -f Root %s", p)
	}
	return "Import " + p + " as a trusted root certificate."
}
