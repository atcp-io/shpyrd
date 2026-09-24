package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/api"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/project"
)

// appNamespace is the namespace of a project (app-<slug>).
func appNamespace(slug string) string { return project.Namespace(slug) }

// validateAppName checks a project identifier given on the command line or
// in shpyrd.yaml: always the slug, never the display name.
func validateAppName(slug string) error {
	if !project.ValidSlug(slug) {
		return fmt.Errorf("invalid project %q: use its slug (lowercase letters, digits and dashes, max %d characters)", slug, project.MaxSlugLength)
	}
	return nil
}

// projectConfig is the optional shpyrd.yaml in a repository, the fly.toml
// equivalent: which project this is, its process types, build settings and
// domains. Everything but `project` is optional (`app` is accepted as an
// alias for compatibility).
//
//	project: my-service
//	processes:
//	  web: { port: 8080 }
//	  worker: {}
//	build:
//	  env:
//	    BP_GO_TARGETS: ./cmd/web:./cmd/worker
//	domains: [my-service.example.com]
//	globals: false            # or: globals: { exclude: [OPENAI_API_KEY] }
type projectConfig struct {
	Project   string                    `json:"project"`
	App       string                    `json:"app"` // deprecated alias of project
	Processes map[string]projectProcess `json:"processes,omitempty"`
	Build     *projectBuild             `json:"build,omitempty"`
	Domains   []string                  `json:"domains,omitempty"`
	Globals   *projectGlobals           `json:"globals,omitempty"`
}

// projectGlobals is the `globals` key (RFC-0016): `false` opts the project
// out of every global config var, `{exclude: [NAME]}` out of some.
type projectGlobals struct {
	Disabled bool
	Exclude  []string
}

func (g *projectGlobals) UnmarshalJSON(b []byte) error {
	var enabled bool
	if err := json.Unmarshal(b, &enabled); err == nil {
		g.Disabled = !enabled
		return nil
	}
	var obj struct {
		Exclude []string `json:"exclude"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("globals: expected false or {exclude: [NAME...]}: %w", err)
	}
	g.Exclude = obj.Exclude
	return nil
}

func (g *projectGlobals) spec() *shpyrdv1.Globals {
	if g == nil {
		return nil
	}
	if !g.Disabled && len(g.Exclude) == 0 {
		return nil // `globals: true`: everything, the default
	}
	return &shpyrdv1.Globals{Disabled: g.Disabled, Exclude: g.Exclude}
}

type projectProcess struct {
	Port     *int32   `json:"port,omitempty"`
	Replicas *int32   `json:"replicas,omitempty"`
	Command  []string `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	// Volumes mounts project volumes: [{name: data, path: /data}].
	Volumes []shpyrdv1.VolumeMount `json:"volumes,omitempty"`
	// Size names an instance size from the cluster catalog (`shpyrd sizes
	// list`), e.g. shared-m or dedicated-s. Empty means the catalog default.
	Size string `json:"size,omitempty"`
	// CPU and Memory override the size's limits, e.g. "500m"/"2" and
	// "256Mi"/"1Gi". Prefer a size.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// HealthCheck configures the readiness probe (RFC-0019). Omit for the
	// default (HTTP / for web, TCP for other ports, none for workers).
	// Disable all checks with `healthCheck: {disabled: true}`.
	//
	//   healthCheck:
	//     path: /healthz
	//     interval: 5s
	//     gracePeriod: 30s
	HealthCheck *shpyrdv1.HealthCheck `json:"healthCheck,omitempty"`
}

func (pp projectProcess) resources(cur corev1.ResourceRequirements) (corev1.ResourceRequirements, error) {
	if pp.CPU == "" && pp.Memory == "" {
		return cur, nil
	}
	out := corev1.ResourceRequirements{Limits: corev1.ResourceList{}}
	if pp.CPU != "" {
		q, err := resource.ParseQuantity(pp.CPU)
		if err != nil {
			return out, fmt.Errorf("cpu %q: %w", pp.CPU, err)
		}
		out.Limits[corev1.ResourceCPU] = q
	}
	if pp.Memory != "" {
		q, err := resource.ParseQuantity(pp.Memory)
		if err != nil {
			return out, fmt.Errorf("memory %q: %w", pp.Memory, err)
		}
		out.Limits[corev1.ResourceMemory] = q
	}
	return out, nil
}

type projectBuild struct {
	// Strategy is "buildpacks" or "dockerfile"; empty means auto: dockerfile
	// when the deployed directory has a Dockerfile, buildpacks otherwise.
	Strategy string `json:"strategy,omitempty"`
	// Env are build-time variables (BP_* for buildpacks, build args for Dockerfiles).
	Env     map[string]string `json:"env,omitempty"`
	Builder string            `json:"builder,omitempty"`
	// Dockerfile path relative to the deployed directory (default "Dockerfile").
	Dockerfile string `json:"dockerfile,omitempty"`
	// Target is the multi-stage build target.
	Target string `json:"target,omitempty"`
}

// applyTo writes the project settings into the App spec. Declared process
// types are authoritative (undeclared ones are removed); replica counts set
// on the cluster survive unless the file pins them.
func (pc *projectConfig) applyTo(a *shpyrdv1.App) error {
	if pc == nil {
		return nil
	}
	if len(pc.Processes) > 0 {
		procs := map[string]shpyrdv1.Process{}
		for name, pp := range pc.Processes {
			cur := a.Spec.Processes[name]
			res, err := pp.resources(cur.Resources)
			if err != nil {
				return fmt.Errorf("shpyrd.yaml: process %s: %w", name, err)
			}
			for _, m := range pp.Volumes {
				if m.Name == "" || m.Path == "" {
					return fmt.Errorf("shpyrd.yaml: process %s: volumes need name and path", name)
				}
			}
			proc := shpyrdv1.Process{Replicas: cur.Replicas, Port: pp.Port, Command: pp.Command, Args: pp.Args, Size: firstNonEmpty(pp.Size, cur.Size), Resources: res, Volumes: pp.Volumes}
			if pp.Replicas != nil {
				proc.Replicas = pp.Replicas
			}
			procs[name] = proc
		}
		a.Spec.Processes = procs
	}
	if pc.Build != nil {
		switch pc.Build.Strategy {
		case "", shpyrdv1.StrategyBuildpacks, shpyrdv1.StrategyDockerfile:
		default:
			return fmt.Errorf("shpyrd.yaml: build.strategy must be buildpacks or dockerfile, got %q", pc.Build.Strategy)
		}
		b := &shpyrdv1.Build{Strategy: pc.Build.Strategy, Builder: pc.Build.Builder, Dockerfile: pc.Build.Dockerfile, Target: pc.Build.Target}
		if b.Strategy == "" && (b.Dockerfile != "" || b.Target != "") {
			b.Strategy = shpyrdv1.StrategyDockerfile
		}
		keys := make([]string, 0, len(pc.Build.Env))
		for k := range pc.Build.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.Env = append(b.Env, corev1.EnvVar{Name: k, Value: pc.Build.Env[k]})
		}
		a.Spec.Build = b
	}
	if len(pc.Domains) > 0 {
		a.Spec.Domains = pc.Domains
	}
	// Carry healthCheck from shpyrd.yaml into App.Spec.Processes (RFC-0019).
	for k, pp := range pc.Processes {
		if pp.HealthCheck != nil {
			if p, ok := a.Spec.Processes[k]; ok {
				p.HealthCheck = pp.HealthCheck
				a.Spec.Processes[k] = p
			}
		}
	}
	if pc.Globals != nil {
		a.Spec.Globals = pc.Globals.spec()
	}
	return nil
}

// loadProjectConfig reads ./shpyrd.yaml when present.
func loadProjectConfig() (*projectConfig, error) {
	b, err := os.ReadFile("shpyrd.yaml")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pc projectConfig
	if err := yaml.Unmarshal(b, &pc); err != nil {
		return nil, fmt.Errorf("shpyrd.yaml: %w", err)
	}
	return &pc, nil
}

// resolveAppName picks the project from --project, then shpyrd.yaml.
func resolveAppName(flag string) (string, error) {
	if flag != "" {
		return flag, validateAppName(flag)
	}
	pc, err := loadProjectConfig()
	if err != nil {
		return "", err
	}
	if pc != nil {
		if name := firstNonEmpty(pc.Project, pc.App); name != "" {
			return name, validateAppName(name)
		}
	}
	return "", errors.New("no project selected: pass --project <name> or add `project: <name>` to shpyrd.yaml")
}

// appClient bundles the clients the app commands need. It talks to the
// Kubernetes API with the user's kubeconfig; the only call that reaches the
// shpyrd server (source upload) goes through the API server's service proxy.
type appClient struct {
	k   *kube.Client
	c   client.Client
	out io.Writer
}

func newAppClient(g *globalFlags, out io.Writer) (*appClient, error) {
	k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
	if err != nil {
		return nil, err
	}
	c, err := k.ControllerClient()
	if err != nil {
		return nil, err
	}
	return &appClient{k: k, c: c, out: out}, nil
}

func (a *appClient) getApp(ctx context.Context, name string) (*shpyrdv1.App, error) {
	if !project.ValidSlug(name) {
		return nil, fmt.Errorf("invalid project %q: use its slug, the identifier in the PROJECT column of `shpyrd projects list`", name)
	}
	app := &shpyrdv1.App{}
	err := a.c.Get(ctx, types.NamespacedName{Namespace: appNamespace(name), Name: name}, app)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("project %q not found; create it with `shpyrd projects create %s`", name, name)
	}
	if err != nil {
		return nil, err
	}
	return app, nil
}

// updateApp applies mutate with a retry on conflicts.
func (a *appClient) updateApp(ctx context.Context, name string, mutate func(*shpyrdv1.App) error) (*shpyrdv1.App, error) {
	for attempt := 0; attempt < 5; attempt++ {
		app, err := a.getApp(ctx, name)
		if err != nil {
			return nil, err
		}
		if err := mutate(app); err != nil {
			return nil, err
		}
		if err := a.c.Update(ctx, app); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return nil, err
		}
		return app, nil
	}
	return nil, errors.New("too many conflicts updating the project")
}

// uploadSource sends an archive to shpyrd-server through the API server
// proxy (works with any kubeconfig and no ingress). The admin token is read
// from the cluster and sent in X-Shpyrd-Token because the proxy strips
// Authorization headers.
func (a *appClient) uploadSource(ctx context.Context, archive []byte) (*api.SourceInfo, error) {
	req := a.k.Kube.CoreV1().RESTClient().Post().
		Namespace(install.DefaultSystemNamespace).
		Resource("services").
		Name("shpyrd-server:http").
		SubResource("proxy").
		Suffix("api/sources").
		SetHeader("Content-Type", "application/gzip").
		Body(archive)
	if sec, err := a.k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace).Get(ctx, install.AdminTokenSecretName, metav1.GetOptions{}); err == nil {
		if tok := strings.TrimSpace(string(sec.Data["token"])); tok != "" {
			req.SetHeader("X-Shpyrd-Token", tok)
		}
	}
	raw, err := req.Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("upload source to shpyrd-server: %w (is the base stack installed? `shpyrd cluster status`)", err)
	}
	var info api.SourceInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("upload source: unexpected response: %s", truncate(string(raw), 200))
	}
	return &info, nil
}

// waitForBuild polls until the App reports a build other than prev.
func (a *appClient) waitForBuild(ctx context.Context, name, prev string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		app, err := a.getApp(ctx, name)
		if err != nil {
			return "", err
		}
		if b := app.Status.LatestBuild; b != "" && b != prev {
			return b, nil
		}
		if app.Status.Phase == shpyrdv1.PhaseFailed && app.Status.ObservedGeneration >= app.Generation {
			return "", errors.New(app.Status.Message)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for kpack to start a build (%s)", firstNonEmpty(app.Status.Message, app.Status.Phase))
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
}

// waitForNewBuild polls until a build other than prev exists: the App's
// LatestBuild (Dockerfile builds report it at once) or a kpack Build created
// after the request (kpack only moves latestBuildRef when a triggered build
// finishes).
func (a *appClient) waitForNewBuild(ctx context.Context, name, prev string, since time.Time, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		app, err := a.getApp(ctx, name)
		if err != nil {
			return "", err
		}
		if b := app.Status.LatestBuild; b != "" && b != prev {
			return b, nil
		}
		if b := a.newestKpackBuild(ctx, app, since); b != "" && b != prev {
			return b, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for a new build (%s)", firstNonEmpty(app.Status.Message, app.Status.Phase))
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
}

// newestKpackBuild returns the name of the newest kpack Build of the app
// created at or after since, or "".
func (a *appClient) newestKpackBuild(ctx context.Context, app *shpyrdv1.App, since time.Time) string {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kpack.io", Version: "v1alpha2", Kind: "BuildList"})
	if err := a.c.List(ctx, list, client.InNamespace(app.Namespace), client.MatchingLabels{"image.kpack.io/image": app.Name}); err != nil {
		return ""
	}
	var newest *unstructured.Unstructured
	for i := range list.Items {
		b := &list.Items[i]
		if b.GetCreationTimestamp().Time.Before(since.Add(-time.Second)) {
			continue
		}
		if newest == nil || b.GetCreationTimestamp().After(newest.GetCreationTimestamp().Time) {
			newest = b
		}
	}
	if newest == nil {
		return ""
	}
	return newest.GetName()
}

// followBuild streams the build pod's steps to out, in order, and returns
// an error when a step fails. kpack builds run their phases as init
// containers; Dockerfile builds fetch the source in an init container and
// build in the main one.
func (a *appClient) followBuild(ctx context.Context, namespace, build string) error {
	pods := a.k.Kube.CoreV1().Pods(namespace)

	var pod *corev1.Pod
	deadline := time.Now().Add(3 * time.Minute)
	for {
		p, err := api.FindBuildPod(ctx, pods, build)
		if err == nil {
			pod = p
			break
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the build instance of build %s did not appear", build)
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	podName := pod.Name

	steps := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, ic := range steps {
		// Wait for the step to start.
		for {
			p, err := pods.Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			st := api.ContainerStatus(p, ic.Name)
			if st != nil && (st.State.Running != nil || st.State.Terminated != nil) {
				break
			}
			if p.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("build instance failed before step %s", ic.Name)
			}
			if err := sleepCtx(ctx, time.Second); err != nil {
				return err
			}
		}
		fmt.Fprintf(a.out, "\n===> %s\n", ic.Name)
		stream, err := pods.GetLogs(podName, &corev1.PodLogOptions{Container: ic.Name, Follow: true}).Stream(ctx)
		if err != nil {
			return fmt.Errorf("logs of step %s: %w", ic.Name, err)
		}
		_, copyErr := io.Copy(a.out, stream)
		stream.Close()
		if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
			return copyErr
		}
		// Check the exit code.
		p, err := pods.Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if st := api.ContainerStatus(p, ic.Name); st != nil && st.State.Terminated != nil && st.State.Terminated.ExitCode != 0 {
			return fmt.Errorf("build step %s failed (exit %d)", ic.Name, st.State.Terminated.ExitCode)
		}
	}
	return nil
}

// waitRunning polls the App until it is Running (or Failed) for the given
// generation, printing phase changes.
func (a *appClient) waitRunning(ctx context.Context, name string, generation int64, timeout time.Duration) (*shpyrdv1.App, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		app, err := a.getApp(ctx, name)
		if err != nil {
			return nil, err
		}
		if app.Status.ObservedGeneration >= generation {
			line := app.Status.Phase
			if app.Status.Message != "" {
				line += ": " + app.Status.Message
			}
			if line != last {
				fmt.Fprintf(a.out, "    %s\n", line)
				last = line
			}
			switch app.Status.Phase {
			case shpyrdv1.PhaseRunning:
				return app, nil
			case shpyrdv1.PhaseFailed:
				return app, errors.New(app.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			return app, fmt.Errorf("timed out waiting for %s to be running (%s)", name, app.Status.Phase)
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return app, err
		}
	}
}

// streamPodLogs tails (and optionally follows) logs of every pod matching
// the selector. Lines are printed Heroku style, "<time> <instance> | msg",
// where instances are named <process>.<n> by creation order. New pods are
// picked up while following.
func (a *appClient) streamPodLogs(ctx context.Context, namespace, selector string, follow bool, tail int64) error {
	pods := a.k.Kube.CoreV1().Pods(namespace)
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	names := map[string]string{}

	start := func(pod corev1.Pod) {
		seen[pod.Name] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			instance := names[pod.Name]
			mu.Unlock()
			opts := &corev1.PodLogOptions{Container: "app", Follow: follow, Timestamps: true}
			if tail >= 0 {
				opts.TailLines = &tail
			}
			stream, err := pods.GetLogs(pod.Name, opts).Stream(ctx)
			if err != nil {
				mu.Lock()
				fmt.Fprintf(a.out, "%-20s %s | (no logs: %v)\n", "", instance, err)
				mu.Unlock()
				return
			}
			defer stream.Close()
			sc := bufio.NewScanner(stream)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				ts := ""
				if i := strings.IndexByte(line, ' '); i > 0 {
					if t, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
						ts = t.Local().Format("2006-01-02T15:04:05")
						line = line[i+1:]
					}
				}
				mu.Lock()
				fmt.Fprintf(a.out, "%s %s | %s\n", ts, instance, line)
				mu.Unlock()
			}
		}()
	}

	for {
		list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		mu.Lock()
		for k, v := range api.InstanceNames(list.Items) {
			names[k] = v
		}
		mu.Unlock()
		for _, p := range list.Items {
			if !seen[p.Name] && p.Status.Phase != corev1.PodPending {
				start(p)
			}
		}
		if !follow {
			break
		}
		if err := sleepCtx(ctx, 5*time.Second); err != nil {
			break
		}
	}
	wg.Wait()
	if len(seen) == 0 {
		return fmt.Errorf("no instances found for selector %q", selector)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// digest reduces an image reference to its short digest so registry
// internals never appear in command output.
func digest(ref string) string {
	if d := api.Digest(ref); d != "" {
		return d
	}
	if ref == "" {
		return "-"
	}
	return ref
}

func age(t metav1.Time) string {
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
