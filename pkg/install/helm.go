package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"shpyrd/pkg/kube"
)

// helmClient wraps the Helm SDK for chart installs from embedded values.
type helmClient struct {
	kube     *kube.Client
	settings *cli.EnvSettings
	registry *registry.Client
	log      *slog.Logger
}

func newHelmClient(k *kube.Client, log *slog.Logger) (*helmClient, error) {
	settings := cli.New()
	rc, err := registry.NewClient(registry.ClientOptWriter(io.Discard), registry.ClientOptEnableCache(true))
	if err != nil {
		return nil, fmt.Errorf("helm registry client: %w", err)
	}
	return &helmClient{kube: k, settings: settings, registry: rc, log: log}, nil
}

func (h *helmClient) config(namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	logf := func(format string, v ...interface{}) {
		h.log.Debug(fmt.Sprintf(format, v...), "component", "helm")
	}
	var getter genericclioptions.RESTClientGetter
	if h.kube != nil {
		getter = h.kube.RESTClientGetterFor(namespace)
	}
	if err := cfg.Init(getter, namespace, "", logf); err != nil {
		return nil, fmt.Errorf("helm init: %w", err)
	}
	cfg.RegistryClient = h.registry
	return cfg, nil
}

// loadChart downloads (or reads from the Helm cache) and loads the chart.
func (h *helmClient) loadChart(spec *HelmSpec) (*chart.Chart, error) {
	cpo := action.ChartPathOptions{RepoURL: spec.Repo, Version: spec.Version}
	inst := action.NewInstall(&action.Configuration{})
	inst.SetRegistryClient(h.registry)
	inst.ChartPathOptions = cpo
	p, err := inst.ChartPathOptions.LocateChart(spec.Chart, h.settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart %s %s: %w", spec.Chart, spec.Version, err)
	}
	ch, err := loader.Load(p)
	if err != nil {
		return nil, fmt.Errorf("load chart %s: %w", p, err)
	}
	if ch.Metadata.Type != "" && ch.Metadata.Type != "application" {
		return nil, fmt.Errorf("chart %s is of type %q and cannot be installed", ch.Name(), ch.Metadata.Type)
	}
	return ch, nil
}

// loadValues reads and merges the component values with the profile overlay.
// Later files win.
func loadValues(tree fs.FS, files []string, vars map[string]string) (map[string]interface{}, error) {
	merged := map[string]interface{}{}
	for _, f := range files {
		b, err := fs.ReadFile(tree, f)
		if err != nil {
			return nil, err
		}
		b, err = Substitute(b, vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		v, err := chartutil.ReadValues(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		// CoalesceTables treats dst as authoritative, so the newer file (v)
		// overrides what has been merged so far.
		merged = chartutil.CoalesceTables(v.AsMap(), merged)
	}
	return merged, nil
}

// valuesFiles lists the values files for a component: the component's own
// files followed by profiles/<profile>/<component>/values.yaml when present.
func valuesFiles(tree fs.FS, c *Component, profile string) []string {
	var files []string
	for _, f := range c.Helm.Values {
		files = append(files, path.Join(c.Dir(), f))
	}
	overlay := path.Join("profiles", profile, c.Name, "values.yaml")
	if _, err := fs.Stat(tree, overlay); err == nil {
		files = append(files, overlay)
	}
	return files
}

// installOrUpgrade makes the release match the chart and values. Interrupted
// runs (pending-* status) are recovered first.
func (h *helmClient) installOrUpgrade(ctx context.Context, spec *HelmSpec, namespace string, ch *chart.Chart, vals map[string]interface{}, timeout time.Duration) (*release.Release, error) {
	cfg, err := h.config(namespace)
	if err != nil {
		return nil, err
	}

	hist := action.NewHistory(cfg)
	hist.Max = 1
	rels, err := hist.Run(spec.Release)
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound):
		return h.install(ctx, cfg, spec, namespace, ch, vals, timeout)
	case err != nil:
		return nil, fmt.Errorf("helm history %s: %w", spec.Release, err)
	}

	last := rels[0]
	if last.Info != nil && last.Info.Status.IsPending() {
		h.log.Warn("release left in pending state by an interrupted run, recovering", "release", spec.Release, "status", last.Info.Status)
		if last.Version <= 1 {
			un := action.NewUninstall(cfg)
			un.Wait = false
			un.Timeout = timeout
			if _, err := un.Run(spec.Release); err != nil {
				return nil, fmt.Errorf("helm uninstall pending %s: %w", spec.Release, err)
			}
			return h.install(ctx, cfg, spec, namespace, ch, vals, timeout)
		}
		rb := action.NewRollback(cfg)
		rb.Version = last.Version - 1
		rb.Timeout = timeout
		if err := rb.Run(spec.Release); err != nil {
			return nil, fmt.Errorf("helm rollback pending %s: %w", spec.Release, err)
		}
	}

	up := action.NewUpgrade(cfg)
	up.Namespace = namespace
	up.Timeout = timeout
	up.MaxHistory = 5
	up.SkipCRDs = spec.SkipCRDs
	up.Wait = false
	up.SetRegistryClient(h.registry)
	rel, err := up.RunWithContext(ctx, spec.Release, ch, vals)
	if err != nil {
		return nil, fmt.Errorf("helm upgrade %s: %w", spec.Release, err)
	}
	return rel, nil
}

func (h *helmClient) install(ctx context.Context, cfg *action.Configuration, spec *HelmSpec, namespace string, ch *chart.Chart, vals map[string]interface{}, timeout time.Duration) (*release.Release, error) {
	inst := action.NewInstall(cfg)
	inst.ReleaseName = spec.Release
	inst.Namespace = namespace
	inst.CreateNamespace = true
	inst.Timeout = timeout
	inst.SkipCRDs = spec.SkipCRDs
	inst.Wait = false
	inst.Labels = map[string]string{"app.kubernetes.io/managed-by": "shpyrd"}
	inst.SetRegistryClient(h.registry)
	rel, err := inst.RunWithContext(ctx, ch, vals)
	if err != nil {
		return nil, fmt.Errorf("helm install %s: %w", spec.Release, err)
	}
	return rel, nil
}

// template renders the chart without a cluster, for export.
func (h *helmClient) template(ctx context.Context, spec *HelmSpec, namespace string, ch *chart.Chart, vals map[string]interface{}) ([]*unstructured.Unstructured, error) {
	cfg := new(action.Configuration)
	inst := action.NewInstall(cfg)
	inst.ReleaseName = spec.Release
	inst.Namespace = namespace
	inst.DryRun = true
	inst.ClientOnly = true
	inst.IncludeCRDs = !spec.SkipCRDs
	inst.SetRegistryClient(h.registry)
	rel, err := inst.RunWithContext(ctx, ch, vals)
	if err != nil {
		return nil, fmt.Errorf("helm template %s: %w", spec.Release, err)
	}
	raw := []byte(rel.Manifest)
	for _, hook := range rel.Hooks {
		raw = append(raw, []byte("\n---\n"+hook.Manifest)...)
	}
	return decodeObjects(raw)
}
