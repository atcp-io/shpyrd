package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/audit"
	"shpyrd/pkg/install"
)

// Teams and project members (RFC-0008) are cluster-scoped objects the CLI
// edits directly with the kubeconfig; the dashboard enforces the roles
// they define, and the controller mirrors them into Kubernetes RBAC.

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

func teamClient(g *globalFlags) (*appClient, error) {
	return newAppClient(g, nil)
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
			if !volumeNameRe.MatchString(name) {
				return errors.New("team names use lowercase letters, digits and dashes (max 40 chars)")
			}
			switch platform {
			case "", shpyrdv1.RolePlatformAdmin, shpyrdv1.RolePlatformViewer:
			default:
				return fmt.Errorf("--platform-role must be %s or %s", shpyrdv1.RolePlatformAdmin, shpyrdv1.RolePlatformViewer)
			}
			emails, err := normalizeEmails(members)
			if err != nil {
				return err
			}
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			team := &shpyrdv1.Team{ObjectMeta: metav1.ObjectMeta{Name: name}}
			err = ac.c.Get(ctx, types.NamespacedName{Name: name}, team)
			created := apierrors.IsNotFound(err)
			if err != nil && !created {
				return err
			}
			if created {
				team.Labels = map[string]string{shpyrdv1.LabelManagedBy: "shpyrd"}
				team.Spec = shpyrdv1.TeamSpec{Description: desc, Members: emails, Groups: groups, PlatformRole: platform}
				if err := warnFirstEnforcement(ctx, cmd, ac); err != nil {
					return err
				}
				if err := ac.c.Create(ctx, team); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Created team %s%s\n", name, describeTeam(team))
			} else {
				team.Spec.Members = mergeStrings(team.Spec.Members, emails)
				team.Spec.Groups = mergeStrings(team.Spec.Groups, groups)
				if platform != "" {
					team.Spec.PlatformRole = platform
				}
				if desc != "" {
					team.Spec.Description = desc
				}
				if err := ac.c.Update(ctx, team); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Updated team %s%s\n", name, describeTeam(team))
			}
			ac.auditCluster(ctx, "team.create", name, describeTeam(team))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&members, "member", nil, "user emails (repeat or comma separate)")
	cmd.Flags().StringSliceVar(&groups, "group", nil, "identity provider group names whose users join the team")
	cmd.Flags().StringVar(&platform, "platform-role", "", "platform-admin or platform-viewer")
	cmd.Flags().StringVar(&desc, "description", "", "what the team is")
	return cmd
}

// warnFirstEnforcement reminds the operator that the first membership
// object switches roles on for everyone.
func warnFirstEnforcement(ctx context.Context, cmd *cobra.Command, ac *appClient) error {
	var teams shpyrdv1.TeamList
	var members shpyrdv1.ProjectMemberList
	if err := ac.c.List(ctx, &teams); err != nil {
		return err
	}
	if err := ac.c.List(ctx, &members); err != nil {
		return err
	}
	if len(teams.Items) == 0 && len(members.Items) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Note: this is the first team. From now on roles are enforced: users without a team or membership see nothing in the dashboard (the admin token keeps full access).")
	}
	return nil
}

func describeTeam(t *shpyrdv1.Team) string {
	var parts []string
	if len(t.Spec.Members) > 0 {
		parts = append(parts, fmt.Sprintf("%d member(s)", len(t.Spec.Members)))
	}
	if len(t.Spec.Groups) > 0 {
		parts = append(parts, "groups "+strings.Join(t.Spec.Groups, ", "))
	}
	if t.Spec.PlatformRole != "" {
		parts = append(parts, t.Spec.PlatformRole)
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
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			var teams shpyrdv1.TeamList
			if err := ac.c.List(ctx, &teams); err != nil {
				return err
			}
			if len(teams.Items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No teams: every signed-in user is a platform admin. Create one with `shpyrd teams create platform --platform-role platform-admin --member you@example.com`.")
				return nil
			}
			sort.Slice(teams.Items, func(i, j int) bool { return teams.Items[i].Name < teams.Items[j].Name })
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMEMBERS\tGROUPS\tPLATFORM ROLE")
			for _, t := range teams.Items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, firstNonEmpty(strings.Join(t.Spec.Members, ", "), "-"), firstNonEmpty(strings.Join(t.Spec.Groups, ", "), "-"), firstNonEmpty(t.Spec.PlatformRole, "-"))
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
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			team := &shpyrdv1.Team{}
			if err := ac.c.Get(ctx, types.NamespacedName{Name: args[0]}, team); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("team %q not found (see `shpyrd teams list`)", args[0])
				}
				return err
			}
			if add {
				team.Spec.Members = mergeStrings(team.Spec.Members, emails)
				team.Spec.Groups = mergeStrings(team.Spec.Groups, groups)
			} else {
				team.Spec.Members = removeStrings(team.Spec.Members, emails)
				team.Spec.Groups = removeStrings(team.Spec.Groups, groups)
			}
			if err := ac.c.Update(ctx, team); err != nil {
				return err
			}
			verb := "Added"
			if !add {
				verb = "Removed"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s in team %s; members now: %s\n", verb, strings.Join(append(emails, groups...), ", "), team.Name, firstNonEmpty(strings.Join(team.Spec.Members, ", "), "-"))
			ac.auditCluster(ctx, "team.update", team.Name, verb+" "+strings.Join(append(emails, groups...), ", "))
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
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			team := &shpyrdv1.Team{ObjectMeta: metav1.ObjectMeta{Name: args[0]}}
			if err := ac.c.Delete(ctx, team); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("team %q not found", args[0])
				}
				return err
			}
			var members shpyrdv1.ProjectMemberList
			if err := ac.c.List(ctx, &members); err == nil {
				for i := range members.Items {
					if members.Items[i].Spec.Team == args[0] {
						_ = ac.c.Delete(ctx, &members.Items[i])
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted team %s\n", args[0])
			ac.auditCluster(ctx, "team.delete", args[0], "")
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
		Long: `Roles per project: viewer (see everything, change nothing), developer
(deploy, roll back, scale, resize, config vars, shell) and admin (also
members, resources, destroy). Grant them to users or teams.

  shpyrd members add shop --user ada@example.com --role developer
  shpyrd members add shop --team web --role viewer
  shpyrd members list shop
  shpyrd members remove shop --user ada@example.com`,
	}
	cmd.AddCommand(newMembersAddCmd(g), newMembersListCmd(g), newMembersRemoveCmd(g))
	return cmd
}

func memberName(project, role, user, team string) string {
	subject := "user-" + strings.NewReplacer("@", "-at-", ".", "-", "+", "-plus-", "_", "-").Replace(strings.ToLower(user))
	if team != "" {
		subject = "team-" + team
	}
	name := project + "-" + role + "-" + subject
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func newMembersAddCmd(g *globalFlags) *cobra.Command {
	var (
		user string
		team string
		role string
	)
	cmd := &cobra.Command{
		Use:   "add <project> (--user <email> | --team <name>) --role viewer|developer|admin",
		Short: "Grant a role on a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			project := args[0]
			switch role {
			case shpyrdv1.RoleViewer, shpyrdv1.RoleDeveloper, shpyrdv1.RoleAdmin:
			default:
				return errors.New("--role must be viewer, developer or admin")
			}
			if (user == "") == (team == "") {
				return errors.New("give exactly one of --user or --team")
			}
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			if _, err := ac.getApp(ctx, project); err != nil {
				return err
			}
			if user != "" {
				emails, err := normalizeEmails([]string{user})
				if err != nil {
					return err
				}
				user = emails[0]
			} else if err := ac.c.Get(ctx, types.NamespacedName{Name: team}, &shpyrdv1.Team{}); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("team %q not found (see `shpyrd teams list`)", team)
				}
				return err
			}
			if err := warnFirstEnforcement(ctx, cmd, ac); err != nil {
				return err
			}
			m := &shpyrdv1.ProjectMember{
				ObjectMeta: metav1.ObjectMeta{Name: memberName(project, role, user, team), Labels: map[string]string{shpyrdv1.LabelProject: project, shpyrdv1.LabelManagedBy: "shpyrd"}},
				Spec:       shpyrdv1.ProjectMemberSpec{Project: project, Role: role, User: user, Team: team},
			}
			if err := ac.c.Create(ctx, m); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return errors.New("this grant already exists")
				}
				return err
			}
			who := user
			if team != "" {
				who = "team " + team
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s on project %s\n", who, role, project)
			ac.audit(ctx, project, "member.add", who, role)
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user email")
	cmd.Flags().StringVar(&team, "team", "", "team name")
	cmd.Flags().StringVar(&role, "role", "", "viewer, developer or admin (required)")
	_ = cmd.MarkFlagRequired("role")
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
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			var list shpyrdv1.ProjectMemberList
			if err := ac.c.List(ctx, &list); err != nil {
				return err
			}
			var rows []shpyrdv1.ProjectMember
			for _, m := range list.Items {
				if len(args) == 0 || m.Spec.Project == args[0] {
					rows = append(rows, m)
				}
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No project roles granted. Add one with `shpyrd members add <project> --user <email> --role developer`.")
				return nil
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Spec.Project != rows[j].Spec.Project {
					return rows[i].Spec.Project < rows[j].Spec.Project
				}
				return rows[i].Name < rows[j].Name
			})
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tROLE\tUSER\tTEAM")
			for _, m := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Spec.Project, m.Spec.Role, firstNonEmpty(m.Spec.User, "-"), firstNonEmpty(m.Spec.Team, "-"))
			}
			return tw.Flush()
		},
	}
}

func newMembersRemoveCmd(g *globalFlags) *cobra.Command {
	var (
		user string
		team string
	)
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
			ac, err := teamClient(g)
			if err != nil {
				return err
			}
			var list shpyrdv1.ProjectMemberList
			if err := ac.c.List(ctx, &list); err != nil {
				return err
			}
			removed := 0
			for i := range list.Items {
				m := &list.Items[i]
				if m.Spec.Project != args[0] {
					continue
				}
				if (user != "" && strings.EqualFold(m.Spec.User, user)) || (team != "" && m.Spec.Team == team) {
					if err := ac.c.Delete(ctx, m); client.IgnoreNotFound(err) != nil {
						return err
					}
					removed++
				}
			}
			if removed == 0 {
				return fmt.Errorf("no role of %s on project %s", firstNonEmpty(user, "team "+team), args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %d role(s) of %s on project %s\n", removed, firstNonEmpty(user, "team "+team), args[0])
			ac.audit(ctx, args[0], "member.remove", firstNonEmpty(user, "team "+team), "")
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user email")
	cmd.Flags().StringVar(&team, "team", "", "team name")
	return cmd
}

// ---- helpers ----------------------------------------------------------------

func normalizeEmails(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		for _, e := range strings.Split(raw, ",") {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			if !strings.Contains(e, "@") {
				return nil, fmt.Errorf("%q is not an email address", e)
			}
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out, nil
}

func mergeStrings(have, add []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, have...), add...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func removeStrings(have, drop []string) []string {
	gone := map[string]bool{}
	for _, s := range drop {
		gone[strings.ToLower(s)] = true
	}
	var out []string
	for _, s := range have {
		if !gone[strings.ToLower(s)] {
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
