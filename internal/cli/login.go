package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Session store (RFC-0052): a JSON file at ~/.shpyrd/sessions.json holds the
// token (admin or user) per workspace URL, so project commands work without a
// kubeconfig. The operator's cluster commands (cluster init, …) keep the
// kubeconfig and live in shpyrd-ctl.

const sessionsFileName = "sessions.json"

type loginSessions struct {
	Sessions map[string]*loginSession `json:"sessions"` // keyed by normalised workspace URL
}

type loginSession struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	SavedAt   time.Time `json:"savedAt"`
	WhoAmI    string    `json:"whoAmI,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

func sessionsPath() string {
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".shpyrd", sessionsFileName)
}

func loadSessions() *loginSessions {
	s := &loginSessions{Sessions: map[string]*loginSession{}}
	raw, err := os.ReadFile(sessionsPath())
	if err == nil {
		_ = json.Unmarshal(raw, s)
		if s.Sessions == nil {
			s.Sessions = map[string]*loginSession{}
		}
	}
	return s
}

func (s *loginSessions) save() error {
	p := sessionsPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, fs.FileMode(0o600))
}

// normaliseURL strips trailing slashes and ensures the scheme is present.
func normaliseURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("not a valid URL: %q", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// SessionFor returns the saved token for the given workspace URL, or "" when
// none is stored.
func SessionFor(wsURL string) string {
	if wsURL == "" {
		return ""
	}
	norm, err := normaliseURL(wsURL)
	if err != nil {
		return ""
	}
	s := loadSessions()
	if sess, ok := s.Sessions[norm]; ok {
		return sess.Token
	}
	return ""
}

// DirectURL returns the workspace URL a saved session uses for direct HTTP.
// It exists so the API transport can dial the right address.
func DirectURL(wsURL string) string {
	if wsURL == "" {
		return ""
	}
	norm, _ := normaliseURL(wsURL)
	s := loadSessions()
	if sess, ok := s.Sessions[norm]; ok {
		return sess.URL
	}
	return ""
}

// newLoginCmd is `shpyrd login`.
func newLoginCmd(g *globalFlags) *cobra.Command {
	var (
		wsURL string
		token string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save credentials for a workspace (no kubeconfig needed for project commands)",
		Long: `Sign in to a shpyrd workspace so project commands work without a kubeconfig:

  shpyrd login --url https://acme.shpyrd.app          # opens the browser for the token
  shpyrd login --url https://shpyrd.oci.shpyrd.io --token <admin token>

After login, every project command (deploy, logs, scale, secrets, access,
members, drains, volumes, pg, redis, allow, domains, releases, rollback,
redeploy, open) uses the workspace API and your identity; no kubeconfig
needed. Cluster commands (cluster init, cluster status, …) keep the
kubeconfig.

Tip: shpyrd cluster token --context <ctx> prints the admin token.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			if wsURL == "" {
				// Try to guess from --context if available.
				if g.kubeCtx != "" {
					return errors.New("--url is required: give the dashboard URL, e.g. https://shpyrd.oci.shpyrd.io")
				}
				return errors.New("--url is required: the workspace URL, e.g. https://acme.shpyrd.app")
			}
			norm, err := normaliseURL(wsURL)
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("--token is required; the browser device flow is not yet implemented.\nRun `shpyrd cluster token --context <ctx>` to get the admin token and pass it here.")
			}
			// Verify the token works before saving.
			if err := verifyToken(ctx, norm, token); err != nil {
				return fmt.Errorf("login failed: %w", err)
			}
			whoAmI := whoAmI(ctx, norm, token)
			s := loadSessions()
			s.Sessions[norm] = &loginSession{URL: norm, Token: token, SavedAt: time.Now(), WhoAmI: whoAmI}
			if err := s.save(); err != nil {
				return fmt.Errorf("save session: %w", err)
			}
			if whoAmI != "" {
				fmt.Fprintf(out, "Signed in to %s as %s\n", norm, whoAmI)
			} else {
				fmt.Fprintf(out, "Signed in to %s\n", norm)
			}
			fmt.Fprintln(out, "Project commands work without a kubeconfig now.")
			return nil
		},
	}
	cmd.Flags().StringVar(&wsURL, "url", os.Getenv("SHPYRD_URL"), "workspace URL (or SHPYRD_URL)")
	cmd.Flags().StringVar(&token, "token", os.Getenv("SHPYRD_TOKEN"), "API token (or SHPYRD_TOKEN; the admin token works: `shpyrd cluster token`)")
	return cmd
}

func verifyToken(ctx context.Context, wsURL, token string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", wsURL+"/api/healthz", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", wsURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error %d", resp.StatusCode)
	}
	return nil
}

func whoAmI(ctx context.Context, wsURL, token string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", wsURL+"/api/me", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	var me struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return ""
	}
	return me.Email
}

// newLogoutCmd is `shpyrd logout`.
func newLogoutCmd(g *globalFlags) *cobra.Command {
	var wsURL string
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove saved credentials for a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			if wsURL == "" {
				return errors.New("--url is required")
			}
			norm, err := normaliseURL(wsURL)
			if err != nil {
				return err
			}
			s := loadSessions()
			if _, ok := s.Sessions[norm]; !ok {
				return fmt.Errorf("not signed in to %s", norm)
			}
			delete(s.Sessions, norm)
			if err := s.save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Signed out of %s\n", norm)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsURL, "url", os.Getenv("SHPYRD_URL"), "workspace URL (or SHPYRD_URL)")
	_ = g
	return cmd
}

// newWhoAmICmd is `shpyrd whoami`.
func newWhoAmICmd(g *globalFlags) *cobra.Command {
	var wsURL string
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the current user of a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			if wsURL == "" {
				return errors.New("--url is required")
			}
			norm, err := normaliseURL(wsURL)
			if err != nil {
				return err
			}
			s := loadSessions()
			sess, ok := s.Sessions[norm]
			if !ok {
				return fmt.Errorf("not signed in to %s; run `shpyrd login --url %s`", norm, norm)
			}
			me := whoAmI(ctx, norm, sess.Token)
			if me == "" {
				me = "(token; re-run `shpyrd login` to refresh)"
			}
			fmt.Fprintln(cmd.OutOrStdout(), me)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsURL, "url", os.Getenv("SHPYRD_URL"), "workspace URL (or SHPYRD_URL)")
	_ = g
	return cmd
}
