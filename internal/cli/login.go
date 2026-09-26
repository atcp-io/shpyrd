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
	"text/tabwriter"
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
	cmd.Flags().StringVar(&token, "token", os.Getenv("SHPYRD_TOKEN"), "API token (or SHPYRD_TOKEN): a personal token from `shpyrd tokens create` or the admin token from `shpyrd cluster token`")
	return cmd
}

// verifyToken checks the token is accepted by the workspace: /api/me answers
// 401 for an unknown, expired or revoked token and 200 for a live one.
func verifyToken(ctx context.Context, wsURL, token string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", wsURL+"/api/me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", wsURL, err)
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("the workspace rejected this token (expired, revoked or mistyped)")
	case resp.StatusCode >= 500:
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
			if err := verifyToken(ctx, norm, sess.Token); err != nil {
				return fmt.Errorf("%s: %w; run `shpyrd login --url %s --token <token>` again", norm, err, norm)
			}
			me := whoAmI(ctx, norm, sess.Token)
			if me == "" {
				// The admin token has no person behind it.
				me = "admin token"
			}
			fmt.Fprintln(cmd.OutOrStdout(), me)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsURL, "url", os.Getenv("SHPYRD_URL"), "workspace URL (or SHPYRD_URL)")
	_ = g
	return cmd
}

// TokenView is a token as shown in the CLI (mirrors api.TokenView).
type TokenView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	PlatformRole string            `json:"platformRole,omitempty"`
	ProjectRoles map[string]string `json:"projectRoles,omitempty"`
	ExpiresAt    *time.Time        `json:"expiresAt,omitempty"`
	LastUsedAt   *time.Time        `json:"lastUsedAt,omitempty"`
}

// TokenCreateView is the create response.
type TokenCreateView struct {
	TokenView
	Token string `json:"token"`
}

// newTokensCmd is `shpyrd tokens`.
func newTokensCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Personal API tokens (scoped credentials for CI and integrations)",
		Long: `API tokens let scripts and CI pipelines authenticate without the admin token.
They carry only the roles you give them and expire when you say.

  shpyrd tokens create ci --platform-role platform-viewer --expires 90d
  shpyrd tokens create ci --project shop --role developer --expires 30d
  shpyrd tokens list
  shpyrd tokens revoke <id>
  
The token value is shown once. Store it in SHPYRD_TOKEN for the CLI, or
pass it to shpyrd login --token.`,
	}

	var wsURL string

	// helper: serverRequest via the login session
	call := func(ctx context.Context, method, path string, body []byte) ([]byte, error) {
		sessions := loadSessions()
		url := os.Getenv("SHPYRD_URL")
		tok := os.Getenv("SHPYRD_TOKEN")
		for _, sess := range sessions.Sessions {
			if url == "" {
				url = sess.URL
			}
			if tok == "" {
				tok = sess.Token
			}
		}
		if wsURL != "" {
			url = wsURL
		}
		if url == "" {
			return nil, errors.New("--url is required or run `shpyrd login`")
		}
		if tok == "" {
			return nil, errors.New("not signed in: run `shpyrd login --url " + url + " --token <token>`")
		}
		return serverRequestDirect(ctx, url, tok, method, path, body, "application/json")
	}

	var (
		name         string
		platformRole string
		projectRole  string
		project      string
		expiresIn    string
	)
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a personal API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			body := map[string]interface{}{"name": args[0]}
			if platformRole != "" {
				body["platformRole"] = platformRole
			}
			if project != "" && projectRole != "" {
				body["projectRoles"] = map[string]string{project: projectRole}
			}
			if expiresIn != "" {
				body["expiresIn"] = expiresIn
			}
			raw, _ := json.Marshal(body)
			resp, err := call(ctx, "POST", "api/tokens", raw)
			if err != nil {
				return err
			}
			var tok TokenCreateView
			if err := json.Unmarshal(resp, &tok); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(resp), 200))
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, tok.Token)
			fmt.Fprintf(out, "Token %q created (id: %s). The value above is shown once; store it safely.\n", tok.Name, tok.ID)
			if tok.ExpiresAt != nil {
				fmt.Fprintf(out, "Expires: %s\n", tok.ExpiresAt.Local().Format("2006-01-02"))
			}
			return nil
		},
	}
	createCmd.Flags().StringVar(&platformRole, "platform-role", "", "platform-viewer or platform-admin")
	createCmd.Flags().StringVar(&project, "project", "", "project slug for a project-scoped role")
	createCmd.Flags().StringVar(&projectRole, "role", "", "user, viewer, developer or admin (with --project)")
	createCmd.Flags().StringVar(&expiresIn, "expires", "90d", "expiry: 30d, 90d, 365d, etc.")
	_ = name

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List your API tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			resp, err := call(ctx, "GET", "api/tokens", nil)
			if err != nil {
				return err
			}
			var tokens []TokenView
			if err := json.Unmarshal(resp, &tokens); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(resp), 200))
			}
			if len(tokens) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No tokens. Create one with `shpyrd tokens create <name>`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tID\tROLE\tEXPIRES\tLAST USED")
			for _, t := range tokens {
				role := t.PlatformRole
				for p, r := range t.ProjectRoles {
					role = p + "=" + r
					break
				}
				if role == "" {
					role = "project-scoped"
				}
				expires := "never"
				if t.ExpiresAt != nil {
					expires = t.ExpiresAt.Local().Format("2006-01-02")
				}
				used := "-"
				if t.LastUsedAt != nil {
					used = ago(*t.LastUsedAt)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.ID, role, expires, used)
			}
			return tw.Flush()
		},
	}

	revokeCmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API token immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			if _, err := call(ctx, "DELETE", "api/tokens/"+args[0], nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Token %s revoked.\n", args[0])
			return nil
		},
	}

	for _, c := range []*cobra.Command{createCmd, listCmd, revokeCmd} {
		c.Flags().StringVar(&wsURL, "url", os.Getenv("SHPYRD_URL"), "workspace URL (or SHPYRD_URL)")
	}
	cmd.AddCommand(createCmd, listCmd, revokeCmd)
	return cmd
}
