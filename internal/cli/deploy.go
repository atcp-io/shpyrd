package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

func newDeployCmd(g *globalFlags) *cobra.Command {
	var (
		appName     string
		gitURL      string
		gitRef      string
		subPath     string
		image       string
		noWait      bool
		workingTree bool
		dockerfile  string
	)
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Build and release the current directory to a project",
		Long: `Deploy to a project.

By default the committed tree of the current directory (git HEAD, or the
whole directory outside a repository) is archived, uploaded to the cluster
and built in-cluster: with the Dockerfile when the directory has one,
otherwise with buildpacks. The resulting image is rolled out and exposed
at https://<app>.<domain>.

  shpyrd deploy --project myapp               archive the committed tree and build in-cluster
  shpyrd deploy --working-tree                archive the directory as is, uncommitted changes included
  shpyrd deploy --git https://github.com/o/r  build from a Git URL; new commits rebuild automatically (buildpacks)
  shpyrd deploy --git ... --dockerfile Dockerfile   build the repository's Dockerfile
  shpyrd deploy --image ghcr.io/o/r:tag       run a prebuilt image (no build)

The build strategy can be pinned in shpyrd.yaml (build.strategy: buildpacks
or dockerfile, plus build.dockerfile, build.target and build.env).

The project is taken from --project or from shpyrd.yaml (project: <name>).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			project, err := loadProjectConfig()
			if err != nil {
				return err
			}
			if gitURL != "" && image != "" {
				return errors.New("--git and --image are mutually exclusive")
			}
			ac, err := newAppClient(g, out)
			if err != nil {
				return err
			}
			before, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}

			// Build strategy: shpyrd.yaml wins, then --dockerfile, then the
			// presence of a Dockerfile in the deployed directory.
			if dockerfile != "" {
				if project == nil {
					project = &projectConfig{}
				}
				if project.Build == nil {
					project.Build = &projectBuild{}
				}
				project.Build.Strategy = shpyrdv1.StrategyDockerfile
				if dockerfile != "auto" {
					project.Build.Dockerfile = dockerfile
				}
			}
			if image == "" && gitURL == "" {
				project = detectDockerfile(project, subPath)
			}
			var mutate func(*shpyrdv1.App) error
			var note string
			switch {
			case image != "":
				fmt.Fprintf(out, "==> Deploying prebuilt image %s to %s\n", image, name)
				note = "Deploy image " + image
				mutate = func(a *shpyrdv1.App) error {
					a.Spec.Image = image
					return nil
				}
			case gitURL != "":
				ref := firstNonEmpty(gitRef, "main")
				fmt.Fprintf(out, "==> Deploying %s @ %s to %s\n", gitURL, ref, name)
				mutate = func(a *shpyrdv1.App) error {
					a.Spec.Image = ""
					a.Spec.Source = &shpyrdv1.Source{Git: &shpyrdv1.GitSource{URL: gitURL, Revision: ref}, SubPath: subPath}
					return nil
				}
			default:
				archive, ref, err := archiveSource(out, workingTree)
				if err != nil {
					return err
				}
				if b := project.Build; b != nil && b.Strategy == shpyrdv1.StrategyDockerfile {
					fmt.Fprintf(out, "==> Building with Dockerfile (%s)\n", firstNonEmpty(b.Dockerfile, "Dockerfile"))
				} else {
					fmt.Fprintln(out, "==> Building with buildpacks")
				}
				fmt.Fprintf(out, "==> Uploading source (%s)\n", humanBytes(len(archive)))
				info, err := ac.uploadSource(ctx, archive)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "    archive %s\n", info.SHA256[:12])
				mutate = func(a *shpyrdv1.App) error {
					a.Spec.Image = ""
					a.Spec.Source = &shpyrdv1.Source{
						Blob:    &shpyrdv1.BlobSource{URL: info.URL, SHA256: info.SHA256, Ref: ref},
						SubPath: subPath,
					}
					return nil
				}
			}

			app, err := ac.updateApp(ctx, name, func(a *shpyrdv1.App) error {
				if err := mutate(a); err != nil {
					return err
				}
				if err := project.applyTo(a); err != nil {
					return err
				}
				if note != "" {
					if a.Annotations == nil {
						a.Annotations = map[string]string{}
					}
					a.Annotations[shpyrdv1.AnnotationReleaseNote] = note
				}
				return nil
			})
			if err != nil {
				return err
			}
			ac.audit(ctx, name, "deploy", name, firstNonEmpty(note, "source deploy"))
			if noWait {
				fmt.Fprintln(out, "Deploy requested. Follow with `shpyrd projects info", name+"`.")
				return nil
			}

			specChanged := app.Generation != before.Generation
			// A build happens only when what is built changed: the same
			// archive with new processes or mounts is released as it is.
			sourceChanged := before.Status.LatestBuild == "" || !sameSource(before.Spec.Source, app.Spec.Source) || before.Spec.Build == nil != (app.Spec.Build == nil) || (before.Spec.Build != nil && app.Spec.Build != nil && !reflect.DeepEqual(*before.Spec.Build, *app.Spec.Build))
			switch {
			case app.HasSource() && app.Spec.Image == "" && sourceChanged:
				fmt.Fprintln(out, "==> Building")
				build, err := ac.waitForBuild(ctx, name, before.Status.LatestBuild, 3*time.Minute)
				if err != nil {
					return err
				}
				if err := ac.followBuild(ctx, app.Namespace, build); err != nil {
					return fmt.Errorf("build failed: %w", err)
				}
			case specChanged:
				fmt.Fprintln(out, "    source unchanged since the last build; releasing the configuration change")
			default:
				fmt.Fprintln(out, "    no changes since the last deploy")
			}

			fmt.Fprintln(out, "==> Releasing")
			final, err := ac.waitRunning(ctx, name, app.Generation, 10*time.Minute)
			if err != nil {
				return err
			}
			if rel := final.CurrentRelease(); rel != nil {
				fmt.Fprintf(out, "\nReleased v%d: %s\n", rel.Number, rel.Description)
			}
			if final.Status.URL != "" {
				fmt.Fprintf(out, "%s\n", final.Status.URL)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "project", "", "project name (default from shpyrd.yaml)")
	cmd.Flags().StringVarP(&appName, "app", "a", "", "alias of --project")
	_ = cmd.Flags().MarkHidden("app")
	cmd.Flags().StringVar(&gitURL, "git", "", "build from this Git repository instead of the local checkout")
	cmd.Flags().StringVar(&gitRef, "ref", "", "Git branch, tag or commit for --git (default main)")
	cmd.Flags().StringVar(&subPath, "path", "", "directory inside the source that holds the code")
	cmd.Flags().StringVar(&image, "image", "", "deploy a prebuilt image instead of building")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return immediately instead of following the build and rollout")
	cmd.Flags().BoolVar(&workingTree, "working-tree", false, "archive the directory as it is on disk instead of the committed HEAD")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "", "build with this Dockerfile (path relative to the deployed directory) instead of buildpacks")
	cmd.Flags().Lookup("dockerfile").NoOptDefVal = "auto"
	return cmd
}

// detectDockerfile picks the dockerfile strategy for local deploys when the
// deployed directory has a Dockerfile and shpyrd.yaml does not pin a
// strategy. Buildpacks stay the default otherwise.
func detectDockerfile(project *projectConfig, subPath string) *projectConfig {
	if project != nil && project.Build != nil && project.Build.Strategy != "" {
		return project
	}
	if project == nil {
		project = &projectConfig{}
	}
	if project.Build == nil {
		project.Build = &projectBuild{}
	}
	file := firstNonEmpty(project.Build.Dockerfile, "Dockerfile")
	if _, err := os.Stat(filepath.Join(subPath, file)); err == nil {
		project.Build.Strategy = shpyrdv1.StrategyDockerfile
	} else {
		project.Build.Strategy = shpyrdv1.StrategyBuildpacks
	}
	return project
}

// archiveSource returns a tar.gz of the current directory and a reference
// for the release description. Inside a Git repository the committed HEAD
// tree of the current directory is used unless workingTree is set (or the
// directory has no tracked files); elsewhere the directory is tarred.
func archiveSource(out io.Writer, workingTree bool) ([]byte, string, error) {
	// --show-prefix is the current directory relative to the repository root
	// ("" at the root, "sub/dir/" below). `git archive HEAD` run inside a
	// subdirectory archives just that subtree with paths relative to it.
	prefix, err := gitOutput("rev-parse", "--show-prefix")
	if err != nil {
		fmt.Fprintln(out, "==> Archiving current directory (not a git repository)")
		data, err := tarDirectory(".")
		return data, "", err
	}
	commit, _ := gitOutput("rev-parse", "--short=12", "HEAD")
	if tracked, _ := gitOutput("ls-files", "--", "."); tracked == "" && !workingTree {
		fmt.Fprintln(out, "    nothing here is committed yet; archiving the working tree instead")
		workingTree = true
	}
	if workingTree {
		fmt.Fprintf(out, "==> Archiving working tree (%s)\n", firstNonEmpty(commit, "uncommitted"))
		data, err := tarDirectory(".")
		if commit != "" {
			commit += "-dirty"
		}
		return data, commit, err
	}
	if dirty, _ := gitOutput("status", "--porcelain", "--", "."); dirty != "" {
		fmt.Fprintln(out, "    warning: uncommitted changes are not included (use --working-tree to deploy them)")
	}
	what := "HEAD"
	if p := strings.TrimSuffix(prefix, "/"); p != "" {
		what = "HEAD:" + p
	}
	fmt.Fprintf(out, "==> Archiving %s (%s)\n", what, commit)
	cmd := exec.Command("git", "archive", "--format=tar.gz", "HEAD")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("git archive: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return data, commit, nil
}

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = io.Discard
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// tarDirectory archives dir (excluding .git and node_modules) as tar.gz.
func tarDirectory(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := filepath.Base(rel)
		if info.IsDir() && (base == ".git" || base == "node_modules") {
			return filepath.SkipDir
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// sameSource reports whether two sources build the same thing: the same
// archive (by digest, else URL), the same Git URL and revision, the same
// directory.
func sameSource(a, b *shpyrdv1.Source) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.SubPath != b.SubPath {
		return false
	}
	switch {
	case a.Blob != nil && b.Blob != nil:
		if a.Blob.SHA256 != "" && b.Blob.SHA256 != "" {
			return a.Blob.SHA256 == b.Blob.SHA256
		}
		return a.Blob.URL == b.Blob.URL
	case a.Git != nil && b.Git != nil:
		return a.Git.URL == b.Git.URL && a.Git.Revision == b.Git.Revision
	}
	return false
}
