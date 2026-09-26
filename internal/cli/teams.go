package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/api"
	"shpyrd/pkg/audit"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
)

// Teams and project roles (RFC-0008) live in the control-plane store
// (RFC-0033); the CLI talks to the server's API for them, as the dashboard
// does, so the rules and the audit trail are the same.

func newTeamsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "teams",
		Aliases: []string{"team"},
		Short:   "Groups of users that projects grant roles to",
		Long: `Teams group users (by email, or by a group of your identity provider) so
projects can grant a role to many people at once. A team may also carry a
platform role: platform-admin (everything, cluster pages, extensions) or
platform-viewer (read-only everywhere).

Until the first team or member exists, every signed-in user is a platform
admin; creating one starts enforcing roles, so put yourself in an admin team
first:

  shpyrd teams create platform --platform-role platform-admin --member you@example.com
  shpyrd teams create web --member ada@example.com --group engineering
  shpyrd members add shop --team web --role developer`,
	}
	cmd.AddCommand(newTeamsCreateCmd(g), newTeamsListCmd(g), newTeamsMembersCmd(g, true), newTeamsMembersCmd(g, false), newTeamsDeleteCmd(g))
	return cmd
}

// teamsAPI is the CLI's view of the teams and members routes.
type teamsAPI struct {
	k *kube.Client
}

func newTeamsAPI(g *globalFlags) (*teamsAPI, error) {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
	if err != nil {
		return nil, err
	}
	return &teamsAPI{k: k}, nil
}

func (t *teamsAPI) call(ctx context.Context, method, path string, body any, out any) error {
	var raw []byte
	var contentType string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		raw, contentType = b, "application/json"
	}
	resp, err := serverRequest(ctx, t.k, method, path, raw, contentType)
	if err != nil {
		return err
	}
	if out != nil && len(resp) > 0 {
		if err := json.Unmarshal(resp, out); err != nil {
			return fmt.Errorf("unexpected response: %s", truncate(string(resp), 200))
		}
	}
	return nil
}

func (t *teamsAPI) teams(ctx context.Context) ([]api.TeamView, error) {
	var out []api.TeamView
	err := t.call(ctx, "GET", "api/teams", nil, &out)
	return out, err
}

func (t *teamsAPI) team(ctx context.Context, name string) (*api.TeamView, error) {
	teams, err := t.teams(ctx)
	if err != nil {
		return nil, err
	}
	for i := range teams {
		if teams[i].Name == name {
			return &teams[i], nil
		}
	}
	return nil, fmt.Errorf("team %q not found (see `shpyrd teams list`)", name)
}

func (t *teamsAPI) put(ctx context.Context, req api.TeamRequest) (*api.TeamView, error) {
	var out api.TeamView
	err := t.call(ctx, "PUT", "api/teams/"+url.PathEscape(req.Name), req, &out)
	return &out, err
}

func (t *teamsAPI) members(ctx context.Context, project string) ([]api.MemberView, error) {
	var out []api.MemberView
	path := "api/workspace/grants"
	if project != "" {
		path = "api/projects/" + url.PathEscape(project) + "/members"
	}
	err := t.call(ctx, "GET", path, nil, &out)
	return out, err
}

func newTeamsCreateCmd(g *globalFlags) *cobra.Command {
	var (
		members  []string
		groups   []string
		platform string
		desc     string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a team (or update its members, groups and platform role)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name := args[0]
			emails, err := normalizeEmails(members)
			if err != nil {
				return err
			}
			switch platform {
			case "", shpyrdv1.RolePlatformAdmin, shpyrdv1.RolePlatformViewer:
			default:
				return errors.New("--platform-role must be platform-admin or platform-viewer")
			}
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			before, err := t.teams(ctx)
			if err != nil {
				return err
			}
			existed := false
			for _, e := range before {
				if e.Name == name {
					existed = true
				}
			}
			team, err := t.put(ctx, api.TeamRequest{Name: name, Description: desc, Members: emails, Groups: groups, PlatformRole: platform})
			if err != nil {
				return err
			}
			if existed {
				fmt.Fprintf(cmd.OutOrStdout(), "Updated team %s%s\n", name, describeTeam(team))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created team %s%s\n", name, describeTeam(team))
			if len(before) == 0 {
				warnFirstEnforcement(cmd, t, ctx)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&members, "member", nil, "user emails (repeat or comma separate)")
	cmd.Flags().StringSliceVar(&groups, "group", nil, "identity provider group names whose users join the team")
	cmd.Flags().StringVar(&platform, "platform-role", "", "platform-admin or platform-viewer")
	cmd.Flags().StringVar(&desc, "description", "", "what the team is")
	return cmd
}

// warnFirstEnforcement tells the operator that roles are now enforced: the
// first team or grant switches the bootstrap mode off.
func warnFirstEnforcement(cmd *cobra.Command, t *teamsAPI, ctx context.Context) {
	grants, err := t.members(ctx, "")
	if err != nil || len(grants) > 0 {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Note: this is the first team. From now on roles are enforced: users without a team or membership see nothing in the dashboard (the admin token keeps full access).")
}

func describeTeam(t *api.TeamView) string {
	var parts []string
	if len(t.Members) > 0 {
		parts = append(parts, "members: "+strings.Join(t.Members, ", "))
	}
	if len(t.Groups) > 0 {
		parts = append(parts, "groups: "+strings.Join(t.Groups, ", "))
	}
	if t.PlatformRole != "" {
		parts = append(parts, "platform role: "+t.PlatformRole)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func newTeamsListCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List teams",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			teams, err := t.teams(ctx)
			if err != nil {
				return err
			}
			if len(teams) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No teams: every signed-in user is a platform admin. Create one with `shpyrd teams create platform --platform-role platform-admin --member you@example.com`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMEMBERS\tGROUPS\tPLATFORM ROLE")
			for _, team := range teams {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", team.Name, firstNonEmpty(strings.Join(team.Members, ", "), "-"), firstNonEmpty(strings.Join(team.Groups, ", "), "-"), firstNonEmpty(team.PlatformRole, "-"))
			}
			return tw.Flush()
		},
	}
}

// newTeamsMembersCmd builds `teams add` (add=true) or `teams remove`.
func newTeamsMembersCmd(g *globalFlags, add bool) *cobra.Command {
	var groups []string
	use, short := "add <team> <email...>", "Add users (or --group groups) to a team"
	if !add {
		use, short = "remove <team> <email...>", "Remove users (or --group groups) from a team"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			emails, err := normalizeEmails(args[1:])
			if err != nil {
				return err
			}
			if len(emails) == 0 && len(groups) == 0 {
				return errors.New("give emails or --group names")
			}
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			team, err := t.team(ctx, args[0])
			if err != nil {
				return err
			}
			req := api.TeamRequest{Name: team.Name, Description: team.Description, Members: team.Members, Groups: team.Groups, PlatformRole: team.PlatformRole}
			if add {
				req.Members = mergeStrings(req.Members, emails)
				req.Groups = mergeStrings(req.Groups, groups)
			} else {
				req.Members = removeStrings(req.Members, emails)
				req.Groups = removeStrings(req.Groups, groups)
			}
			updated, err := t.put(ctx, req)
			if err != nil {
				return err
			}
			verb := "Added"
			if !add {
				verb = "Removed"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s in team %s; members now: %s\n", verb, strings.Join(append(emails, groups...), ", "), updated.Name, firstNonEmpty(strings.Join(updated.Members, ", "), "-"))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&groups, "group", nil, "identity provider group names")
	return cmd
}

func newTeamsDeleteCmd(g *globalFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete <team>",
		Aliases: []string{"rm"},
		Short:   "Delete a team and the project roles granted to it",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			if !yes {
				return fmt.Errorf("this deletes team %q and every project role granted to it; re-run with --yes", args[0])
			}
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			if err := t.call(ctx, "DELETE", "api/teams/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted team %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm")
	return cmd
}

// ---- project members -------------------------------------------------------

func newMembersCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "members",
		Aliases: []string{"member"},
		Short:   "Who has which role on a project",
		Long: `Project roles (RFC-0008): user (opens the app), viewer (read), developer
(deploy, scale, config vars, shells) and admin (everything, including members
and destroy). A role is granted to a user by email or to a team.

  shpyrd members add shop --user ada@example.com --role developer
  shpyrd members add shop --team web --role developer
  shpyrd members list shop
  shpyrd members remove shop --user ada@example.com`,
	}
	cmd.AddCommand(newMembersAddCmd(g), newMembersListCmd(g), newMembersRemoveCmd(g))
	return cmd
}

func newMembersAddCmd(g *globalFlags) *cobra.Command {
	var user, team, role string
	cmd := &cobra.Command{
		Use:   "add <project> (--user <email> | --team <name>) --role user|viewer|developer|admin",
		Short: "Grant a role on a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			project := args[0]
			switch role {
			case shpyrdv1.RoleUser, shpyrdv1.RoleViewer, shpyrdv1.RoleDeveloper, shpyrdv1.RoleAdmin:
			default:
				return errors.New("--role must be user, viewer, developer or admin")
			}
			if (user == "") == (team == "") {
				return errors.New("give exactly one of --user or --team")
			}
			if user != "" {
				emails, err := normalizeEmails([]string{user})
				if err != nil {
					return err
				}
				user = emails[0]
			}
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			var out api.MemberView
			if err := t.call(ctx, "POST", "api/projects/"+url.PathEscape(project)+"/members", api.MemberRequest{Role: role, User: user, Team: team}, &out); err != nil {
				return err
			}
			who := user
			if team != "" {
				who = "team " + team
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s on project %s\n", who, role, project)
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user email")
	cmd.Flags().StringVar(&team, "team", "", "team name")
	cmd.Flags().StringVar(&role, "role", "", "user (opens the app), viewer, developer or admin (required)")
	return cmd
}

func newMembersListCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list [project]",
		Aliases: []string{"ls"},
		Short:   "List the roles granted on a project (all projects without an argument)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			project := ""
			if len(args) == 1 {
				project = args[0]
			}
			rows, err := t.members(ctx, project)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No project roles granted. Add one with `shpyrd members add <project> --user <email> --role developer`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tROLE\tUSER\tTEAM")
			for _, m := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Project, m.Role, firstNonEmpty(m.User, "-"), firstNonEmpty(m.Team, "-"))
			}
			return tw.Flush()
		},
	}
}

func newMembersRemoveCmd(g *globalFlags) *cobra.Command {
	var user, team string
	cmd := &cobra.Command{
		Use:     "remove <project> (--user <email> | --team <name>)",
		Aliases: []string{"rm"},
		Short:   "Remove every role of a user or team on a project",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			if (user == "") == (team == "") {
				return errors.New("give exactly one of --user or --team")
			}
			user = strings.ToLower(strings.TrimSpace(user))
			t, err := newTeamsAPI(g)
			if err != nil {
				return err
			}
			rows, err := t.members(ctx, args[0])
			if err != nil {
				return err
			}
			removed := 0
			for _, m := range rows {
				if (user != "" && m.User == user) || (team != "" && m.Team == team) {
					if err := t.call(ctx, "DELETE", "api/projects/"+url.PathEscape(args[0])+"/members/"+url.PathEscape(m.Name), nil, nil); err != nil {
						return err
					}
					removed++
				}
			}
			if removed == 0 {
				return fmt.Errorf("no roles of %s on project %s", firstNonEmpty(user, "team "+team), args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %d role(s) of %s on project %s\n", removed, firstNonEmpty(user, "team "+team), args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user email")
	cmd.Flags().StringVar(&team, "team", "", "team name")
	return cmd
}

func normalizeEmails(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		for _, part := range strings.Split(e, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if !strings.Contains(part, "@") {
				return nil, fmt.Errorf("%q is not an email address", part)
			}
			if !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out, nil
}

func mergeStrings(have, add []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(have)+len(add))
	for _, s := range append(append([]string{}, have...), add...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func removeStrings(have, drop []string) []string {
	gone := map[string]bool{}
	for _, s := range drop {
		gone[s] = true
	}
	out := make([]string, 0, len(have))
	for _, s := range have {
		if !gone[s] {
			out = append(out, s)
		}
	}
	return out
}

// audit records a CLI action on a project (best effort).
func (a *appClient) audit(ctx context.Context, project, action, target, detail string) {
	entry := audit.Entry{Actor: audit.LocalActor(), Action: action, Target: target, Detail: detail, Via: "cli"}
	_ = audit.Record(ctx, a.k.Kube, audit.AppRef(project), entry)
}

// auditCluster records a cluster-level CLI action.
func (a *appClient) auditCluster(ctx context.Context, action, target, detail string) {
	entry := audit.Entry{Actor: audit.LocalActor(), Action: action, Target: target, Detail: detail, Via: "cli"}
	_ = audit.Record(ctx, a.k.Kube, audit.ClusterRef(install.DefaultSystemNamespace), entry)
}
