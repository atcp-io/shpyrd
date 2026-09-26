package localca

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Chromium, Chrome, Brave, Vivaldi and Edge share one NSS database on Linux
// and do not read the operating system trust store, so installing the CA
// there is what makes the dashboard's certificate trusted in a browser.
// Firefox keeps a database per profile and reads the system store when
// security.enterprise_roots.enabled is set; it is left to browserNote.
func nssImportArgs(db, nickname, certPath string) []string {
	// "C,," trusts the certificate as a CA for TLS only.
	return []string{"-d", "sql:" + db, "-A", "-t", "C,,", "-n", nickname, "-i", certPath}
}

func nssDeleteArgs(db, nickname string) []string {
	return []string{"-d", "sql:" + db, "-D", "-n", nickname}
}

// nssDatabase reports the database when certutil and the database both
// exist. A missing database is left alone rather than created: it belongs to
// the browser, which makes it on first run.
func nssDatabase(db string) (string, bool) {
	if db == "" {
		return "", false
	}
	if _, err := exec.LookPath("certutil"); err != nil {
		return "", false
	}
	if st, err := os.Stat(db); err != nil || !st.IsDir() {
		return "", false
	}
	return db, true
}

// nssSharedDatabase is where the Chromium family keeps its certificates.
func nssSharedDatabase() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pki", "nssdb")
}

func nssImport(db, nickname, certPath string) error {
	return certutil(nssImportArgs(db, nickname, certPath))
}

func nssDelete(db, nickname string) error {
	return certutil(nssDeleteArgs(db, nickname))
}

func certutil(args []string) error {
	var stderr bytes.Buffer
	cmd := exec.Command("certutil", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("certutil: %v: %s", err, msg)
		}
		return fmt.Errorf("certutil: %w", err)
	}
	return nil
}

// nssCommand is the line a user runs when shpyrd could not do it, because
// certutil is missing or the browser has never started.
func nssCommand(nickname, certPath string) string {
	return "certutil " + strings.Join(nssImportArgs(nssSharedDatabase(), nickname, certPath), " ")
}
