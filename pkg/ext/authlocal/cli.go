package authlocal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/install"
	"github.com/shpyrd-io/shpyrd/pkg/kube"
)

func newUsersCmd(g ext.CLIGlobals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage the accounts people sign in to the dashboard with (extension auth-local)",
		Long: `Local accounts live in the cluster (Dex, extension auth-local) and sign in
to the dashboard with email and password. Until roles arrive (RFC-0008),
every account is an administrator.

  shpyrd users add ada@example.com --name "Ada Lovelace"   # prompts for the password
  shpyrd users list
  shpyrd users passwd ada@example.com
  shpyrd users rm ada@example.com`,
	}
	cmd.AddCommand(newUsersAddCmd(g), newUsersListCmd(g), newUsersPasswdCmd(g), newUsersRmCmd(g))
	return cmd
}

func storeFor(g ext.CLIGlobals) (*Store, error) {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.Kubeconfig(), Context: g.Context()})
	if err != nil {
		return nil, err
	}
	return &Store{Dynamic: k.Dynamic, Namespace: install.DefaultSystemNamespace}, nil
}

func cliContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx
}

func newUsersAddCmd(g ext.CLIGlobals) *cobra.Command {
	var (
		name     string
		password string
	)
	cmd := &cobra.Command{
		Use:   "add <email>",
		Short: "Create an account (prompts for the password unless --password is given)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := storeFor(g)
			if err != nil {
				return err
			}
			pw, err := passwordOrPrompt(cmd, password, true)
			if err != nil {
				return err
			}
			u, err := st.Create(cliContext(), args[0], name, pw)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created user %s (%s). Sign in at the dashboard with \"Email and password\".\n", u.Email, u.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "display name (default: the part before @)")
	cmd.Flags().StringVar(&password, "password", "", "password (prompted when omitted; prefer the prompt so it stays out of shell history)")
	return cmd
}

func newUsersListCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := storeFor(g)
			if err != nil {
				return err
			}
			users, err := st.List(cliContext())
			if err != nil {
				return err
			}
			if len(users) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No accounts yet. Create one with `shpyrd users add <email>`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "EMAIL\tNAME\tCREATED")
			for _, u := range users {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Email, u.Name, u.CreatedAt.Local().Format(time.DateTime))
			}
			return tw.Flush()
		},
	}
}

func newUsersPasswdCmd(g ext.CLIGlobals) *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "passwd <email>",
		Short: "Change an account's password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := storeFor(g)
			if err != nil {
				return err
			}
			pw, err := passwordOrPrompt(cmd, password, true)
			if err != nil {
				return err
			}
			if err := st.SetPassword(cliContext(), args[0], pw); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Password of %s changed.\n", strings.ToLower(args[0]))
			return nil
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "new password (prompted when omitted)")
	return cmd
}

func newUsersRmCmd(g ext.CLIGlobals) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <email>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an account",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := storeFor(g)
			if err != nil {
				return err
			}
			if err := st.Delete(cliContext(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted user %s. Existing dashboard sessions end when they expire.\n", strings.ToLower(args[0]))
			return nil
		},
	}
}

// passwordOrPrompt returns the flag value or asks on the terminal (twice
// when confirm is set), never echoing.
func passwordOrPrompt(cmd *cobra.Command, flag string, confirm bool) (string, error) {
	if flag != "" {
		return flag, CheckPassword(flag)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal to prompt for the password; pass --password")
	}
	read := func(prompt string) (string, error) {
		fmt.Fprint(cmd.ErrOrStderr(), prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		return string(b), err
	}
	pw, err := read("Password: ")
	if err != nil {
		return "", err
	}
	if err := CheckPassword(pw); err != nil {
		return "", err
	}
	if confirm {
		again, err := read("Repeat password: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("passwords do not match")
		}
	}
	return pw, nil
}
