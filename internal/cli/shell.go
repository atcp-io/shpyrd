package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/api"
	"shpyrd/pkg/sizes"
)

// Buildpack images set up the language runtime (PATH, env) through the CNB
// launcher; a plain exec'd shell would miss it, so commands go through it
// when present.
const cnbLauncher = "/cnb/lifecycle/launcher"

// ---- shell ------------------------------------------------------------------

func newShellCmd(g *globalFlags) *cobra.Command {
	var (
		appName  string
		process  string
		instance string
	)
	cmd := &cobra.Command{
		Use:   "shell [-- command...]",
		Short: "Open a shell in a running instance",
		Long: `Attach an interactive shell to a running instance of the project (bash, or
sh when the image has no bash). Buildpack images get the same environment
as the running process. Pick the instance with --instance (web.2) or the
process type with --process; the default is the first web instance.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			pod, label, err := ac.pickInstance(ctx, app, process, instance)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Connecting to %s (%s)...\n", label, pod.Name)
			command := args
			if len(command) == 0 {
				command = []string{"bash"}
			}
			return ac.execInteractive(ctx, pod.Namespace, pod.Name, "app", command, len(args) == 0, stdinIsTerminal())
		},
	}
	cmd.Flags().SetInterspersed(false) // everything after the command belongs to it
	appFlag(cmd, &appName)
	cmd.Flags().StringVarP(&process, "process", "p", "", "process type (default web, else the first one)")
	cmd.Flags().StringVarP(&instance, "instance", "i", "", "instance name, e.g. web.2")
	return cmd
}

// pickInstance chooses a running pod of the project by process/instance.
// Without --process it takes web when the app has one (and reports when it
// is not running rather than silently attaching to another process type).
func (a *appClient) pickInstance(ctx context.Context, app *shpyrdv1.App, process, instance string) (*corev1.Pod, string, error) {
	if instance != "" {
		process = strings.SplitN(instance, ".", 2)[0]
	}
	if process == "" {
		if _, hasWeb := app.Spec.Processes["web"]; hasWeb || len(app.Spec.Processes) == 0 {
			process = "web"
		}
	}
	selector := shpyrdv1.LabelApp + "=" + app.Name + "," + shpyrdv1.LabelProcess
	if process != "" {
		selector += "=" + process
	}
	list, err := a.k.Kube.CoreV1().Pods(app.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, "", err
	}
	names := api.InstanceNames(list.Items)
	var running []corev1.Pod
	for _, p := range list.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	if len(running) == 0 {
		if process != "" && len(list.Items) > 0 {
			return nil, "", fmt.Errorf("no running %s instance yet (%s is %s); try again in a moment", process, names[list.Items[0].Name], list.Items[0].Status.Phase)
		}
		if process != "" {
			return nil, "", fmt.Errorf("no running %s instance found (is the project deployed?)", process)
		}
		return nil, "", errors.New("no running instance found (is the project deployed?)")
	}
	sort.Slice(running, func(i, j int) bool { return names[running[i].Name] < names[running[j].Name] })
	if instance != "" {
		for i := range running {
			if names[running[i].Name] == instance {
				return &running[i], instance, nil
			}
		}
		return nil, "", fmt.Errorf("instance %q not found; running: %s", instance, joinInstances(running, names))
	}
	return &running[0], names[running[0].Name], nil
}

func joinInstances(pods []corev1.Pod, names map[string]string) string {
	var out []string
	for _, p := range pods {
		out = append(out, names[p.Name])
	}
	return strings.Join(out, ", ")
}

// execInteractive runs command in the container, with a TTY when the local
// stdin is a terminal. When wantShell is set, it tries the CNB launcher first
// so buildpack environments are loaded, then bash, then sh.
func (a *appClient) execInteractive(ctx context.Context, namespace, pod, container string, command []string, wantShell, tty bool) error {
	if !wantShell {
		return remoteExit(a.exec(ctx, namespace, pod, container, command, tty))
	}
	var lastErr error
	for _, cmd := range [][]string{{cnbLauncher, "--", "bash"}, {"bash"}, {cnbLauncher, "--", "sh"}, {"sh"}} {
		err := a.exec(ctx, namespace, pod, container, cmd, tty)
		if err == nil {
			return nil
		}
		if !isNotFound(err) {
			return remoteExit(err)
		}
		lastErr = err
	}
	return fmt.Errorf("no usable shell in the image: %v", lastErr)
}

func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// remoteExit turns a remote non-zero exit into an exitError so the CLI exits
// with the same code instead of printing an error.
func remoteExit(err error) error {
	var ce kexec.CodeExitError
	if errors.As(err, &ce) {
		return &exitError{code: ce.Code}
	}
	return err
}

// isNotFound reports exec failures caused by a missing executable.
func isNotFound(err error) bool {
	m := err.Error()
	return strings.Contains(m, "executable file not found") || strings.Contains(m, "no such file or directory") || strings.Contains(m, "exit code 127") || strings.Contains(m, "exit code 126")
}

// exec streams an exec session; with tty the local terminal is put in raw
// mode and resizes are forwarded.
func (a *appClient) exec(ctx context.Context, namespace, pod, container string, command []string, tty bool) error {
	req := a.k.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    !tty,
			TTY:       tty,
		}, scheme.ParameterCodec)
	return a.stream(ctx, req.URL().String(), tty, os.Stdout)
}

// attach streams a pod's main process (used by `shpyrd run`).
func (a *appClient) attach(ctx context.Context, namespace, pod, container string, tty bool, stdout io.Writer) error {
	req := a.k.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: container,
			Stdin:     true,
			Stdout:    true,
			Stderr:    !tty,
			TTY:       tty,
		}, scheme.ParameterCodec)
	return a.stream(ctx, req.URL().String(), tty, stdout)
}

func (a *appClient) stream(ctx context.Context, rawURL string, tty bool, stdout io.Writer) error {
	ws, err := remotecommand.NewWebSocketExecutor(a.k.Config, "GET", rawURL)
	if err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(a.k.Config, "POST", u)
	if err != nil {
		return err
	}
	executor, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool { return httpstream.IsUpgradeFailure(err) })
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: os.Stdin, Stdout: stdout, Stderr: os.Stderr, Tty: tty}
	if tty {
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			defer term.Restore(int(os.Stdin.Fd()), state)
		}
		q := newSizeQueue()
		defer q.stop()
		opts.TerminalSizeQueue = q
	}
	return executor.StreamWithContext(ctx, opts)
}

type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// sizeQueue forwards terminal size changes to the remote side.
type sizeQueue struct {
	ch   chan remotecommand.TerminalSize
	done chan struct{}
}

func newSizeQueue() *sizeQueue {
	q := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 1), done: make(chan struct{})}
	q.push()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-sig:
				q.push()
			case <-q.done:
				return
			}
		}
	}()
	return q
}

func (q *sizeQueue) push() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	select {
	case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
	default:
	}
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case s := <-q.ch:
		return &s
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() { close(q.done) }

// ---- run --------------------------------------------------------------------

func newRunCmd(g *globalFlags) *cobra.Command {
	var (
		appName string
		size    string
		detach  bool
	)
	cmd := &cobra.Command{
		Use:   "run <command...>",
		Short: "Run a one-off command in a new instance of the current release",
		Long: `Start a temporary instance with the project's current build and config vars,
run the command with a terminal attached and remove the instance when it
exits (like 'heroku run'). Use it for migrations, consoles and scripts.

  shpyrd run npm run migrate
  shpyrd run rails console
  shpyrd run --detach ./import.sh     # leave it running; follow with 'shpyrd logs -p run'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			name, err := resolveAppName(appName)
			if err != nil {
				return err
			}
			ac, err := newAppClient(g, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			app, err := ac.getApp(ctx, name)
			if err != nil {
				return err
			}
			image := firstNonEmpty(app.Spec.Image, app.Status.Image)
			if image == "" {
				return errors.New("the project has no release yet; deploy first")
			}
			catalog, _, err := loadCatalog(ctx, ac.k)
			if err != nil {
				return err
			}
			res, _, err := catalog.Resolve(size, corev1.ResourceRequirements{})
			if err != nil {
				return err
			}
			tty := !detach && stdinIsTerminal()
			pod := runPod(app, image, args, res, tty, !detach)
			created, err := ac.k.Kube.CoreV1().Pods(app.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("start one-off instance: %w", err)
			}
			if detach {
				fmt.Fprintf(cmd.OutOrStdout(), "Started %s (follow with `shpyrd logs --process run --project %s`)\n", created.Name, name)
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Running on %s (%s)...\n", created.Name, sizeLabel(size, catalog))
			defer func() {
				_ = ac.k.Kube.CoreV1().Pods(app.Namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](5)})
			}()
			if err := ac.waitPodRunningOrDone(ctx, app.Namespace, created.Name, 3*time.Minute); err != nil {
				return err
			}
			if tty {
				fmt.Fprintln(cmd.ErrOrStderr(), "If you don't see a prompt, try pressing enter.")
			}
			out := &countingWriter{w: os.Stdout}
			attachErr := ac.attach(ctx, app.Namespace, created.Name, "app", tty, out)
			if out.n == 0 {
				// The command finished before we attached: print what it wrote.
				logs, lerr := ac.k.Kube.CoreV1().Pods(app.Namespace).GetLogs(created.Name, &corev1.PodLogOptions{Container: "app"}).DoRaw(ctx)
				if lerr == nil {
					_, _ = os.Stdout.Write(logs)
				}
			} else if attachErr != nil && ctx.Err() != nil {
				return attachErr
			}
			code, err := ac.exitCode(ctx, app.Namespace, created.Name)
			if err != nil {
				return err
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().SetInterspersed(false) // flags go before the command: shpyrd run --size shared-l sh -c '...'
	appFlag(cmd, &appName)
	cmd.Flags().StringVar(&size, "size", "", "instance size (default: the catalog default)")
	cmd.Flags().BoolVar(&detach, "detach", false, "start the instance and return; it is not deleted automatically")
	return cmd
}

func sizeLabel(size string, cat *sizes.Catalog) string {
	if size == "" {
		size = cat.Default
	}
	if s, ok := cat.Get(size); ok {
		return fmt.Sprintf("%s: %s CPU, %s", s.Name, s.CPU, s.Memory)
	}
	return size
}

// runPod builds the one-off pod: the release image and config vars, the
// command through the CNB launcher, stdin attached (a TTY when interactive).
// StdinOnce keeps stdout/stderr attached after stdin reaches EOF; without
// it the runtime detaches the session as soon as piped input ends.
func runPod(app *shpyrdv1.App, image string, command []string, res corev1.ResourceRequirements, tty, attach bool) *corev1.Pod {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := fmt.Sprintf("%s-run-%s", app.Name, hex.EncodeToString(suffix))
	// "--" makes the launcher exec the command as-is instead of via bash -c.
	cmd := append([]string{cnbLauncher, "--"}, command...)
	if !app.UsesBuildpacks() {
		cmd = command // Dockerfile and prebuilt images have no launcher
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: app.Namespace,
			Labels: map[string]string{
				shpyrdv1.LabelApp:       app.Name,
				shpyrdv1.LabelProcess:   "run",
				shpyrdv1.LabelManagedBy: "shpyrd",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: ptr.To[int64](3600),
			EnableServiceLinks:    ptr.To(false),
			Containers: []corev1.Container{{
				Name:      "app",
				Image:     image,
				Command:   cmd,
				Stdin:     attach,
				StdinOnce: attach,
				TTY:       tty,
				Resources: res,
				Env:       append([]corev1.EnvVar{{Name: "SHPYRD_RUN", Value: "1"}}, app.Spec.Env...),
				EnvFrom: []corev1.EnvFromSource{{
					SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: app.EnvSecretName()}, Optional: ptr.To(true)},
				}},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
			}},
		},
	}
}

func (a *appClient) waitPodRunningOrDone(ctx context.Context, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		p, err := a.k.Kube.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch p.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
			return nil
		}
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" || w.Reason == "CreateContainerError") {
				return fmt.Errorf("instance cannot start: %s: %s", w.Reason, w.Message)
			}
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the one-off instance to start")
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (a *appClient) exitCode(ctx context.Context, namespace, name string) (int, error) {
	for i := 0; i < 30; i++ {
		p, err := a.k.Kube.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return 0, nil
			}
			return 0, err
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				return int(cs.State.Terminated.ExitCode), nil
			}
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// exitError carries a remote exit code to main.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("command exited with code %d", e.code) }

// ExitCode returns the process exit code embedded in err, or 1.
func ExitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}
