package localnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CaddyAdmin is Caddy's default admin endpoint.
const CaddyAdmin = "http://localhost:2019"

// SiteFileName is the file shpyrd owns inside the Caddy directory.
const SiteFileName = "shpyrd.caddy"

// Caddy describes a Caddy found running on this machine.
type Caddy struct {
	// Caddyfile is the configuration file the running process was started
	// with, when it could be determined ("" otherwise).
	Caddyfile string
	// Version as reported by the admin API, when available.
	Version string
}

// DetectCaddy reports whether a Caddy answers on the admin API and where
// its Caddyfile is.
func DetectCaddy() (*Caddy, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(CaddyAdmin + "/config/")
	if err != nil {
		return nil, false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	c := &Caddy{Caddyfile: findCaddyfile()}
	if out, err := exec.Command("caddy", "version").Output(); err == nil {
		c.Version = strings.Fields(string(out))[0]
	}
	return c, true
}

var configFlagRe = regexp.MustCompile(`(?:--config|-c)[ =]([^\s]+)`)

// findCaddyfile reads the running process's command line for --config, then
// falls back to the usual Homebrew and Linux locations.
func findCaddyfile() string {
	if out, err := exec.Command("pgrep", "-x", "caddy").Output(); err == nil {
		for _, pid := range strings.Fields(string(out)) {
			if cmdline, err := exec.Command("ps", "-o", "command=", "-p", pid).Output(); err == nil {
				if m := configFlagRe.FindStringSubmatch(string(cmdline)); m != nil {
					if _, err := os.Stat(m[1]); err == nil {
						return m[1]
					}
				}
			}
		}
	}
	candidates := []string{"/etc/caddy/Caddyfile"}
	if p := brewPrefix(); p != "" {
		candidates = append([]string{filepath.Join(p, "etc", "Caddyfile")}, candidates...)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// CaddyDir is ~/.shpyrd/caddy.
func CaddyDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "caddy"), nil
}

// SiteFile renders the Caddy site that makes Caddy the front door: TLS from
// Caddy's internal CA, plain HTTP to kind's HTTP host port, the scheme
// forwarded so ingress-nginx neither redirects nor lies to the apps, and
// responses flushed as they come so log streams are not buffered.
func SiteFile(domain string, httpPort int) string {
	return fmt.Sprintf(`# managed by shpyrd: the front door for *.%[1]s (RFC-0057).
# Edits are overwritten by `+"`shpyrd cluster init`"+`; remove with `+"`shpyrd cluster destroy`"+`.
*.%[1]s, %[1]s {
	tls internal
	reverse_proxy 127.0.0.1:%[2]d {
		header_up X-Forwarded-Proto https
		flush_interval -1
	}
}
`, domain, httpPort)
}

// WriteSiteFile writes ~/.shpyrd/caddy/shpyrd.caddy and returns its path.
func WriteSiteFile(domain string, httpPort int) (string, error) {
	dir, err := CaddyDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, SiteFileName)
	return path, os.WriteFile(path, []byte(SiteFile(domain, httpPort)), 0o644)
}

// RemoveSiteFile deletes the site file; missing is fine. It reports whether
// something was removed.
func RemoveSiteFile() (bool, error) {
	dir, err := CaddyDir()
	if err != nil {
		return false, err
	}
	err = os.Remove(filepath.Join(dir, SiteFileName))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// SiteFileExists says whether shpyrd left a site file on this machine.
func SiteFileExists() bool {
	dir, err := CaddyDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, SiteFileName))
	return err == nil
}

// ImportLine is what the user's Caddyfile needs once.
func ImportLine(caddyDir string) string { return "import " + filepath.Join(caddyDir, "*.caddy") }

// HasImport reports whether the Caddyfile already imports shpyrd's directory
// (exactly, or through a wider glob such as `import /Users/me/.shpyrd/caddy/*`).
func HasImport(caddyfile, caddyDir string) bool {
	b, err := os.ReadFile(caddyfile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "import" && strings.HasPrefix(f[1], caddyDir) {
			return true
		}
	}
	return false
}

// AddImport appends the import line to the Caddyfile.
func AddImport(caddyfile, caddyDir string) error {
	f, err := os.OpenFile(caddyfile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# shpyrd: sites written by `shpyrd cluster create` (RFC-0057)\n%s\n", ImportLine(caddyDir))
	return err
}

// Reload asks the running Caddy to load the Caddyfile again: through the
// caddy binary when present (it knows the file's directory for imports),
// else through the admin API with the file's content.
func Reload(ctx context.Context, caddyfile string) error {
	if caddyfile == "" {
		return errors.New("the Caddyfile of the running Caddy could not be found; run `caddy reload` yourself")
	}
	if bin, err := exec.LookPath("caddy"); err == nil {
		cmd := exec.CommandContext(ctx, bin, "reload", "--config", caddyfile, "--adapter", "caddyfile")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("caddy reload: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	body, err := os.ReadFile(caddyfile)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, CaddyAdmin+"/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		return fmt.Errorf("caddy admin API: %s: %s", resp.Status, strings.TrimSpace(out.String()))
	}
	return nil
}

// Serves reports whether Caddy answers TLS for host on 443 and the request
// reaches shpyrd (any HTTP status is fine: the point is the path works).
func Serves(ctx context.Context, host string) bool {
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/api/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}
