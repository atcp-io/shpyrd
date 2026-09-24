package install

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"helm.sh/helm/v3/pkg/chart"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"shpyrd/deploy"
	"shpyrd/pkg/kube"
)

// DefaultSystemNamespace hosts shpyrd's own components and the install record.
const DefaultSystemNamespace = "shpyrd-system"

// InstallRecordName is the ConfigMap that records what was installed.
const InstallRecordName = "shpyrd-install"

// Options configures an Engine.
type Options struct {
	// Profile selects profiles/<name> in the manifest tree.
	Profile string
	// Vars override the profile variables (e.g. SHPYRD_DOMAIN).
	Vars map[string]string
	// Skip lists components to leave out; Only restricts to the listed ones.
	Skip []string
	Only []string
	// Version is recorded in the cluster and exposed as SHPYRD_VERSION.
	Version string
	// CADir holds the locally generated root CA (hook local-ca).
	CADir string
	// RegistryUser and RegistryPassword are the credentials of a private
	// registry (hook registry-credentials). They are written to a Secret,
	// never to the install record; empty keeps an existing Secret.
	RegistryUser     string
	RegistryPassword string
	// Tree overrides the embedded manifests (tests, development).
	Tree fs.FS
	// Extensions are the enabled extensions' components, appended to the
	// profile's runlevels (RFC-0002). Their names are recorded as
	// SHPYRD_EXTENSIONS for the server.
	Extensions []ExtensionComponent
	// Reporter receives progress; defaults to a slog based reporter.
	Reporter Reporter
	Logger   *slog.Logger
}

// ExtensionComponent is a component an enabled extension adds to a runlevel.
type ExtensionComponent struct {
	// Extension is the extension's name (recorded in SHPYRD_EXTENSIONS).
	Extension string
	// Component is the directory under components/ (may be empty when the
	// extension has no cluster component); Runlevel is where it goes.
	Component string
	Runlevel  string
}

// Reporter receives installer progress. Implementations must be safe for
// concurrent use: components in one runlevel run in parallel.
type Reporter interface {
	Runlevel(name string, components []string)
	Step(component, message string)
	Done(component string, took time.Duration)
	Failed(component string, err error)
}

// Engine applies a profile to a cluster.
type Engine struct {
	opts       Options
	log        *slog.Logger
	rep        Reporter
	tree       fs.FS
	kube       *kube.Client
	helm       *helmClient
	applier    *applier
	profile    *Profile
	components map[string]*Component
	vars       map[string]string
	chartMu    sync.Mutex
}

// New loads the profile and prepares clients. k may be nil for Export.
func New(k *kube.Client, opts Options) (*Engine, error) {
	if opts.Profile == "" {
		opts.Profile = "local"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Reporter == nil {
		opts.Reporter = &logReporter{log: opts.Logger}
	}
	if opts.Tree == nil {
		opts.Tree = deploy.FS
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}

	profile, err := LoadProfile(opts.Tree, opts.Profile)
	if err != nil {
		return nil, err
	}
	if err := profile.addExtensions(opts.Extensions); err != nil {
		return nil, err
	}
	comps, err := profile.Components(opts.Tree)
	if err != nil {
		return nil, err
	}
	byName := map[string]*Component{}
	for _, c := range comps {
		byName[c.Name] = c
	}
	for _, name := range append(append([]string{}, opts.Skip...), opts.Only...) {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("unknown component %q (profile %s has: %s)", name, profile.Name, strings.Join(profile.componentNames(), ", "))
		}
	}

	vars := mergeVars(map[string]string{
		VarProfile:  profile.Name,
		VarVersion:  opts.Version,
		VarSystemNS: DefaultSystemNamespace,
		VarCluster:  "shpyrd",
	}, profile.Vars)
	vars = mergeVars(vars, opts.Vars)
	vars = mergeVars(vars, derivedVars(vars, opts.Extensions))

	hc, err := newHelmClient(k, opts.Logger)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		opts:       opts,
		log:        opts.Logger,
		rep:        opts.Reporter,
		tree:       opts.Tree,
		kube:       k,
		helm:       hc,
		profile:    profile,
		components: byName,
		vars:       vars,
	}
	if k != nil {
		e.applier = &applier{kube: k, log: opts.Logger}
	}
	return e, nil
}

// Vars returns the effective variables.
func (e *Engine) Vars() map[string]string { return mergeVars(nil, e.vars) }

// Profile returns the loaded profile.
func (e *Engine) Profile() *Profile { return e.profile }

// SystemNamespace is where the install record lives.
func (e *Engine) SystemNamespace() string { return e.vars[VarSystemNS] }

// addExtensions appends the enabled extensions' components to the runlevels
// they name; a component already in the profile is left where it is.
func (p *Profile) addExtensions(exts []ExtensionComponent) error {
	present := map[string]bool{}
	for _, name := range p.componentNames() {
		present[name] = true
	}
	for _, x := range exts {
		if x.Component == "" || present[x.Component] {
			continue
		}
		found := false
		for i := range p.Runlevels {
			if p.Runlevels[i].Name == x.Runlevel {
				p.Runlevels[i].Components = append(p.Runlevels[i].Components, x.Component)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("extension %s: profile %s has no runlevel %q", x.Extension, p.Name, x.Runlevel)
		}
		present[x.Component] = true
	}
	return nil
}

func (p *Profile) componentNames() []string {
	var names []string
	for _, rl := range p.Runlevels {
		names = append(names, rl.Components...)
	}
	return names
}

// selected returns the components of a runlevel after Skip/Only filtering.
func (e *Engine) selected(rl Runlevel) []*Component {
	var out []*Component
	for _, name := range rl.Components {
		if contains(e.opts.Skip, name) {
			continue
		}
		if len(e.opts.Only) > 0 && !contains(e.opts.Only, name) {
			continue
		}
		out = append(out, e.components[name])
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Apply installs every selected component, runlevel by runlevel.
func (e *Engine) Apply(ctx context.Context) error {
	if e.kube == nil {
		return fmt.Errorf("no cluster connection")
	}
	if err := e.ensureNamespace(ctx, e.SystemNamespace()); err != nil {
		return err
	}
	for _, rl := range e.profile.Runlevels {
		comps := e.selected(rl)
		if len(comps) == 0 {
			continue
		}
		names := make([]string, 0, len(comps))
		for _, c := range comps {
			names = append(names, c.Name)
		}
		e.rep.Runlevel(rl.Name, names)

		g, gctx := errgroup.WithContext(ctx)
		for _, c := range comps {
			c := c
			g.Go(func() error {
				start := time.Now()
				if err := e.applyComponent(gctx, c); err != nil {
					e.rep.Failed(c.Name, err)
					return fmt.Errorf("%s: %w", c.Name, err)
				}
				e.rep.Done(c.Name, time.Since(start))
				return e.record(gctx, c)
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	return e.recordProfile(ctx)
}

func (e *Engine) applyComponent(ctx context.Context, c *Component) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout.Duration)
	defer cancel()

	for _, h := range c.Hooks {
		hook, ok := hooks[h]
		if !ok {
			return fmt.Errorf("unknown hook %q", h)
		}
		e.rep.Step(c.Name, "running hook "+h)
		if err := hook(ctx, e, c); err != nil {
			return fmt.Errorf("hook %s: %w", h, err)
		}
	}

	if c.Namespace != "" {
		if err := e.ensureNamespace(ctx, c.Namespace); err != nil {
			return err
		}
	}

	if c.Helm != nil {
		e.rep.Step(c.Name, fmt.Sprintf("fetching chart %s %s", c.Helm.Chart, c.Helm.Version))
		ch, vals, err := e.chartAndValues(c)
		if err != nil {
			return err
		}
		e.rep.Step(c.Name, fmt.Sprintf("installing release %s (%s %s) in %s", c.Helm.Release, ch.Name(), ch.Metadata.Version, c.Namespace))
		if _, err := e.helm.installOrUpgrade(ctx, c.Helm, c.Namespace, ch, vals, c.Timeout.Duration); err != nil {
			return err
		}
		e.kube.InvalidateCache()
	}

	if c.Kustomize != nil {
		objs, err := e.renderComponent(c)
		if err != nil {
			return err
		}
		e.rep.Step(c.Name, fmt.Sprintf("applying %d objects", len(objs)))
		if err := e.applier.applyAll(ctx, objs, c.Namespace); err != nil {
			return err
		}
	}

	for _, w := range c.Wait {
		e.rep.Step(c.Name, "waiting for "+w.String())
		if err := e.applier.waitFor(ctx, w, c.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// chartAndValues fetches the chart (serialised: the Helm cache is not safe
// for concurrent writers) and merges the values.
func (e *Engine) chartAndValues(c *Component) (*chart.Chart, map[string]interface{}, error) {
	e.chartMu.Lock()
	loaded, err := e.helm.loadChart(c.Helm)
	e.chartMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	vals, err := loadValues(e.tree, valuesFiles(e.tree, c, e.profile.Name), e.vars)
	if err != nil {
		return nil, nil, err
	}
	return loaded, vals, nil
}

// renderComponent renders the profile overlay when present, else the base.
func (e *Engine) renderComponent(c *Component) ([]*unstructured.Unstructured, error) {
	dir := path.Join(c.Dir(), c.Kustomize.Path)
	overlay := path.Join("profiles", e.profile.Name, c.Name)
	if _, err := fs.Stat(e.tree, path.Join(overlay, "kustomization.yaml")); err == nil {
		dir = overlay
	}
	return renderKustomize(e.tree, dir, e.vars)
}

func (e *Engine) ensureNamespace(ctx context.Context, name string) error {
	ns := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":   name,
			"labels": map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
	}}
	if err := e.applier.applyOne(ctx, ns, "", false); err != nil {
		return fmt.Errorf("namespace %s: %w", name, err)
	}
	return nil
}

// Export renders every selected component to dir as numbered YAML files plus
// a kustomization.yaml, for GitOps tooling. Helm charts are templated
// client-side.
func (e *Engine) Export(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var files []string
	n := 0
	for _, rl := range e.profile.Runlevels {
		for _, c := range e.selected(rl) {
			var objs []*unstructured.Unstructured
			if c.Namespace != "" {
				objs = append(objs, &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "v1", "kind": "Namespace",
					"metadata": map[string]interface{}{"name": c.Namespace},
				}})
			}
			if c.Helm != nil {
				ch, vals, err := e.chartAndValues(c)
				if err != nil {
					return fmt.Errorf("%s: %w", c.Name, err)
				}
				rendered, err := e.helm.template(ctx, c.Helm, c.Namespace, ch, vals)
				if err != nil {
					return fmt.Errorf("%s: %w", c.Name, err)
				}
				objs = append(objs, rendered...)
			}
			if c.Kustomize != nil {
				rendered, err := e.renderComponent(c)
				if err != nil {
					return fmt.Errorf("%s: %w", c.Name, err)
				}
				objs = append(objs, rendered...)
			}
			for _, o := range objs {
				if o.GetNamespace() == "" && c.Namespace != "" && isLikelyNamespaced(o.GetKind()) {
					o.SetNamespace(c.Namespace)
				}
			}
			sortObjects(objs)
			name := fmt.Sprintf("%02d-%s-%s.yaml", n, rl.Name, c.Name)
			n++
			if err := writeObjects(filepath.Join(dir, name), objs); err != nil {
				return err
			}
			files = append(files, name)
			e.rep.Step(c.Name, "exported "+name)
		}
	}
	kust := map[string]interface{}{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  files,
	}
	b, _ := yaml.Marshal(kust)
	return os.WriteFile(filepath.Join(dir, "kustomization.yaml"), b, 0o644)
}

func writeObjects(file string, objs []*unstructured.Unstructured) error {
	var sb strings.Builder
	for _, o := range objs {
		b, err := yaml.Marshal(o.Object)
		if err != nil {
			return err
		}
		sb.WriteString("---\n")
		sb.Write(b)
	}
	return os.WriteFile(file, []byte(sb.String()), 0o644)
}

var clusterScopedKinds = map[string]bool{
	"Namespace": true, "CustomResourceDefinition": true, "ClusterRole": true, "ClusterRoleBinding": true,
	"StorageClass": true, "PersistentVolume": true, "IngressClass": true, "ClusterIssuer": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true, "APIService": true,
	"PriorityClass": true, "ClusterStore": true, "ClusterStack": true, "ClusterBuilder": true,
	"ClusterBuildpack": true, "ClusterLifecycle": true, "Bundle": true, "ValidatingAdmissionPolicy": true,
	"ValidatingAdmissionPolicyBinding": true, "RuntimeClass": true, "CSIDriver": true,
}

func isLikelyNamespaced(kind string) bool { return !clusterScopedKinds[kind] }

// ComponentStatus is the health of one component as seen by Status.
type ComponentStatus struct {
	Runlevel  string
	Name      string
	Ready     bool
	Detail    string
	AppliedAt string
	Version   string
}

// Status re-evaluates every wait condition with a short timeout and merges the
// result with the install record.
func (e *Engine) Status(ctx context.Context) ([]ComponentStatus, error) {
	if e.kube == nil {
		return nil, fmt.Errorf("no cluster connection")
	}
	record, _ := e.readRecord(ctx)
	var out []ComponentStatus
	for _, rl := range e.profile.Runlevels {
		for _, c := range e.selected(rl) {
			st := ComponentStatus{Runlevel: rl.Name, Name: c.Name, Ready: true}
			if rec, ok := record[c.Name]; ok {
				st.AppliedAt = rec.AppliedAt
				st.Version = rec.Version
			} else {
				st.Ready = false
				st.Detail = "not installed"
			}
			if st.Ready {
				for _, w := range c.Wait {
					wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					err := e.applier.waitFor(wctx, w, c.Namespace)
					cancel()
					if err != nil {
						st.Ready = false
						st.Detail = w.String() + " not ready"
						break
					}
				}
			}
			out = append(out, st)
		}
	}
	return out, nil
}

// Remove deletes a component from the cluster: the Helm release, the
// rendered Kustomize objects and its install record entry. Used when an
// extension is disabled.
func (e *Engine) Remove(ctx context.Context, name string) error {
	c, ok := e.components[name]
	if !ok {
		return fmt.Errorf("unknown component %q", name)
	}
	start := time.Now()
	if c.Kustomize != nil {
		objs, err := e.renderComponent(c)
		if err != nil {
			return err
		}
		e.rep.Step(c.Name, fmt.Sprintf("deleting %d objects", len(objs)))
		if err := e.applier.deleteAll(ctx, objs, c.Namespace); err != nil {
			return err
		}
	}
	if c.Helm != nil {
		e.rep.Step(c.Name, "uninstalling Helm release "+c.Helm.Release)
		if err := e.helm.uninstall(c.Helm, c.Namespace, c.Timeout.Duration); err != nil {
			return err
		}
	}
	// Dropping the record key: apply an empty ConfigMap as this component's
	// field manager, so server-side apply removes the field it owned.
	cm := e.recordObject(map[string]interface{}{})
	if err := e.applier.applyOneAs(ctx, cm, e.SystemNamespace(), false, fieldManager+"-record-"+c.Name); err != nil {
		return err
	}
	e.rep.Done(c.Name, time.Since(start))
	return nil
}

// ComponentRecord is one entry of the install record ConfigMap.
type ComponentRecord struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	AppliedAt string `json:"appliedAt"`
}

func (e *Engine) record(ctx context.Context, c *Component) error {
	rec := ComponentRecord{Name: c.Name, AppliedAt: time.Now().UTC().Format(time.RFC3339)}
	if c.Helm != nil {
		rec.Version = c.Helm.Version
	}
	b, err := yaml.Marshal(rec)
	if err != nil {
		return err
	}
	cm := e.recordObject(map[string]interface{}{"component." + c.Name: string(b)})
	return e.applier.applyOneAs(ctx, cm, e.SystemNamespace(), false, fieldManager+"-record-"+c.Name)
}

func (e *Engine) recordProfile(ctx context.Context) error {
	vars, _ := yaml.Marshal(e.vars)
	overrides, _ := yaml.Marshal(e.overrides())
	cm := e.recordObject(map[string]interface{}{
		"profile":   e.profile.Name,
		"version":   e.opts.Version,
		"vars":      string(vars),
		"overrides": string(overrides),
		"updatedAt": time.Now().UTC().Format(time.RFC3339),
	})
	return e.applier.applyOneAs(ctx, cm, e.SystemNamespace(), false, fieldManager+"-record-profile")
}

// recordObject builds a partial ConfigMap for server-side apply. Each writer
// uses its own field manager (see record/recordProfile) and therefore owns
// only the keys it sets, so components do not clobber each other.
func (e *Engine) recordObject(data map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      InstallRecordName,
			"namespace": e.SystemNamespace(),
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": fieldManager},
		},
		"data": data,
	}}
}

// readRecord returns the per-component records from the cluster.
func (e *Engine) readRecord(ctx context.Context) (map[string]ComponentRecord, error) {
	return ReadRecords(ctx, e.kube, e.SystemNamespace())
}

// ReadRecords returns the per-component install records keyed by name.
func ReadRecords(ctx context.Context, k *kube.Client, systemNS string) (map[string]ComponentRecord, error) {
	if systemNS == "" {
		systemNS = DefaultSystemNamespace
	}
	cm, err := k.Kube.CoreV1().ConfigMaps(systemNS).Get(ctx, InstallRecordName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]ComponentRecord{}
	for key, v := range cm.Data {
		if !strings.HasPrefix(key, "component.") {
			continue
		}
		var rec ComponentRecord
		if err := yaml.Unmarshal([]byte(v), &rec); err == nil {
			out[rec.Name] = rec
		}
	}
	return out, nil
}

// InstallInfo is the profile level part of the install record.
type InstallInfo struct {
	Profile string
	Version string
	Vars    map[string]string
	// Overrides are the variables the operator set explicitly (--set and
	// the flags that map to variables); `cluster init` carries them over
	// so a re-run does not need every flag again.
	Overrides map[string]string
	UpdatedAt string
}

// overrides are the explicitly given variables worth carrying over: the
// ones that differ from what the profile would derive on its own.
func (e *Engine) overrides() map[string]string {
	out := map[string]string{}
	for k, v := range e.opts.Vars {
		switch k {
		case VarDomain, VarCluster, VarHTTPPort, VarHTTPSPort, VarFrontDoor, VarLocalDNS:
			continue // seeded from their own flags
		case VarServerImage:
			continue // follows the CLI version; a development image is for one run
		}
		if e.profile.Vars[k] == v {
			continue
		}
		out[k] = v
	}
	return out
}

// ReadInstallInfo reads the profile level record, or an error when the
// cluster was never initialised.
func ReadInstallInfo(ctx context.Context, k *kube.Client, systemNS string) (*InstallInfo, error) {
	if systemNS == "" {
		systemNS = DefaultSystemNamespace
	}
	cm, err := k.Kube.CoreV1().ConfigMaps(systemNS).Get(ctx, InstallRecordName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	info := &InstallInfo{Profile: cm.Data["profile"], Version: cm.Data["version"], UpdatedAt: cm.Data["updatedAt"]}
	_ = yaml.Unmarshal([]byte(cm.Data["vars"]), &info.Vars)
	_ = yaml.Unmarshal([]byte(cm.Data["overrides"]), &info.Overrides)
	return info, nil
}

// logReporter is the default Reporter.
type logReporter struct{ log *slog.Logger }

func (r *logReporter) Runlevel(name string, comps []string) {
	r.log.Info("runlevel", "name", name, "components", strings.Join(comps, ","))
}
func (r *logReporter) Step(c, msg string) { r.log.Info(msg, "component", c) }
func (r *logReporter) Done(c string, took time.Duration) {
	r.log.Info("done", "component", c, "took", took.Round(time.Second))
}
func (r *logReporter) Failed(c string, err error) { r.log.Error("failed", "component", c, "err", err) }
