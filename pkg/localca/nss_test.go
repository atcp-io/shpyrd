package localca

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNSSArgs(t *testing.T) {
	want := []string{"-d", "sql:/home/x/.pki/nssdb", "-A", "-t", "C,,", "-n", "shpyrd CA", "-i", "/ca.pem"}
	if got := nssImportArgs("/home/x/.pki/nssdb", "shpyrd CA", "/ca.pem"); !reflect.DeepEqual(got, want) {
		t.Errorf("nssImportArgs = %q, want %q", got, want)
	}
	want = []string{"-d", "sql:/home/x/.pki/nssdb", "-D", "-n", "shpyrd CA"}
	if got := nssDeleteArgs("/home/x/.pki/nssdb", "shpyrd CA"); !reflect.DeepEqual(got, want) {
		t.Errorf("nssDeleteArgs = %q, want %q", got, want)
	}
}

// The database is the browser's, so a missing one is left alone rather than
// created: Chromium makes it on first run.
func TestNSSDatabaseMustExist(t *testing.T) {
	if _, ok := nssDatabase(filepath.Join(t.TempDir(), "absent")); ok {
		t.Errorf("nssDatabase accepted a directory that does not exist")
	}
	dir := t.TempDir()
	if _, err := exec.LookPath("certutil"); err != nil {
		t.Skip("certutil not installed: cannot judge an existing database")
	}
	if _, ok := nssDatabase(dir); !ok {
		t.Errorf("nssDatabase rejected an existing directory")
	}
}

// Imports and removes a real certificate in a throwaway NSS database, which
// is what the browsers read.
func TestNSSImportAndDelete(t *testing.T) {
	if _, err := exec.LookPath("certutil"); err != nil {
		t.Skip("certutil not installed (libnss3-tools)")
	}
	db := t.TempDir()
	mk := exec.Command("certutil", "-N", "--empty-password", "-d", "sql:"+db)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("certutil -N: %v: %s", err, out)
	}
	ca, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if err := nssImport(db, "shpyrd test CA", ca.CertPath()); err != nil {
		t.Fatalf("nssImport: %v", err)
	}
	listed, err := exec.Command("certutil", "-L", "-d", "sql:"+db).Output()
	if err != nil {
		t.Fatalf("certutil -L: %v", err)
	}
	if !strings.Contains(string(listed), "shpyrd test CA") {
		t.Fatalf("imported certificate not listed:\n%s", listed)
	}

	if err := nssDelete(db, "shpyrd test CA"); err != nil {
		t.Fatalf("nssDelete: %v", err)
	}
	listed, err = exec.Command("certutil", "-L", "-d", "sql:"+db).Output()
	if err != nil {
		t.Fatalf("certutil -L: %v", err)
	}
	if strings.Contains(string(listed), "shpyrd test CA") {
		t.Errorf("certificate still listed after delete:\n%s", listed)
	}
	if err := nssDelete(db, "shpyrd test CA"); err == nil {
		t.Errorf("nssDelete on a missing nickname = nil, want an error the caller can ignore")
	}
	_ = os.Remove(filepath.Join(db, "cert9.db"))
}
