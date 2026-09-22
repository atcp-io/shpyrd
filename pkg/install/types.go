// Package install renders and applies the shpyrd base stack in dependency
// ordered runlevels, from manifests embedded in the binary.
//
// Layout of the manifest tree (see deploy/):
//
//	components/<name>/component.yaml   component definition
//	components/<name>/values.yaml      Helm values (helm components)
//	components/<name>/base/            Kustomize base (kustomize components)
//	profiles/<profile>/profile.yaml    ordered runlevels and default variables
//	profiles/<profile>/<name>/values.yaml         values overlay for a component
//	profiles/<profile>/<name>/kustomization.yaml  Kustomize overlay for a component
package install

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Profile is an ordered list of components with default variables.
type Profile struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
	Runlevels   []Runlevel        `json:"runlevels"`
}

// Runlevel groups components that can be applied together. Levels are
// applied in order and each level is waited for before the next starts.
type Runlevel struct {
	Name       string   `json:"name"`
	Components []string `json:"components"`
}

// Component is one installable unit: a Helm chart, a Kustomize tree, or both.
type Component struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Namespace is the target namespace, created when missing. Kustomize
	// resources without a namespace are placed here.
	Namespace string `json:"namespace,omitempty"`
	// Hooks are Go functions run before the component is applied
	// (see hooks.go), e.g. to create a Secret from locally generated data.
	Hooks     []string       `json:"hooks,omitempty"`
	Helm      *HelmSpec      `json:"helm,omitempty"`
	Kustomize *KustomizeSpec `json:"kustomize,omitempty"`
	Wait      []WaitSpec     `json:"wait,omitempty"`
	// Timeout for the whole component including waits. Defaults to 5m.
	Timeout metav1Duration `json:"timeout,omitempty"`

	dir string // directory inside the manifest tree
}

// HelmSpec describes a chart to install with the Helm SDK.
type HelmSpec struct {
	// Repo is an HTTP(S) chart repository URL. Leave empty for OCI charts and
	// put the full oci:// reference in Chart.
	Repo    string `json:"repo,omitempty"`
	Chart   string `json:"chart"`
	Version string `json:"version,omitempty"`
	// Release defaults to the component name.
	Release string `json:"release,omitempty"`
	// Values are file names relative to the component directory.
	Values []string `json:"values,omitempty"`
	// SkipCRDs installs the chart without its crds/ directory.
	SkipCRDs bool `json:"skipCRDs,omitempty"`
}

// KustomizeSpec describes a Kustomize tree relative to the component directory.
type KustomizeSpec struct {
	Path string `json:"path"`
}

// WaitSpec is one readiness condition. Exactly one field is set.
type WaitSpec struct {
	// Deployment, StatefulSet, DaemonSet and Job take "namespace/name".
	Deployment  string `json:"deployment,omitempty"`
	StatefulSet string `json:"statefulSet,omitempty"`
	DaemonSet   string `json:"daemonSet,omitempty"`
	Job         string `json:"job,omitempty"`
	// CRD waits for a CustomResourceDefinition to be Established.
	CRD string `json:"crd,omitempty"`
	// Endpoints waits for "namespace/service" to have a ready address.
	Endpoints string `json:"endpoints,omitempty"`
	// DryRun repeatedly server-side dry-runs the given object until the API
	// server (and any admission webhook) accepts it.
	DryRun map[string]interface{} `json:"dryRun,omitempty"`
	// Condition waits for a status condition on any object (typically a
	// custom resource) to be True.
	Condition *ConditionSpec `json:"condition,omitempty"`
}

// ConditionSpec identifies an object and the status condition to wait for.
type ConditionSpec struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	// Type defaults to Ready.
	Type string `json:"type,omitempty"`
}

func (w WaitSpec) String() string {
	switch {
	case w.Deployment != "":
		return "deployment/" + w.Deployment
	case w.StatefulSet != "":
		return "statefulset/" + w.StatefulSet
	case w.DaemonSet != "":
		return "daemonset/" + w.DaemonSet
	case w.Job != "":
		return "job/" + w.Job
	case w.CRD != "":
		return "crd/" + w.CRD
	case w.Endpoints != "":
		return "endpoints/" + w.Endpoints
	case w.DryRun != nil:
		kind, _ := w.DryRun["kind"].(string)
		return "dry-run/" + kind
	case w.Condition != nil:
		t := w.Condition.Type
		if t == "" {
			t = "Ready"
		}
		name := w.Condition.Name
		if w.Condition.Namespace != "" {
			name = w.Condition.Namespace + "/" + name
		}
		return strings.ToLower(w.Condition.Kind) + "/" + name + " " + t
	}
	return "unknown"
}

// metav1Duration is a YAML friendly duration ("5m", "90s").
type metav1Duration struct{ time.Duration }

func (d *metav1Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d metav1Duration) MarshalJSON() ([]byte, error) {
	return yaml.Marshal(d.Duration.String())
}

// LoadProfile reads profiles/<name>/profile.yaml from the manifest tree.
func LoadProfile(tree fs.FS, name string) (*Profile, error) {
	b, err := fs.ReadFile(tree, path.Join("profiles", name, "profile.yaml"))
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", name, err)
	}
	var p Profile
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return nil, fmt.Errorf("profile %q: %w", name, err)
	}
	if p.Name == "" {
		p.Name = name
	}
	return &p, nil
}

// LoadComponent reads components/<name>/component.yaml.
func LoadComponent(tree fs.FS, name string) (*Component, error) {
	dir := path.Join("components", name)
	b, err := fs.ReadFile(tree, path.Join(dir, "component.yaml"))
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", name, err)
	}
	var c Component
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("component %q: %w", name, err)
	}
	if c.Name == "" {
		c.Name = name
	}
	if c.Helm != nil && c.Helm.Release == "" {
		c.Helm.Release = c.Name
	}
	if c.Timeout.Duration == 0 {
		c.Timeout.Duration = 5 * time.Minute
	}
	c.dir = dir
	return &c, nil
}

// Dir is the component directory inside the manifest tree.
func (c *Component) Dir() string { return c.dir }

// Components resolves every component referenced by the profile, in order.
func (p *Profile) Components(tree fs.FS) ([]*Component, error) {
	var out []*Component
	seen := map[string]bool{}
	for _, rl := range p.Runlevels {
		for _, name := range rl.Components {
			if seen[name] {
				return nil, fmt.Errorf("profile %q: component %q listed twice", p.Name, name)
			}
			seen[name] = true
			c, err := LoadComponent(tree, name)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	return out, nil
}
