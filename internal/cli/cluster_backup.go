package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"shpyrd/pkg/api"
	"shpyrd/pkg/backup"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/store"
)

// Platform backups (RFC-0037): `cluster backup` runs one now, `cluster
// backups` lists what the target holds, `cluster backup key` prints the
// passphrase to keep elsewhere, `cluster restore` brings an archive back.

func newClusterBackupCmd(g *globalFlags) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the platform's state now (an encrypted archive to the configured target)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			raw, err := serverRequest(ctx, k, "POST", "api/cluster/backups", nil, "")
			if err != nil {
				return err
			}
			var started struct {
				Job string `json:"job"`
			}
			_ = json.Unmarshal(raw, &started)
			fmt.Fprintf(out, "Backup started (job %s).\n", started.Job)
			if !wait {
				fmt.Fprintln(out, "Follow it with `shpyrd cluster backups`.")
				return nil
			}
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
				info, err := fetchBackups(ctx, k)
				if err != nil {
					return err
				}
				for _, run := range info.Runs {
					if run.Name != started.Job {
						continue
					}
					switch run.Status {
					case "succeeded":
						if len(info.Archives) > 0 {
							a := info.Archives[0]
							fmt.Fprintf(out, "Done: %s (%s) in %s.\n", a.Name, humanBytes(int(a.Size)), info.Target)
						} else {
							fmt.Fprintln(out, "Done.")
						}
						return nil
					case "failed":
						return fmt.Errorf("backup failed: %s (logs: kubectl -n %s logs job/%s)", run.Message, install.DefaultSystemNamespace, run.Name)
					}
				}
			}
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the backup to finish")
	cmd.AddCommand(newClusterBackupKeyCmd(g))
	return cmd
}

func fetchBackups(ctx context.Context, k *kube.Client) (*api.BackupInfo, error) {
	raw, err := serverRequest(ctx, k, "GET", "api/cluster/backups", nil, "")
	if err != nil {
		return nil, err
	}
	var info api.BackupInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
	}
	return &info, nil
}

func newClusterBackupKeyCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "key",
		Short: "Print the passphrase that encrypts the backups (keep it outside the cluster: a restore needs it)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			sec, err := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace).Get(ctx, install.BackupKeySecretName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("no backup key: platform backups are not set up (`shpyrd cluster init --backup-target s3://…`)")
			}
			fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(sec.Data["passphrase"])))
			return nil
		},
	}
}

func newClusterBackupsCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "backups",
		Short: "List the platform backups in the target and the recent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			info, err := fetchBackups(ctx, k)
			if err != nil {
				return err
			}
			if !info.Enabled {
				fmt.Fprintln(out, "Platform backups are not set up. Give `shpyrd cluster init` a target:")
				fmt.Fprintln(out, "  --backup-target s3://bucket/prefix [--backup-credentials-file <name>-backups.env]")
				fmt.Fprintln(out, "contrib/aws/terraform/backups and contrib/oci/terraform/backups create the bucket.")
				return nil
			}
			how := "the platform's cloud identity"
			if info.AccessKey {
				how = "an access key"
			}
			fmt.Fprintf(out, "Target:    %s (%s)\n", info.Target, how)
			if info.Endpoint != "" {
				fmt.Fprintf(out, "Endpoint:  %s\n", info.Endpoint)
			}
			fmt.Fprintf(out, "Schedule:  %s UTC, keeping %d\n", info.Schedule, info.Keep)
			if info.LastSuccessful != nil {
				fmt.Fprintf(out, "Last good: %s\n", ago(*info.LastSuccessful))
			}
			if info.Error != "" {
				fmt.Fprintf(out, "\nCannot list the target: %s\n", info.Error)
			} else if len(info.Archives) == 0 {
				fmt.Fprintln(out, "\nNo backups yet. `shpyrd cluster backup` runs one now.")
			} else {
				fmt.Fprintln(out)
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ARCHIVE\tSIZE\tCREATED")
				for _, a := range info.Archives {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, humanBytes(int(a.Size)), ago(a.Modified))
				}
				tw.Flush()
			}
			if len(info.Runs) > 0 {
				fmt.Fprintln(out)
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "RUN\tSTATUS\tSTARTED\tMESSAGE")
				for i, r := range info.Runs {
					if i == 5 {
						break
					}
					started := ""
					if r.Started != nil {
						started = ago(*r.Started)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Status, started, truncate(r.Message, 60))
				}
				tw.Flush()
			}
			fmt.Fprintf(out, "\nRestore: shpyrd cluster restore --from %s/<archive> --passphrase-file <file>\n", info.Target)
			return nil
		},
	}
}

type restoreFlags struct {
	from            string
	file            string
	passphraseFile  string
	credentialsFile string
	endpoint        string
	region          string
	awsProfile      string
	projects        []string
	noSystem        bool
	overwrite       bool
	dryRun          bool
}

func newClusterRestoreCmd(g *globalFlags) *cobra.Command {
	var f restoreFlags
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a platform backup into this cluster (run `shpyrd cluster init` first)",
		Long: `Restore a platform backup into the current cluster.

The cluster must run the platform already (` + "`shpyrd cluster init`" + ` with this
infrastructure's settings); the restore brings back the platform's state:
sizes, global config vars, sign-in users, teams, members, and every project
(its config vars, resources, apps and sources; apps build again). Projects
that already exist are skipped unless --overwrite says to update them.

Not in a backup: what lives inside volumes and databases. Postgres archives
stay in the cluster's object storage; a restored database starts empty.

  shpyrd cluster restore --from s3://bucket/prefix --passphrase-file key.txt        # latest
  shpyrd cluster restore --from s3://bucket/prefix/dev-20260925-030000.tar.gz.age \
      --passphrase-file key.txt --project shop                                       # one project
  shpyrd cluster restore --file dev-20260925-030000.tar.gz.age --passphrase-file key.txt --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestore(signalContext(), cmd, g, &f)
		},
	}
	cmd.Flags().StringVar(&f.from, "from", "", "s3://bucket/prefix (the latest archive) or s3://bucket/prefix/<archive>")
	cmd.Flags().StringVar(&f.file, "file", "", "a downloaded archive instead of --from")
	cmd.Flags().StringVar(&f.passphraseFile, "passphrase-file", "", "file holding the passphrase (`shpyrd cluster backup key` on the cluster that made the backup); or SHPYRD_BACKUP_PASSPHRASE")
	cmd.Flags().StringVar(&f.credentialsFile, "credentials-file", "", "KEY=value file with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for the target (default: this cluster's backup target Secret, then the environment and ~/.aws/credentials)")
	cmd.Flags().StringVar(&f.endpoint, "endpoint", "", "S3 endpoint of the target (Oracle: https://<namespace>.compat.objectstorage.<region>.oraclecloud.com; default: this cluster's, then AWS)")
	cmd.Flags().StringVar(&f.region, "region", "", "region of the target")
	cmd.Flags().StringVar(&f.awsProfile, "aws-profile", os.Getenv("AWS_PROFILE"), "profile in ~/.aws/credentials when no keys are given")
	cmd.Flags().StringArrayVar(&f.projects, "project", nil, "restore only this project (repeatable)")
	cmd.Flags().BoolVar(&f.noSystem, "no-system", false, "skip sizes, globals, sign-in users, teams and members")
	cmd.Flags().BoolVar(&f.overwrite, "overwrite", false, "update objects that already exist (projects too)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "show what the archive holds and stop")
	return cmd
}

func runRestore(ctx context.Context, cmd *cobra.Command, g *globalFlags, f *restoreFlags) error {
	out := cmd.OutOrStdout()
	if (f.from == "") == (f.file == "") {
		return errors.New("give --from s3://… or --file <archive>")
	}
	passphrase := os.Getenv("SHPYRD_BACKUP_PASSPHRASE")
	if f.passphraseFile != "" {
		raw, err := os.ReadFile(f.passphraseFile)
		if err != nil {
			return fmt.Errorf("--passphrase-file: %w", err)
		}
		passphrase = strings.TrimSpace(string(raw))
	}
	if passphrase == "" {
		return errors.New("the passphrase is needed: --passphrase-file <file> (from `shpyrd cluster backup key` on the cluster that made the backup)")
	}
	k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
	if err != nil {
		return err
	}

	// The archive: a local file or the target.
	var encrypted io.Reader
	name := f.file
	if f.file != "" {
		raw, err := os.ReadFile(f.file)
		if err != nil {
			return err
		}
		encrypted = bytes.NewReader(raw)
	} else {
		target, archive, err := restoreTarget(ctx, k, f)
		if err != nil {
			return err
		}
		if archive == "" {
			entries, err := target.List(ctx)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				return fmt.Errorf("no archives in %s", f.from)
			}
			archive = entries[0].Name
		}
		fmt.Fprintf(out, "Downloading %s from s3://%s/%s…\n", archive, target.Bucket, target.Prefix)
		rc, err := target.Download(ctx, archive)
		if err != nil {
			return err
		}
		defer rc.Close()
		encrypted = rc
		name = archive
	}
	plain, err := backup.Decrypt(encrypted, passphrase)
	if err != nil {
		return fmt.Errorf("%s: %w (wrong passphrase?)", name, err)
	}
	a, err := backup.Read(plain)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	man := a.Manifest
	profile, vars := a.InstallRecord()
	fmt.Fprintf(out, "Archive %s: %s (profile %s, shpyrd %s), made %s\n", name, firstNonEmpty(man.Domain, vars[install.VarDomain], man.Cluster), profile, man.ShpyrdVersion, man.CreatedAt.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(out, "  %d objects, %d sources, projects: %s\n", man.Objects, man.Sources, strings.Join(man.Projects, ", "))

	info, err := install.ReadInstallInfo(ctx, k, "")
	if err != nil {
		return fmt.Errorf("this cluster does not run the platform yet: run `shpyrd cluster init --profile %s --domain %s …` first, then restore", firstNonEmpty(profile, "<profile>"), firstNonEmpty(vars[install.VarDomain], "<domain>"))
	}
	if info.Vars[install.VarDomain] != vars[install.VarDomain] && vars[install.VarDomain] != "" {
		fmt.Fprintf(out, "  note: this cluster's domain is %s; the archive's projects were served under %s\n", info.Vars[install.VarDomain], vars[install.VarDomain])
	}
	if f.dryRun {
		fmt.Fprintln(out, "\nFiles:")
		paths := a.Paths("")
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(out, "  %s (%s)\n", p, humanBytes(len(a.Files[p])))
		}
		return nil
	}

	r := &backup.Restorer{
		Dynamic: k.Dynamic, Archive: a,
		Projects: f.projects, System: !f.noSystem, Overwrite: f.overwrite,
		UploadSource: func(ctx context.Context, sha string, data []byte) error {
			_, err := serverRequest(ctx, k, "POST", "api/sources", data, "application/gzip")
			return err
		},
		ImportStore: func(ctx context.Context, dump *store.Dump, overwrite bool) error {
			body, err := json.Marshal(dump)
			if err != nil {
				return err
			}
			path := "api/workspace/import"
			if overwrite {
				path += "?overwrite=true"
			}
			_, err = serverRequest(ctx, k, "POST", path, body, "application/json")
			return err
		},
		Log: func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) },
	}
	fmt.Fprintln(out)
	res, err := r.Run(ctx)
	if res != nil {
		fmt.Fprintf(out, "\n%d created, %d updated, %d already there, %d sources uploaded.\n", res.Created, res.Updated, res.Skipped, res.Sources)
		for _, w := range res.Warnings {
			fmt.Fprintf(out, "  warning: %s\n", w)
		}
		if len(res.Projects) > 0 {
			fmt.Fprintf(out, "Projects %s build and start now: `shpyrd projects list`.\n", strings.Join(res.Projects, ", "))
		}
		if len(res.SkippedProjects) > 0 {
			fmt.Fprintf(out, "Skipped (already exist): %s.\n", strings.Join(res.SkippedProjects, ", "))
		}
	}
	return err
}

// restoreTarget builds the target for --from: its bucket and prefix from
// the URI, endpoint/region/credentials from the flags, else from the
// cluster's own backup target (the same bucket on a restore into place),
// else the environment. It returns the archive name when the URI names one.
func restoreTarget(ctx context.Context, k *kube.Client, f *restoreFlags) (*backup.Target, string, error) {
	bucket, prefix, err := backup.ParseURI(f.from)
	if err != nil {
		return nil, "", err
	}
	archive := ""
	if strings.HasSuffix(prefix, ".tar.gz.age") {
		if i := strings.LastIndex(prefix, "/"); i >= 0 {
			archive, prefix = prefix[i+1:], prefix[:i]
		} else {
			archive, prefix = prefix, ""
		}
	}
	t := &backup.Target{Bucket: bucket, Prefix: prefix, Endpoint: f.endpoint, Region: f.region, Profile: f.awsProfile}
	if f.credentialsFile != "" {
		creds, err := readVarsFileAny(f.credentialsFile)
		if err != nil {
			return nil, "", fmt.Errorf("--credentials-file: %w", err)
		}
		t.AccessKey, t.SecretKey = creds["AWS_ACCESS_KEY_ID"], creds["AWS_SECRET_ACCESS_KEY"]
		if t.Endpoint == "" {
			t.Endpoint = creds["SHPYRD_BACKUP_ENDPOINT"]
		}
		if t.Region == "" {
			t.Region = creds["SHPYRD_BACKUP_REGION"]
		}
	}
	if sec, err := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace).Get(ctx, install.BackupTargetSecretName, metav1.GetOptions{}); err == nil {
		if t.Endpoint == "" {
			t.Endpoint = string(sec.Data["SHPYRD_BACKUP_ENDPOINT"])
		}
		if t.Region == "" {
			t.Region = string(sec.Data["SHPYRD_BACKUP_REGION"])
		}
		if t.AccessKey == "" && f.credentialsFile == "" {
			t.AccessKey, t.SecretKey = string(sec.Data["AWS_ACCESS_KEY_ID"]), string(sec.Data["AWS_SECRET_ACCESS_KEY"])
		}
	}
	return t, archive, nil
}
