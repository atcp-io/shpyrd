// Package sizes defines instance sizes: named CPU/memory allocations a
// process runs with, the equivalent of Fly machine sizes or Render instance
// types. The catalog is cluster-wide (ConfigMap shpyrd-system/shpyrd-sizes),
// seeded with defaults at install time and editable afterwards.
//
// Two kinds exist:
//
//   - shared: the CPU is a guaranteed share that can burst (Kubernetes
//     Burstable QoS: cpu request = size, cpu limit = size x BurstFactor).
//   - dedicated: requests equal limits (Guaranteed QoS), whole cores.
//
// Memory is never overcommitted: request equals limit for both kinds.
package sizes

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// ConfigMap holding the catalog and its key.
const (
	ConfigMapName = "shpyrd-sizes"
	ConfigMapKey  = "sizes.yaml"
)

// Kinds of sizes.
const (
	Shared    = "shared"
	Dedicated = "dedicated"
)

// BurstFactor is how far a shared size may burst above its CPU allocation.
const BurstFactor = 4

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// Size is one catalog entry.
type Size struct {
	Name string `json:"name"`
	// Kind is shared or dedicated.
	Kind string `json:"kind"`
	// CPU in cores, e.g. "0.25", "500m", "2".
	CPU string `json:"cpu"`
	// Memory, e.g. "64Mi", "1Gi".
	Memory string `json:"memory"`
	// Description is free text for the dashboard.
	Description string `json:"description,omitempty"`
}

// Catalog is the cluster's list of sizes and the one used when a process
// does not name any.
type Catalog struct {
	Default string `json:"default"`
	Sizes   []Size `json:"sizes"`
}

// Defaults is the catalog seeded at install time. Memory steps follow Fly
// and Render; the smallest entries suit Go and static sites, JVM and Node
// apps usually need shared-m or larger.
func Defaults() Catalog {
	return Catalog{
		Default: "shared-s",
		Sizes: []Size{
			{Name: "shared-xs", Kind: Shared, CPU: "0.25", Memory: "32Mi", Description: "Tiny: static sites, Go services"},
			{Name: "shared-s", Kind: Shared, CPU: "0.5", Memory: "64Mi", Description: "Default"},
			{Name: "shared-m", Kind: Shared, CPU: "0.5", Memory: "256Mi", Description: "Node.js, Python, Ruby"},
			{Name: "shared-l", Kind: Shared, CPU: "1", Memory: "512Mi", Description: "JVM, heavier web apps"},
			{Name: "shared-xl", Kind: Shared, CPU: "2", Memory: "1Gi"},
			{Name: "dedicated-s", Kind: Dedicated, CPU: "1", Memory: "1Gi", Description: "Guaranteed CPU"},
			{Name: "dedicated-m", Kind: Dedicated, CPU: "2", Memory: "4Gi"},
			{Name: "dedicated-l", Kind: Dedicated, CPU: "4", Memory: "8Gi"},
			{Name: "dedicated-xl", Kind: Dedicated, CPU: "8", Memory: "16Gi"},
			{Name: "dedicated-2xl", Kind: Dedicated, CPU: "16", Memory: "32Gi"},
		},
	}
}

// Parse reads a catalog from its YAML form and validates it.
func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("sizes: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Marshal renders the catalog as YAML for the ConfigMap.
func (c Catalog) Marshal() ([]byte, error) {
	return yaml.Marshal(c)
}

// Validate checks names, kinds, quantities and the default.
func (c Catalog) Validate() error {
	if len(c.Sizes) == 0 {
		return errors.New("sizes: catalog must have at least one size")
	}
	seen := map[string]bool{}
	for _, s := range c.Sizes {
		if err := s.Validate(); err != nil {
			return err
		}
		if seen[s.Name] {
			return fmt.Errorf("sizes: duplicate size %q", s.Name)
		}
		seen[s.Name] = true
	}
	if c.Default == "" {
		return errors.New("sizes: default size must be set")
	}
	if !seen[c.Default] {
		return fmt.Errorf("sizes: default %q is not in the catalog", c.Default)
	}
	return nil
}

// Validate checks one size.
func (s Size) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("sizes: invalid name %q (lowercase letters, digits, dashes)", s.Name)
	}
	if s.Kind != Shared && s.Kind != Dedicated {
		return fmt.Errorf("sizes: %s: kind must be %s or %s", s.Name, Shared, Dedicated)
	}
	cpu, err := resource.ParseQuantity(s.CPU)
	if err != nil || cpu.Sign() <= 0 {
		return fmt.Errorf("sizes: %s: invalid cpu %q", s.Name, s.CPU)
	}
	mem, err := resource.ParseQuantity(s.Memory)
	if err != nil || mem.Sign() <= 0 {
		return fmt.Errorf("sizes: %s: invalid memory %q", s.Name, s.Memory)
	}
	return nil
}

// Get returns a size by name.
func (c Catalog) Get(name string) (Size, bool) {
	for _, s := range c.Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return Size{}, false
}

// Sorted returns the sizes ordered by kind (shared first) then CPU then memory.
func (c Catalog) Sorted() []Size {
	out := append([]Size(nil), c.Sizes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == Shared
		}
		ci, cj := resource.MustParse(out[i].CPU), resource.MustParse(out[j].CPU)
		if ci.Cmp(cj) != 0 {
			return ci.Cmp(cj) < 0
		}
		mi, mj := resource.MustParse(out[i].Memory), resource.MustParse(out[j].Memory)
		return mi.Cmp(mj) < 0
	})
	return out
}

// Upsert adds or replaces a size.
func (c *Catalog) Upsert(s Size) error {
	if err := s.Validate(); err != nil {
		return err
	}
	for i := range c.Sizes {
		if c.Sizes[i].Name == s.Name {
			c.Sizes[i] = s
			return nil
		}
	}
	c.Sizes = append(c.Sizes, s)
	return nil
}

// Remove deletes a size; the default cannot be removed.
func (c *Catalog) Remove(name string) error {
	if name == c.Default {
		return fmt.Errorf("sizes: %q is the default size; pick another default first", name)
	}
	for i := range c.Sizes {
		if c.Sizes[i].Name == name {
			c.Sizes = append(c.Sizes[:i], c.Sizes[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("sizes: %q not found", name)
}

// Resources turns a size into Kubernetes requests and limits.
func (s Size) Resources() corev1.ResourceRequirements {
	cpu := resource.MustParse(s.CPU)
	mem := resource.MustParse(s.Memory)
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: mem},
	}
	if s.Kind == Dedicated {
		out.Limits[corev1.ResourceCPU] = cpu
		return out
	}
	burst := resource.NewMilliQuantity(cpu.MilliValue()*BurstFactor, resource.DecimalSI)
	out.Limits[corev1.ResourceCPU] = *burst
	return out
}

// Resolve picks the resources for a process: an explicit override wins
// (missing halves are filled from the size), then the named size, then the
// catalog default. It returns the size name used ("" when fully explicit).
func (c Catalog) Resolve(sizeName string, override corev1.ResourceRequirements) (corev1.ResourceRequirements, string, error) {
	name := sizeName
	if name == "" {
		name = c.Default
	}
	size, ok := c.Get(name)
	if !ok {
		return corev1.ResourceRequirements{}, "", fmt.Errorf("unknown size %q (see `shpyrd sizes list`)", name)
	}
	base := size.Resources()
	if len(override.Requests) == 0 && len(override.Limits) == 0 {
		return base, size.Name, nil
	}
	// Explicit cpu/memory limits replace the size's; requests follow the
	// size's kind (dedicated: equal to limits; shared: kept from the size or
	// derived when the size has none).
	merged := base
	for k, v := range override.Limits {
		merged.Limits[k] = v
		if size.Kind == Dedicated || k == corev1.ResourceMemory {
			merged.Requests[k] = v
		} else if cur, ok := merged.Requests[k]; ok && cur.Cmp(v) > 0 {
			merged.Requests[k] = v
		}
	}
	for k, v := range override.Requests {
		merged.Requests[k] = v
	}
	return merged, "", nil
}
